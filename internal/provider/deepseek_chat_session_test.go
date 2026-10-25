package provider

import (
	"testing"

	"aurora/internal/provider/session"
)

// ── 会话复用消费者 loop 的纯函数 seam 测试(ticket 02,wire-fake 之外的第三层)──
//
// runChatTurn 的分支逻辑(续轮增量 vs 新会话引导 vs vision 绕行)全部
// 注入接口覆盖,不需要上游网络。消费 seam 的帧序化行为由
// preConsumedStream 预置回放 + wire 级测试(deepseek_chat_wire_test.go)覆盖。

// fakeSessionPool 记录 acquire/discard/release 轨迹,返回预置租约。
type fakeSessionPool struct {
	leases   []*session.Lease // Acquire/AcquireNew 依次弹出
	acquired int
	acqNew   int
	discards int
	tokens   []string // Acquire/AcquireNew 收到的 token(轮换传递断言)
	released []string // release 收到的 responseMessageID
}

func (f *fakeSessionPool) Acquire(clientKey, modelID, token string) (*session.Lease, error) {
	f.acquired++
	f.tokens = append(f.tokens, token)
	if len(f.leases) > 0 {
		l := f.leases[0]
		f.leases = f.leases[1:]
		return l, nil
	}
	return &session.Lease{}, nil
}

func (f *fakeSessionPool) AcquireNew(clientKey, modelID, token string) (*session.Lease, error) {
	f.acqNew++
	return f.Acquire(clientKey, modelID, token)
}

func (f *fakeSessionPool) Discard(l *session.Lease) { f.discards++ }

func (f *fakeSessionPool) Release(l *session.Lease, responseMessageID string) {
	f.released = append(f.released, responseMessageID)
}

// fakeChatSender 记录发入的 CompletionRequest,返回预置流。
type fakeChatSender struct {
	sent  []*deepseekSenderReq
	reply string
	msgID string
}

func (f *fakeChatSender) Send(token string, req deepseekSenderReq) (*preConsumedStream, error) {
	f.sent = append(f.sent, &req)
	return &preConsumedStream{
		result: &deepseekStreamResult{Text: f.reply, ResponseMsgID: f.msgID},
		deltas: []deepseekDelta{{Text: f.reply}},
	}, nil
}

// 租约建立绑定本轮 token:flow 把 token 传给池(spec「session 与建立时 token 绑定」)。
func TestLoopPassesTokenToPool(t *testing.T) {
	pool := &fakeSessionPool{leases: []*session.Lease{{SessionID: "s1"}}}
	sender := &fakeChatSender{reply: "答", msgID: "m1"}
	flow := &deepseekChatFlow{
		pool: pool, sender: sender, clientKey: "u1", token: "tok-a",
		messages: []deepseekTurn{{Text: "问"}},
	}
	flow.runForTest(t)

	if len(pool.tokens) != 1 || pool.tokens[0] != "tok-a" {
		t.Fatalf("acquire 应传入本轮 token, got %v", pool.tokens)
	}
}

// ── 断言用例 ──

// 续轮:pool 给出带 ParentMessageID 的租约 → prompt 只含本轮增量,Release 归还锚点。
func TestLoopResumeSendsIncrementOnly(t *testing.T) {
	pool := &fakeSessionPool{leases: []*session.Lease{{SessionID: "s1", ParentMessageID: "77"}}}
	sender := &fakeChatSender{reply: "答2", msgID: "78"}
	flow := &deepseekChatFlow{
		pool:      pool,
		sender:    sender,
		clientKey: "u1",
		token:     "tok-1",
		messages: []deepseekTurn{
			{Text: "问1"},
			{Text: "问2"},
		},
	}
	out := flow.runForTest(t)

	if len(sender.sent) != 1 {
		t.Fatalf("发送次数 = %d, want 1", len(sender.sent))
	}
	// token 必须透传到 acquire(ticket 05 live 验证暴露的 P0:空 token 建会话必 401)。
	if len(pool.tokens) != 1 || pool.tokens[0] != "tok-1" {
		t.Fatalf("acquire 应收到本轮 token, got %v", pool.tokens)
	}
	req := sender.sent[0]
	if req.ParentMessageID != "77" {
		t.Fatalf("续轮 parent_message_id = %q, want 77", req.ParentMessageID)
	}
	if req.Prompt != "问2" {
		t.Fatalf("续轮 prompt 应只含本轮增量 = %q, want 问2", req.Prompt)
	}
	if len(pool.released) != 1 || pool.released[0] != "78" {
		t.Fatalf("release 锚点 = %v, want [78]", pool.released)
	}
	if pool.discards != 0 {
		t.Fatalf("成功续轮不应 discard")
	}
	if out.text != "答2" {
		t.Fatalf("输出 = %q, want 答2", out.text)
	}
}

