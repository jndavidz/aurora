package provider

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"aurora/internal/provider/session"
	"aurora/typings/official"
)

// ── 流式降级路径的客户端形态(ticket 03 验收 4)──
//
// 降级重试必须对客户端透明:重试成功时 SSE 事件序列与正常路径同型
// (不出现 response.failed);重试仍失败才报 response.failed——
// 且任何分支都不得 panic 或退回 JSON 错误(SSE 头已发出)。

// streamTurnBody 跑一轮流式并返回原始 SSE 文本(gin test context)。
func streamTurnBody(t *testing.T, clientKey string, pool chatSessionPool, sender chatSender, req *official.ResponsesAPIRequest) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	d := &DeepSeek{}
	flow := &deepseekChatFlow{
		pool:      pool,
		sender:    sender,
		clientKey: clientKey,
		modelType: "default",
		token:     "tok",
		messages:  []deepseekTurn{{Text: "问"}},
	}
	d.chatStreamTurn(c, &deepseekModel{ID: "deepseek", Variant: variantChat, Mode: modeQuick}, req, flow)
	return w.Body.String()
}

// 续轮首战失败 → 静默新开重试成功:客户端只见一条完整正常流。
func TestStreamDegradeRetryIsTransparent(t *testing.T) {
	pool := &fakeSessionPool{leases: []*session.Lease{
		{SessionID: "s1", ParentMessageID: "77"},
		{SessionID: "s2"},
	}}
	failFirst := true
	sender := &fakeChatSender{reply: "答", msgID: "78"}
	body := streamTurnBody(t, "u1", pool, &failOnceSender{inner: sender, failFirst: &failFirst}, responsesReq("问"))

	if strings.Contains(body, "response.failed") {
		t.Fatalf("降级重试对客户端应透明,却出现 response.failed:\n%s", body)
	}
	if !strings.Contains(body, "response.completed") {
		t.Fatalf("重试成功应有 response.completed:\n%s", body)
	}
	if !strings.Contains(body, "答") {
		t.Fatalf("重试响应正文缺失:\n%s", body)
	}
	if pool.discards != 1 {
		t.Fatalf("首战失败应 Discard 一次, got %d", pool.discards)
	}
	if len(pool.released) != 1 || pool.released[0] != "78" {
		t.Fatalf("重试成功后应 Release 新锚点, got %v", pool.released)
	}
}

// 连续失败:恰好重试一次后向客户端报 response.failed(不无限重试、不 panic)。
func TestStreamDoubleFailureEmitsFailedEvent(t *testing.T) {
	pool := &fakeSessionPool{leases: []*session.Lease{{SessionID: "s1"}, {SessionID: "s2"}}}
	sender := &fakeChatSender{}
	body := streamTurnBody(t, "u1", pool, &errAlwaysSender{inner: sender}, responsesReq("问"))

	if !strings.Contains(body, "response.failed") {
		t.Fatalf("重试仍失败应报 response.failed:\n%s", body)
	}
	if strings.Contains(body, "response.completed") {
		t.Fatalf("失败流不应出现 response.completed:\n%s", body)
	}
	if len(sender.sent) != 2 {
		t.Fatalf("应恰好发 2 次(首次+重试一次), got %d", len(sender.sent))
	}
	if pool.discards != 2 {
		t.Fatalf("两次失败都应 Discard, got %d", pool.discards)
	}
}

// 无 clientKey(不进池)的流式失败:同样只报 response.failed,不退化为 JSON 500。
func TestStreamNoPoolFailureEmitsFailedEvent(t *testing.T) {
	pool := &fakeSessionPool{}
	sender := &fakeChatSender{}
	body := streamTurnBody(t, "", pool, &errAlwaysSender{inner: sender}, responsesReq("问"))

	if !strings.Contains(body, "response.failed") {
		t.Fatalf("无池流式失败应报 response.failed:\n%s", body)
	}
	if pool.acquired != 0 {
		t.Fatalf("无 key 不应进池, acquired=%d", pool.acquired)
	}
	if len(sender.sent) != 1 {
		t.Fatalf("无池不降级重试,应只发 1 次, got %d", len(sender.sent))
	}
}
