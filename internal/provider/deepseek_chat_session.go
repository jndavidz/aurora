package provider

import (
	"net/http"
	"strings"

	"aurora/internal/deepseekweb"
	"aurora/internal/provider/session"
)

// deepseekChatFlow 是 DeepSeek chat 变体一轮对话的消费者 loop:
//
//	acquire(池命中→续轮 / 未命中→新开引导) → send → consume → release/discard
//
// 发送(prompt 构造、parent_message_id)、消费(增量回调、锚点提取)、
// 降级(失败丢弃重试一次)全部经由两个小接口(chatSessionPool / chatSender),
// 单测注入 fake 覆盖全部分支,无网络。
//
// spec: .scratch/upstream-session-reuse/spec.md(§会话策略/§失败降级)

// deepseekTurn 是拍平前的一轮输入(仅取文本;图片走独立 vision 路径)。
type deepseekTurn struct {
	Text string
}

// chatSessionPool 是 provider 侧需要的会话策略面(session.Pool 的窄视图)。
type chatSessionPool interface {
	Acquire(clientKey, modelID string) (*session.Lease, error)
	AcquireNew(clientKey, modelID string) (*session.Lease, error)
	Release(l *session.Lease, responseMessageID string)
	Discard(l *session.Lease)
}

// deepseekDelta / deepseekStreamResult 是消费层的最小视图(对齐 deepseekweb 形状)。
type deepseekDelta struct {
	Text      string
	Reasoning string
}

type deepseekStreamResult struct {
	Text             string
	Reasoning        string
	ResponseMsgID    string
	RequestMsgID     string
	Finished         bool
	Err              string
	PromptTokens     int
	CompletionTokens int
}

// chatSender 是一轮 completion 的发送通道:发入请求,返回待消费的流。
type chatSender interface {
	Send(token string, req deepseekSenderReq) (*preConsumedStream, error)
}

// deepseekSenderReq 是发送请求的最小视图(生产实现映射到 deepseekweb.CompletionRequest)。
type deepseekSenderReq struct {
	SessionID       string
	ParentMessageID string
	Prompt          string
	ModelType       string
	ThinkingEnabled bool
	SearchEnabled   bool
	RefFileIDs      []string
}

// preConsumedStream 是 chatSender 返回的待消费流:
//   - 测试注入:deltas/result 预置,seamConsume 回放
//   - 生产注入:live 持有真实响应体,seamConsumeLive 逐帧消费(帧序化)
type preConsumedStream struct {
	result *deepseekStreamResult
	deltas []deepseekDelta
	live   *http.Response
	// cleanup 自建会话的删除回调(仅无池路径;消费完成后必调)。
	cleanup func()
}

// seamConsume 消费一条流:
//   - live(生产):deepseekweb.ConsumeStream 逐帧回调,帧序化(delta 到达即可见,
//     流式管线在回调里实时 flush SSE);结束关 body、触发自建会话 cleanup
//   - 预置(测试):deltas 顺序回放后返回预置汇总
func seamConsume(s *preConsumedStream, onDelta func(deepseekDelta)) *deepseekStreamResult {
	if s == nil {
		return &deepseekStreamResult{Err: "empty stream"}
	}
	// live 流:逐帧消费(帧序化,delta 回调期间数据即可见)。
	if s.live != nil {
		res := deepseekweb.ConsumeStream(s.live.Body, func(d deepseekweb.Delta) {
			onDelta(deepseekDelta{Text: d.Text, Reasoning: d.Reasoning})
		})
		s.live.Body.Close()
		if s.cleanup != nil {
			s.cleanup()
		}
		return &deepseekStreamResult{
			Text:             res.Text,
			Reasoning:        res.Reasoning,
			ResponseMsgID:    res.ResponseMsgID,
			RequestMsgID:     res.RequestMsgID,
			Finished:         res.Finished,
			Err:              res.Err,
			PromptTokens:     res.PromptTokens,
			CompletionTokens: res.CompletionTokens,
		}
	}
	for _, d := range s.deltas {
		onDelta(d)
	}
	if s.cleanup != nil {
		s.cleanup()
	}
	return s.result
}

// deepseekChatFlow 承载一轮对话的全部决策输入与输出。
type deepseekChatFlow struct {
	pool      chatSessionPool
	sender    chatSender
	clientKey string // 空 = 不进池
	isNew     bool   // 显式新会话信令
	vision    bool   // 识图请求不进池(spec 拍板)
	modelType string // default / expert / vision
	token     string
	messages  []deepseekTurn
	// instructions 仅新会话引导路径前置(续轮忽略,网页无 system 位)。
	instructions string
}

// deepseekTurnOutput 是一轮的产物(增量回调已实时吐给调用方)。
type deepseekTurnOutput struct {
	text             string
	reasoning        string
	responseMsgID    string
	promptTokens     int
	completionTokens int
}

// runForTest / runForTestErr 是测试入口(run 的薄包装,便于断言)。
func (f *deepseekChatFlow) runForTest(t interface{ Fatalf(string, ...any) }) deepseekTurnOutput {
	out, err := f.run()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return out
}