// 上游未给续轮锚点(response_message_id 为空)时不得入池:
// 入池会让下轮在同一 session 上全量重发历史——既非续轮(无 parent)也非
// 新会话引导(旧 session),违反 spec §会话策略。故丢弃并删上游。
func TestLoopEmptyAnchorDoesNotPool(t *testing.T) {
	pool := &fakeSessionPool{leases: []*session.Lease{{SessionID: "s1"}}}
	sender := &fakeChatSender{reply: "答1"} // msgID 空 = 上游没给锚点
	flow := &deepseekChatFlow{
		pool:      pool,
		sender:    sender,
		clientKey: "u1",
		messages:  []deepseekTurn{{Text: "问1"}},
	}
	out := flow.runForTest(t)

	if out.text != "答1" {
		t.Fatalf("输出 = %q, want 答1", out.text)
	}
	if len(pool.released) != 0 {
		t.Fatalf("无锚点不应 Release 入池, released=%v", pool.released)
	}
	if pool.discards != 1 {
		t.Fatalf("无锚点应 Discard(下轮走新会话引导), discards=%d", pool.discards)
	}
}

// 首轮/未命中:parent 为空 → prompt 为本轮全部内容(新会话引导)。
func TestLoopFirstTurnBootstrap(t *testing.T) {
	pool := &fakeSessionPool{leases: []*session.Lease{{SessionID: "s1"}}}
	sender := &fakeChatSender{reply: "答1", msgID: "76"}
	flow := &deepseekChatFlow{
		pool:      pool,
		sender:    sender,
		clientKey: "u1",
		messages:  []deepseekTurn{{Text: "问1"}},
	}
	flow.runForTest(t)

	if len(sender.sent) != 1 || sender.sent[0].Prompt != "问1" {
		t.Fatalf("首轮 prompt = %+v", sender.sent)
	}
	if sender.sent[0].ParentMessageID != "" {
		t.Fatalf("首轮 parent 应为空, got %q", sender.sent[0].ParentMessageID)
	}
	if len(pool.released) != 1 {
		t.Fatalf("首轮成功后应 Release 入池")
	}
}

// 无 clientKey:完全不进池,行为与旧路径一致(prompt 全量、Release 不触发)。
func TestLoopNoKeyBypassesPool(t *testing.T) {
	pool := &fakeSessionPool{}
	sender := &fakeChatSender{reply: "答", msgID: "m1"}
	flow := &deepseekChatFlow{
		pool:      pool,
		sender:    sender,
		clientKey: "",
		messages: []deepseekTurn{
			{Text: "问1"},
			{Text: "问2"},
		},
	}
	flow.runForTest(t)

	if pool.acquired != 0 {
		t.Fatalf("无 key 不应 acquire 池")
	}
	if len(sender.sent) != 1 {
		t.Fatalf("发送次数 = %d, want 1", len(sender.sent))
	}
	want := "问1\n\n问2"
	if sender.sent[0].Prompt != want {
		t.Fatalf("无 key prompt = %q, want %q(拍平引导)", sender.sent[0].Prompt, want)
	}
}

// 显式信令:isNew → AcquireNew(作废旧 entry)。
func TestLoopNewSignalAcquireNew(t *testing.T) {
	pool := &fakeSessionPool{leases: []*session.Lease{{SessionID: "s2"}}}
	sender := &fakeChatSender{reply: "答", msgID: "m1"}
	flow := &deepseekChatFlow{
		pool:      pool,
		sender:    sender,
		clientKey: "u1",
		isNew:     true,
		messages:  []deepseekTurn{{Text: "问"}},
	}
	flow.runForTest(t)

	if pool.acqNew != 1 {
		t.Fatalf("信令应走 AcquireNew, got %d", pool.acqNew)
	}
}

