package provider

import (
	"os"
	"strings"
	"testing"
	"time"

	"aurora/internal/deepseekweb"
	"aurora/internal/provider/session"
)

// ── live 验证(ticket 05):上游会话续轮语义 ──
//
// 手工单次运行,**不是 CI 用例**:由 DS_TEST_TOKEN 守卫,缺省自动跳过。
// 测试纪律(AGENTS.md §测试纪律):单发、间隔 ≥2s、单条即止、不复跑不重放。
// 运行:
//
//	DS_TEST_TOKEN=<userToken> go test ./internal/provider/ -run TestLiveDeepSeek -v -count=1
//
// 验证目标:同一上游 session 的第二轮带 parent_message_id = 上轮
// response_message_id、prompt 只含增量时,上游是否(a)记住上轮内容、(b)正常返回。
// quick(default)/expert 各一条。

// countingUpstream 包一层 deepseekUpstream,统计建会话次数(证明续轮复用未新开)
// 与删除次数(证明收尾未悬挂会话)。
type countingUpstream struct {
	inner   deepseekUpstream
	creates int
	deletes int
}

func (c *countingUpstream) CreateSession(token string) (string, error) {
	c.creates++
	return c.inner.CreateSession(token)
}

func (c *countingUpstream) DeleteSession(token, sessionID string) error {
	c.deletes++
	return c.inner.DeleteSession(token, sessionID)
}

// liveResumeProbe 跑两轮:首轮新开(池未命中),次轮续轮(parent = 上轮响应 id)。
func liveResumeProbe(t *testing.T, clientKey, modelType string, thinking bool, first, second string) (turn1, turn2 deepseekTurnOutput, up *countingUpstream) {
	t.Helper()
	tok := strings.TrimSpace(os.Getenv("DS_TEST_TOKEN"))
	if tok == "" {
		t.Skip("DS_TEST_TOKEN not set")
	}
	client, err := deepseekweb.NewClient("", "", "")
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	up = &countingUpstream{inner: deepseekUpstream{client: client}}
	pool := session.NewPool(up, session.PoolConfig{CleanupInterval: -1}) // 无后台清理,避免测试期误删
	sender := &deepseekWebSender{client: client, searchEnabled: false, thinking: thinking}
	defer func() {
		// 手工收尾:删掉池内残留 session(测试即止,不留悬挂会话)。
		if l, err := pool.Acquire(clientKey, modelType, tok); err == nil {
			pool.Discard(l)
		}
	}()

	start := time.Now()
	turn1, err = (&deepseekChatFlow{
		pool: pool, sender: sender, clientKey: clientKey, modelType: modelType,
		token: tok, messages: []deepseekTurn{{Text: first}},
	}).run()
	if err != nil {
		t.Fatalf("turn1: %v", err)
	}
	d1 := time.Since(start)
	t.Logf("[%s] turn1: %q (reasoning %d 字) msgID=%s 耗时=%v", modelType, turn1.text, len([]rune(turn1.reasoning)), turn1.responseMsgID, d1.Round(time.Millisecond))

	if turn1.responseMsgID == "" {
		t.Fatalf("[%s] turn1 未拿到 response_message_id,续轮无锚点", modelType)
	}
	// 真人节奏:两轮间隔 ≥2s。
	time.Sleep(3 * time.Second)

	start = time.Now()
	turn2, err = (&deepseekChatFlow{
		pool: pool, sender: sender, clientKey: clientKey, modelType: modelType,
		token: tok, messages: []deepseekTurn{{Text: second}},
	}).run()
	if err != nil {
		t.Fatalf("turn2 续轮失败: %v", err)
	}
	d2 := time.Since(start)
	t.Logf("[%s] turn2: %q (reasoning %d 字) msgID=%s 耗时=%v", modelType, turn2.text, len([]rune(turn2.reasoning)), turn2.responseMsgID, d2.Round(time.Millisecond))

	if up.creates != 1 {
		t.Errorf("[%s] 续轮应复用同一 session,建会话次数 = %d, want 1", modelType, up.creates)
	}
	if up.deletes != 0 {
		t.Errorf("[%s] 续轮期间不应删除上游 session, deletes = %d", modelType, up.deletes)
	}
	if turn2.responseMsgID == "" || turn2.responseMsgID == turn1.responseMsgID {
		t.Errorf("[%s] 续轮响应 id 异常:%q → %q", modelType, turn1.responseMsgID, turn2.responseMsgID)
	}
	return turn1, turn2, up
}

// quick(default)档 live 续轮:上游侧重放证明服务端记忆生效。
func TestLiveDeepSeekQuickResume(t *testing.T) {
	t1, t2, _ := liveResumeProbe(t, "live-quick", "default", false,
		"请只回答:我最喜欢的数字是 47。",
		"我刚才说的数字是多少?只回答数字。")
	if !strings.Contains(t2.text, "47") {
		t.Errorf("续轮未体现服务端记忆(上轮内容未命中):%q", t2.text)
	}
	if t1.text == "" {
		t.Errorf("首轮无正文")
	}
}

// expert(thinking)档 live 续轮:验证 thinking 语义与续轮是否兼容。
func TestLiveDeepSeekExpertResume(t *testing.T) {
	_, t2, _ := liveResumeProbe(t, "live-expert", "expert", true,
		"请只回答:我最喜欢的数字是 47。",
		"我刚才说的数字是多少?只回答数字。")
	if !strings.Contains(t2.text, "47") {
		t.Errorf("续轮未体现服务端记忆(上轮内容未命中):%q", t2.text)
	}
	if len([]rune(t2.reasoning)) == 0 {
		t.Errorf("expert 档续轮无 thinking 内容,thinking_enabled 语义可疑")
	}
}
