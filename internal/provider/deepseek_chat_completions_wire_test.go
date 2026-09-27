package provider

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aurora/internal/config"
	"aurora/typings/official"

	"github.com/gin-gonic/gin"
)

// ── chat.completions 表面接入会话复用(ticket 04)wire 级回环 ──
//
// 真 DeepSeek provider + 真 session.Pool + httptest 假上游:走公开入口
// ChatCompletions(c, req) → chatCompletions,验证 user 字段/头信令的接线。
// 断言口径与 Responses 表面(deepseek_chat_wire_test.go)一致。

// newWireProvider 构造指向假上游的 DeepSeek,并注入一个 token(NextToken 非空)。
func newWireProvider(t *testing.T, base string) *DeepSeek {
	t.Helper()
	tokFile := filepath.Join(t.TempDir(), "deepseek_tokens.txt")
	if err := os.WriteFile(tokFile, []byte("tok\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	return NewDeepSeek(&config.Config{
		DeepSeekWebBase:   base,
		DeepSeekWebTokens: tokFile,
		DeepSeekModels:    []string{"deepseek"},
	})
}

func startChatWire(t *testing.T, w *wireUpstream) (*DeepSeek, func()) {
	t.Helper()
	srv := httptest.NewServer(w.handler(t))
	return newWireProvider(t, srv.URL), srv.Close
}

// chatCompletionCall 走 /v1/chat/completions 公开入口一轮。
func chatCompletionCall(t *testing.T, d *DeepSeek, headers map[string]string, req official.APIRequest) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(nil))
	for k, v := range headers {
		c.Request.Header.Set(k, v)
	}
	d.ChatCompletions(c, &req)
	return rec
}

// apiChatReq 构造 chat.completions 请求(文本轮,可带 user)。
func apiChatReq(user string, texts ...string) official.APIRequest {
	msgs := make([]official.APIMessage, 0, len(texts))
	for _, s := range texts {
		msgs = append(msgs, official.APIMessage{Role: "user", Content: official.MessageContent{TextValue: s}})
	}
	return official.APIRequest{Model: "deepseek", User: user, Messages: msgs}
}

// user 字段作 clientKey:连续两轮同 user 复用同一上游 session,第二轮只发增量。
func TestChatCompletionsUserFieldResumesSession(t *testing.T) {
	w := &wireUpstream{}
	d, closer := startChatWire(t, w)
	defer closer()

	chatCompletionCall(t, d, nil, apiChatReq("u1", "你好"))
	chatCompletionCall(t, d, nil, apiChatReq("u1", "你好", "换个话题"))

	if w.creates != 1 {
		t.Fatalf("同 user 两轮应复用 session,creates=%d", w.creates)
	}
	if w.deletes != 0 {
		t.Fatalf("复用期间不应删 session,deletes=%d", w.deletes)
	}
	if len(w.completions) != 2 {
		t.Fatalf("completions=%d, want 2", len(w.completions))
	}
	if w.completions[0].ParentMessageID != nil {
		t.Fatalf("首轮 parent 应为 null, got %v", w.completions[0].ParentMessageID)
	}
	if w.completions[1].ParentMessageID == nil {
		t.Fatal("第二轮 parent_message_id 应非空(续轮锚点)")
	}
	if got := w.completions[1].Prompt; got != "换个话题" {
		t.Fatalf("第二轮 prompt 应只含本轮增量, got %q", got)
	}
	if w.completions[0].SessionID != w.completions[1].SessionID {
		t.Fatalf("两轮应同 session: %q vs %q", w.completions[0].SessionID, w.completions[1].SessionID)
	}
}

// user 字段 #new# 前缀:剥前缀后作 key,强制新会话并作废池内旧 entry。
func TestChatCompletionsNewPrefixForcesNewSession(t *testing.T) {
	w := &wireUpstream{}
	d, closer := startChatWire(t, w)
	defer closer()

	chatCompletionCall(t, d, nil, apiChatReq("u1", "你好"))
	chatCompletionCall(t, d, nil, apiChatReq("#new#u1", "换个话题"))

	if w.creates != 2 {
		t.Fatalf("#new# 应强制新开 session,creates=%d", w.creates)
	}
	if w.deletes != 1 {
		t.Fatalf("池内旧 entry 应被作废(顺手删上游),deletes=%d", w.deletes)
	}
	if w.completions[1].ParentMessageID != nil {
		t.Fatalf("新会话引导 parent 应为 null, got %v", w.completions[1].ParentMessageID)
	}
	if got := w.completions[1].Prompt; got != "换个话题" {
		t.Fatalf("新会话引导 prompt 应为本轮内容, got %q", got)
	}
}