// 失败降级:sender 首次报错 → Discard 失败租约 → 重试一次新开成功。
func TestLoopFailureDegradesToNewSession(t *testing.T) {
	pool := &fakeSessionPool{leases: []*session.Lease{
		{SessionID: "s1", ParentMessageID: "77"},
		{SessionID: "s2"},
	}}
	sender := &fakeChatSender{reply: "答2", msgID: "78"}
	failFirst := true
	sender2 := &failOnceSender{inner: sender, failFirst: &failFirst}
	flow := &deepseekChatFlow{
		pool:      pool,
		sender:    sender2,
		clientKey: "u1",
		messages:  []deepseekTurn{{Text: "问1"}, {Text: "问2"}},
	}
	out := flow.runForTest(t)

	if pool.discards != 1 {
		t.Fatalf("失败后应 Discard 一次, got %d", pool.discards)
	}
	// 首败(装饰层拦)+ 重试成功(inner.Send 记)各 1 条。
	if len(sender2.inner.sent) != 2 {
		t.Fatalf("重试应再发一次, sent=%d", len(sender2.inner.sent))
	}
	if sender2.inner.sent[0].ParentMessageID != "77" || sender2.inner.sent[0].Prompt != "问2" {
		t.Fatalf("首败请求应为续轮形态: %+v", sender2.inner.sent[0])
	}
	// 拍平保留历史(新会话引导),断言仅断 parent 空 + 含本轮内容。
	if sender2.inner.sent[1].ParentMessageID != "" || sender2.inner.sent[1].Prompt != "问1\n\n问2" {
		t.Fatalf("重试应新开引导(拍平本轮内容): %+v", sender2.inner.sent[1])
	}
	if out.text != "答2" {
		t.Fatalf("降级后输出 = %q", out.text)
	}
	if len(pool.released) != 1 || pool.released[0] != "78" {
		t.Fatalf("重试成功后应 Release 新锚点, got %v", pool.released)
	}
}

// 连续失败:重试一次仍失败 → 返回错误,不无限重试。
func TestLoopDoubleFailureReturnsError(t *testing.T) {
	pool := &fakeSessionPool{leases: []*session.Lease{{SessionID: "s1"}}}
	sender := &fakeChatSender{}
	sender2 := &errAlwaysSender{inner: sender}
	flow := &deepseekChatFlow{
		pool:      pool,
		sender:    sender2,
		clientKey: "u1",
		messages:  []deepseekTurn{{Text: "问"}},
	}
	err := flow.runForTestErr(t)
	if err == nil {
		t.Fatal("重试一次仍失败应返回 error")
	}
	if len(sender.sent) != 2 {
		t.Fatalf("应恰好发 2 次(首次+重试一次), got %d", len(sender.sent))
	}
	if pool.discards != 2 {
		t.Fatalf("两次失败都应 Discard, got %d", pool.discards)
	}
}

// vision 请求不进池:modelType = vision 时 acquire 不触发(独立会话路径)。
func TestLoopVisionBypassesPool(t *testing.T) {
	pool := &fakeSessionPool{}
	sender := &fakeChatSender{reply: "看到了", msgID: "m1"}
	flow := &deepseekChatFlow{
		pool:       pool,
		sender:     sender,
		clientKey:  "u1",
		vision:     true,
		modelType:  "vision",
		messages:   []deepseekTurn{{Text: "这是什么"}},
		refFileIDs: []string{"f1", "f2"},
	}
	flow.runForTest(t)

	if pool.acquired != 0 {
		t.Fatalf("vision 请求不应进池, acquired=%d", pool.acquired)
	}
	if len(sender.sent) != 1 || sender.sent[0].ModelType != "vision" {
		t.Fatalf("vision 请求形态: %+v", sender.sent)
	}
	// 已上传并 fork 的 vision 文件 id 必须随本轮发出(缺了上游报“发送至识图模式”;
	// ticket 02 接线时曾丢失,此处锁住)。
	if got := sender.sent[0].RefFileIDs; len(got) != 2 || got[0] != "f1" || got[1] != "f2" {
		t.Fatalf("vision ref_file_ids = %v, want [f1 f2]", got)
	}
}