func (f *deepseekChatFlow) runForTestErr(t interface{ Fatalf(string, ...any) }) error {
	_, err := f.run()
	return err
}

// poolable 报告本轮是否走会话池:有 clientKey 且非 vision(spec 拍板)。
// 流式/非流式两条管线共用同一判定,避免判定条件漂移。
func (f *deepseekChatFlow) poolable() bool { return f.clientKey != "" && !f.vision }

// run 执行一轮:acquire → 构造请求 → send → consume → release/discard。
// 失败降级:首次失败(任意上游错误)→ Discard → 新开重试一次 → 仍失败返回 error。
func (f *deepseekChatFlow) run() (deepseekTurnOutput, error) {
	poolable := f.poolable()

	var l *session.Lease
	if poolable {
		var err error
		if f.isNew {
			l, err = f.pool.AcquireNew(f.clientKey, f.modelID())
		} else {
			l, err = f.pool.Acquire(f.clientKey, f.modelID())
		}
		if err != nil {
			return deepseekTurnOutput{}, err
		}
	}

	for attempt := 0; ; attempt++ {
		req := f.buildRequest(l)
		stream, err := f.sender.Send(f.token, req)
		if err != nil {
			if attempt > 0 || !poolable {
				return deepturnFail(l, f.pool, poolable, err)
			}
			// 失败降级:丢弃失败租约 → 新开重试一次(spec §失败降级)。
			if poolable {
				f.pool.Discard(l)
			}
			l2, cerr := f.pool.AcquireNew(f.clientKey, f.modelID())
			if cerr != nil {
				return deepseekTurnOutput{}, cerr
			}
			l = l2
			continue
		}

		var out deepseekTurnOutput
		res := seamConsume(stream, func(d deepseekDelta) {
			if d.Text != "" {
				out.text += d.Text
			}
			if d.Reasoning != "" {
				out.reasoning += d.Reasoning
			}
		})
		if res != nil {
			out.responseMsgID = res.ResponseMsgID
			out.promptTokens = res.PromptTokens
			out.completionTokens = res.CompletionTokens
			if res.Err != "" && out.text == "" && out.reasoning == "" {
				// 流级失败按发送失败同型处理(降级重试一次)。
				if poolable {
					f.pool.Discard(l)
				}
				if attempt > 0 || !poolable {
					return deepseekTurnOutput{}, flowError(res.Err)
				}
				l2, cerr := f.pool.AcquireNew(f.clientKey, f.modelID())
				if cerr != nil {
					return deepseekTurnOutput{}, cerr
				}
				l = l2
				continue
			}
		}

		if poolable && l != nil {
			f.pool.Release(l, out.responseMsgID)
		}
		return out, nil
	}
}

// flowError 把流级错误包装为 error。
type flowError string

func (e flowError) Error() string { return string(e) }

// deepturnFail 统一最终失败出口:Discard 后透传错误。
func deepturnFail(l *session.Lease, p chatSessionPool, poolable bool, err error) (deepseekTurnOutput, error) {
	if poolable {
		p.Discard(l)
	}
	return deepseekTurnOutput{}, err
}

// modelID 会话键的 model 段:vision 走独立通道,统一用 modelType 语义。
func (f *deepseekChatFlow) modelID() string { return f.modelType }

// buildRequest 构造本轮发送请求:
//   - 池租约带 ParentMessageID → 续轮:prompt 只含本轮增量(messages 末条)
//   - 否则 → 新会话引导:prompt 为本轮全部内容(拍平,instructions 前置)
func (f *deepseekChatFlow) buildRequest(l *session.Lease) deepseekSenderReq {
	resume := l != nil && l.ParentMessageID != ""
	prompt := flattenChatTurns(f.messages, f.instructions)
	if resume && len(f.messages) > 0 {
		prompt = f.messages[len(f.messages)-1].Text
	}
	var parent string
	if l != nil {
		parent = l.ParentMessageID
	}
	return deepseekSenderReq{
		SessionID:       leaseSessionID(l),
		ParentMessageID: parent,
		Prompt:          prompt,
		ModelType:       f.modelType,
	}
}

// leaseSessionID 取租约的 session id(无池请求为空,由 adapter 自建会话)。
func leaseSessionID(l *session.Lease) string {
	if l == nil {
		return ""
	}
	return l.SessionID
}

// flattenChatTurns 把本轮内容拍平为新会话引导 prompt(与 flattenChatItems
// 同型:文本拼接、instructions 前置、空轮跳过)。
func flattenChatTurns(turns []deepseekTurn, instructions string) string {
	var sb strings.Builder
	if instructions != "" {
		sb.WriteString(instructions)
		sb.WriteString("\n\n")
	}
	for _, t := range turns {
		if t.Text == "" {
			continue
		}
		sb.WriteString(t.Text)
		sb.WriteString("\n\n")
	}
	return strings.TrimSpace(sb.String())
}

// 编译期引用 deepseekweb(生产 adapter 落点;防止 import 漂移)。
var _ = deepseekweb.CompletionRequest{}