// X-Session-Key 头在同一表面生效;X-Session-Action: new 强制新会话。
func TestChatCompletionsSessionHeaders(t *testing.T) {
	w := &wireUpstream{}
	d, closer := startChatWire(t, w)
	defer closer()

	key := map[string]string{"X-Session-Key": "h1"}
	chatCompletionCall(t, d, key, apiChatReq("", "你好"))
	chatCompletionCall(t, d, key, apiChatReq("", "你好", "继续"))

	if w.creates != 1 {
		t.Fatalf("X-Session-Key 两轮应复用,creates=%d", w.creates)
	}
	if w.completions[1].ParentMessageID == nil {
		t.Fatal("X-Session-Key 续轮 parent 应非空")
	}

	// X-Session-Action: new 与 key 并存 → 强制新开并作废旧 entry。
	newAction := map[string]string{"X-Session-Key": "h1", "X-Session-Action": "new"}
	chatCompletionCall(t, d, newAction, apiChatReq("", "换话题"))

	if w.creates != 2 {
		t.Fatalf("X-Session-Action: new 应新开,creates=%d", w.creates)
	}
	if w.deletes != 1 {
		t.Fatalf("旧 entry 应作废,deletes=%d", w.deletes)
	}
	if w.completions[2].ParentMessageID != nil {
		t.Fatalf("信令轮 parent 应为 null, got %v", w.completions[2].ParentMessageID)
	}
}

// 无 X-Session-Key 头且 user 为空:完全不进池,每次独立处理(一次性调用与连续对话互不干扰)。
func TestChatCompletionsNoKeyBypassesPool(t *testing.T) {
	w := &wireUpstream{}
	d, closer := startChatWire(t, w)
	defer closer()

	chatCompletionCall(t, d, nil, apiChatReq("", "你好"))
	chatCompletionCall(t, d, nil, apiChatReq("", "问1", "问2"))

	if w.creates != 2 {
		t.Fatalf("无 key 应每轮独立自建 session,creates=%d", w.creates)
	}
	if w.deletes != 2 {
		t.Fatalf("无 key 独立会话路径应逐轮删除,deletes=%d", w.deletes)
	}
	for i, comp := range w.completions {
		if comp.ParentMessageID != nil {
			t.Fatalf("无 key 第 %d 轮不应有续轮锚点, got %v", i+1, comp.ParentMessageID)
		}
	}
	// 无 key → 新会话引导:prompt 为本轮全部内容(拍平),不做增量裁剪。
	if got := w.completions[1].Prompt; got != "问1\n\n问2" {
		t.Fatalf("无 key prompt 应拍平本轮内容, got %q", got)
	}
}

// 流式路径同样复用:同 user 两轮流式,第二轮 parent 非空且响应为 chat.completion.chunk。
func TestChatCompletionsStreamResumes(t *testing.T) {
	w := &wireUpstream{}
	d, closer := startChatWire(t, w)
	defer closer()

	req1 := apiChatReq("u1", "你好")
	req1.Stream = true
	req2 := apiChatReq("u1", "你好", "继续")
	req2.Stream = true

	rec1 := chatCompletionCall(t, d, nil, req1)
	rec2 := chatCompletionCall(t, d, nil, req2)

	body1 := rec1.Body.String()
	if !strings.Contains(body1, "chat.completion.chunk") {
		t.Fatalf("流式响应应为 chat.completion.chunk:\n%s", body1)
	}
	if !strings.Contains(body1, "回复1") {
		t.Fatalf("流式响应缺正文:\n%s", body1)
	}
	if !strings.Contains(rec2.Body.String(), "[DONE]") {
		t.Fatalf("流式响应应以 [DONE] 收尾:\n%s", rec2.Body.String())
	}
	if w.creates != 1 {
		t.Fatalf("流式两轮应复用 session,creates=%d", w.creates)
	}
	if w.completions[1].ParentMessageID == nil {
		t.Fatal("流式第二轮 parent 应非空")
	}
}

// 两表面统一:X-Session-Action: new 在 Responses 表面同样强制新会话。
func TestResponsesActionHeaderForcesNewSession(t *testing.T) {
	w := &wireUpstream{}
	d, closer := startChatWire(t, w)
	defer closer()

	key := map[string]string{"X-Session-Key": "h1"}
	d.Responses(ginTestContext(t, key), responsesReq("你好"))
	d.Responses(ginTestContext(t, key), responsesReq("你好", "继续"))
	if w.creates != 1 {
		t.Fatalf("Responses 表面 X-Session-Key 两轮应复用,creates=%d", w.creates)
	}

	newAction := map[string]string{"X-Session-Key": "h1", "X-Session-Action": "new"}
	d.Responses(ginTestContext(t, newAction), responsesReq("换话题"))
	if w.creates != 2 {
		t.Fatalf("Responses 表面 X-Session-Action: new 应新开,creates=%d", w.creates)
	}
	if w.deletes != 1 {
		t.Fatalf("Responses 表面旧 entry 应作废,deletes=%d", w.deletes)
	}
}

// ginTestContext 构造一个带指定请求头的 Responses 表面 gin 上下文。
func ginTestContext(t *testing.T, headers map[string]string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))
	for k, v := range headers {
		c.Request.Header.Set(k, v)
	}
	return c
}