// ── expert 档回退开关(ticket 05)──
//
// live 验证(2026-09-27)结论:DeepSeek expert 续轮正常(thinking 语义与
// 服务端记忆均生效),故开关默认保持复用、仅备而不用。上游若后续变更导致
// expert 续轮异常,置 DEEPSEEK_EXPERT_RESUME=0 即让 expert 档退回每轮新开
// (无池独立会话路径),quick 档不受影响。

// expert 档回退:noResume → 不进池、每轮新开引导(拍平本轮内容)。
func TestLoopExpertResumeDisabledBypassesPool(t *testing.T) {
	pool := &fakeSessionPool{}
	sender := &fakeChatSender{reply: "答", msgID: "m1"}
	flow := &deepseekChatFlow{
		pool:      pool,
		sender:    sender,
		clientKey: "u1",
		noResume:  true,
		modelType: "expert",
		messages: []deepseekTurn{
			{Text: "问1"},
			{Text: "问2"},
		},
	}
	out := flow.runForTest(t)

	if pool.acquired != 0 || pool.acqNew != 0 {
		t.Fatalf("回退档不应进池, acquired=%d acqNew=%d", pool.acquired, pool.acqNew)
	}
	if len(sender.sent) != 1 {
		t.Fatalf("发送次数 = %d, want 1", len(sender.sent))
	}
	if sender.sent[0].ParentMessageID != "" {
		t.Fatalf("回退档 parent 应为空, got %q", sender.sent[0].ParentMessageID)
	}
	if sender.sent[0].Prompt != "问1\n\n问2" {
		t.Fatalf("回退档应为新会话引导(拍平本轮内容), got %q", sender.sent[0].Prompt)
	}
	if sender.sent[0].ModelType != "expert" {
		t.Fatalf("回退只关复用,不改模式: modelType = %q", sender.sent[0].ModelType)
	}
	if out.text != "答" {
		t.Fatalf("输出 = %q", out.text)
	}
}

// 开关默认(noResume=false)时 expert 档照常进池复用——默认行为是复用。
func TestLoopExpertResumeDefaultStillPools(t *testing.T) {
	pool := &fakeSessionPool{leases: []*session.Lease{{SessionID: "s1", ParentMessageID: "77"}}}
	sender := &fakeChatSender{reply: "答2", msgID: "78"}
	flow := &deepseekChatFlow{
		pool:      pool,
		sender:    sender,
		clientKey: "u1",
		modelType: "expert",
		messages:  []deepseekTurn{{Text: "问1"}, {Text: "问2"}},
	}
	flow.runForTest(t)

	if pool.acquired != 1 {
		t.Fatalf("默认应进池, acquired=%d", pool.acquired)
	}
	if sender.sent[0].ParentMessageID != "77" {
		t.Fatalf("默认应续轮, parent = %q", sender.sent[0].ParentMessageID)
	}
}

// ── runStreaming 流式收口用例(spec arch-deepening-phase2 WP1) ──
// 零增量降级重试 / 已吐增量不降级 / 双重失败报错,经 onDelta 断言增量到达。

// scriptedChatSender 按脚本依次返回预置流(Send 层错误用 nil 流 + err 表达)。
type scriptedChatSender struct {
	steps []scriptedStep
	n     int
	sent  int
}

type scriptedStep struct {
	stream *preConsumedStream
	err    error
}

func (s *scriptedChatSender) Send(token string, req deepseekSenderReq) (*preConsumedStream, error) {
	i := s.n
	if i >= len(s.steps) {
		i = len(s.steps) - 1
	}
	s.n++
	s.sent++
	st := s.steps[i]
	if st.err != nil {
		return nil, st.err
	}
	return st.stream, nil
}

func newRunStreamingFlow(pool chatSessionPool, sender chatSender) *deepseekChatFlow {
	return &deepseekChatFlow{
		pool: pool, sender: sender, clientKey: "u1", token: "tok-a",
		messages: []deepseekTurn{{Text: "问"}},
	}
}

// TestRunStreamingZeroDeltaDegradesAndRetries 首轮零增量流级错误 → 丢弃租约换新
// 重试一次 → 次轮成功;onDelta 只收到次轮增量。
func TestRunStreamingZeroDeltaDegradesAndRetries(t *testing.T) {
	pool := &fakeSessionPool{leases: []*session.Lease{{SessionID: "s1"}}}
	sender := &scriptedChatSender{steps: []scriptedStep{
		{stream: &preConsumedStream{result: &deepseekStreamResult{Err: "upstream boom"}}},
		{stream: &preConsumedStream{result: &deepseekStreamResult{Text: "答", ResponseMsgID: "m1"}, deltas: []deepseekDelta{{Text: "答"}}}},
	}}
	flow := newRunStreamingFlow(pool, sender)
	l, err := flow.acquireLease()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	out, emitted, rerr := flow.runStreaming(l, func(d deepseekDelta) {
		if d.Text != "" {
			got = append(got, d.Text)
		}
	})
	if rerr != nil {
		t.Fatalf("runStreaming err: %v", rerr)
	}
	if out.text != "答" || !emitted {
		t.Fatalf("out=%+v emitted=%v, want text=答 emitted=true", out, emitted)
	}
	if len(got) != 1 || got[0] != "答" {
		t.Fatalf("onDelta got %v, want [答](首轮零增量不回调)", got)
	}
	if pool.discards != 1 || pool.acqNew < 1 {
		t.Fatalf("pool discards=%d acqNew=%d, want 1/1+", pool.discards, pool.acqNew)
	}
	if len(pool.released) != 1 || pool.released[0] != "m1" {
		t.Fatalf("released=%v, want [m1]", pool.released)
	}
}

// TestRunStreamingEmittedThenErrKeepsOutput 已吐增量后上游报错:B 项拍板——
// 不降级、不报错,正常归还收尾(onDelta 已收到半截正文)。
func TestRunStreamingEmittedThenErrKeepsOutput(t *testing.T) {
	pool := &fakeSessionPool{leases: []*session.Lease{{SessionID: "s1"}}}
	sender := &scriptedChatSender{steps: []scriptedStep{
		{stream: &preConsumedStream{
			result: &deepseekStreamResult{Err: "late failure", ResponseMsgID: "m2"},
			deltas: []deepseekDelta{{Text: "半截"}},
		}},
	}}
	flow := newRunStreamingFlow(pool, sender)
	l, _ := flow.acquireLease()
	var got []string
	out, emitted, rerr := flow.runStreaming(l, func(d deepseekDelta) {
		if d.Text != "" {
			got = append(got, d.Text)
		}
	})
	if rerr != nil {
		t.Fatalf("emitted-then-err must not error, got %v", rerr)
	}
	if out.text != "半截" || !emitted || len(got) != 1 {
		t.Fatalf("out=%+v emitted=%v deltas=%v", out, emitted, got)
	}
	// 不降级(无 AcquireNew),成功收尾按锚点归还
	if pool.acqNew != 0 {
		t.Fatalf("acqNew=%d, want 0(已吐增量不降级)", pool.acqNew)
	}
	if len(pool.released) != 1 || pool.released[0] != "m2" {
		t.Fatalf("released=%v, want [m2]", pool.released)
	}
}

// TestRunStreamingDoubleFailureReturnsError 两轮零增量失败 → 报错且不发增量。
func TestRunStreamingDoubleFailureReturnsError(t *testing.T) {
	pool := &fakeSessionPool{leases: []*session.Lease{{SessionID: "s1"}, {SessionID: "s2"}}}
	sender := &scriptedChatSender{steps: []scriptedStep{
		{stream: &preConsumedStream{result: &deepseekStreamResult{Err: "e1"}}},
		{stream: &preConsumedStream{result: &deepseekStreamResult{Err: "e2"}}},
	}}
	flow := newRunStreamingFlow(pool, sender)
	l, _ := flow.acquireLease()
	var got int
	_, emitted, rerr := flow.runStreaming(l, func(d deepseekDelta) { got++ })
	if rerr == nil || rerr.Error() != "e2" {
		t.Fatalf("err=%v, want e2", rerr)
	}
	if emitted || got != 0 {
		t.Fatalf("emitted=%v deltas=%d, want false/0", emitted, got)
	}
	if pool.acqNew != 1 || pool.discards != 2 {
		t.Fatalf("acqNew=%d discards=%d, want 1/2", pool.acqNew, pool.discards)
	}
}