// 无 key 的流式失败:尚未写出任何内容 → 保留旧路径的干净 JSON 502
// (不退化为半截 SSE 流)。
func TestChatCompletionsStreamFailureBeforeStartIsJSON(t *testing.T) {
	w := &wireUpstream{failCompletions: map[int]bool{0: true}}
	d, closer := startChatWire(t, w)
	defer closer()

	req := apiChatReq("", "你好")
	req.Stream = true
	rec := chatCompletionCall(t, d, nil, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("尚未开流的失败应为 502, got %d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "chat.completion.chunk") {
		t.Fatalf("不应写出任何 SSE 块:\n%s", rec.Body.String())
	}
}

// 池路径流式首战失败 → 静默新开重试成功:客户端只见一条完整正常流(无 error 帧、
// 无重复 role 块)。
func TestChatCompletionsStreamRetryIsTransparent(t *testing.T) {
	w := &wireUpstream{failCompletions: map[int]bool{0: true}}
	d, closer := startChatWire(t, w)
	defer closer()

	req := apiChatReq("u1", "你好")
	req.Stream = true
	rec := chatCompletionCall(t, d, nil, req)
	body := rec.Body.String()

	if strings.Contains(body, "\"error\"") {
		t.Fatalf("降级重试对客户端应透明,却出现 error 帧:\n%s", body)
	}
	if !strings.Contains(body, "回复2") {
		t.Fatalf("重试响应正文缺失:\n%s", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("流应以 [DONE] 收尾:\n%s", body)
	}
	if n := strings.Count(body, "\"role\":\"assistant\""); n != 1 {
		t.Fatalf("role 块应恰好出现 1 次(失败重试不重发), got %d:\n%s", n, body)
	}
	if w.creates != 2 {
		t.Fatalf("首战失败应 Discard 后新开一次, creates=%d", w.creates)
	}
}

// A 项拍板(2026-09-28)回归:多 token 池下同 clientKey 连续两轮必须复用
// session(session 与建立时的 token 绑定;轮询 NextToken 会让池命中恒为 0)。
// 修复前(TokFor 之前):第二次 completions 走 NextToken 取到不同 token,
// creates=2 且 deletes=1 —— 本测试即红;修复后(TokenFor)全绿。
func TestChatCompletionsMultiTokenPoolStillResumes(t *testing.T) {
	w := &wireUpstream{}
	d, closer := startChatWireWithTokens(t, w, "tok-A\ntok-B\n")
	defer closer()

	chatCompletionCall(t, d, nil, apiChatReq("u1", "你好"))
	chatCompletionCall(t, d, nil, apiChatReq("u1", "还在吗"))

	if w.creates != 1 {
		t.Fatalf("多 token 池同 key 两轮应复用 session, creates=%d", w.creates)
	}
	if w.deletes != 0 {
		t.Fatalf("复用期间不应删 session, deletes=%d", w.deletes)
	}
}

// 硬规则回归:chat.completions 带 tools 时,prompt 不得注入任何工具信息
// (chat 变体剥离 tools;ticket 04 的接线改走 flow 后仍需保持)。
func TestChatCompletionsStripsTools(t *testing.T) {
	w := &wireUpstream{}
	d, closer := startChatWire(t, w)
	defer closer()

	req := apiChatReq("u1", "读一下")
	req.Tools = []official.Tool{{Type: "function", Function: official.ToolFunction{Name: "read"}}}
	req.Messages = append(req.Messages,
		official.APIMessage{Role: "assistant", ToolCalls: []official.ToolCallRef{{Index: 0, ID: "c1", Type: "function", Function: official.ToolCallFunc{Name: "read", Arguments: `{"path":"a.txt"}`}}}},
		official.APIMessage{Role: "tool", ToolCallID: "c1", Content: official.MessageContent{TextValue: "文件内容"}},
	)

	chatCompletionCall(t, d, nil, req)

	if len(w.completions) != 1 {
		t.Fatalf("completions=%d, want 1", len(w.completions))
	}
	prompt := w.completions[0].Prompt
	for _, banned := range []string{"read", "a.txt", "文件内容", "tool_call"} {
		if strings.Contains(prompt, banned) {
			t.Fatalf("chat prompt 不得含工具信息 %q: %q", banned, prompt)
		}
	}
	if !strings.Contains(prompt, "读一下") {
		t.Fatalf("chat prompt 丢失正文: %q", prompt)
	}
}

// startChatWireWithTokens 同 startChatWire,但注入多行 token 文件。
func startChatWireWithTokens(t *testing.T, w *wireUpstream, tokens string) (*DeepSeek, func()) {
	t.Helper()
	srv := httptest.NewServer(w.handler(t))
	tokFile := filepath.Join(t.TempDir(), "deepseek_tokens.txt")
	if err := os.WriteFile(tokFile, []byte(tokens), 0o600); err != nil {
		srv.Close()
		t.Fatalf("write token file: %v", err)
	}
	return NewDeepSeek(&config.Config{
		DeepSeekWebBase:   srv.URL,
		DeepSeekWebTokens: tokFile,
		DeepSeekModels:    []string{"deepseek"},
	}), srv.Close
}
