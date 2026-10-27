package handler

// provider_tools_test.go —— T03 方案 A(provider 工具模拟)离线测试。
// 用假 TextCollector(canned 文本 + 错误)驱动 handleProviderToolCalling:
// 注入/解析/重试降级/SSE 帧序列全在本地验证,不触真实网络。

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aurora/internal/config"
	"aurora/internal/provider"
	officialtypes "aurora/typings/official"

	"github.com/gin-gonic/gin"
)

// ── 假 TextCollector ─────────────────────────────────────────────

// ptCollector 按调用次数返回 canned 文本/错误,并记录每轮收到的请求
// (断言注入落在副本上、重试轮带 nudge)。
type ptCollector struct {
	replies  []string
	errs     []error
	calls    int
	requests []*officialtypes.APIRequest
}

func (f *ptCollector) CollectText(_ *gin.Context, req *officialtypes.APIRequest) (string, error) {
	f.calls++
	f.requests = append(f.requests, req)
	i := f.calls - 1
	if i < len(f.errs) && f.errs[i] != nil {
		return "", f.errs[i]
	}
	if i < len(f.replies) {
		return f.replies[i], nil
	}
	return "", nil
}

// 编译期断言:假收集器满足 provider 侧接缝。
var _ provider.TextCollector = (*ptCollector)(nil)

// ── 测试脚手架 ───────────────────────────────────────────────────

func ptHandler(retries int) *ChatHandler {
	return &ChatHandler{cfg: &config.Config{ProviderToolEmulation: true, ProviderToolRetries: retries}}
}

func ptContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	return c, w
}

func ptTools() []officialtypes.Tool {
	return []officialtypes.Tool{
		{Type: "function", Function: officialtypes.ToolFunction{Name: "get_weather", Description: "Get the weather"}},
		{Type: "function", Function: officialtypes.ToolFunction{Name: "search_web", Description: "Search the web"}},
	}
}

func ptRequest(stream bool) *officialtypes.APIRequest {
	return &officialtypes.APIRequest{
		Model:  "deepseek",
		Stream: stream,
		Messages: []officialtypes.APIMessage{
			officialtypes.NewTextMessage("system", "You are a helpful assistant."),
			officialtypes.NewTextMessage("user", "What's the weather in Paris?"),
		},
		Tools: ptTools(),
	}
}

func ptMessagesText(msgs []officialtypes.APIMessage) string {
	var parts []string
	for _, m := range msgs {
		parts = append(parts, m.Role+": "+m.Text())
	}
	return strings.Join(parts, "\n")
}

func ptSystemCount(msgs []officialtypes.APIMessage) int {
	n := 0
	for _, m := range msgs {
		if m.Role == "system" {
			n++
		}
	}
	return n
}

func ptChatCompletion(t *testing.T, body []byte) officialtypes.ChatCompletion {
	t.Helper()
	var resp officialtypes.ChatCompletion
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("响应不是合法 chat.completion JSON: %v; body=%s", err, body)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices 数量 = %d, 想要 1; body=%s", len(resp.Choices), body)
	}
	return resp
}

// ptStreamChunks 把 SSE 输出解成 chunk map(跳过 [DONE])。
func ptStreamChunks(t *testing.T, body string) []map[string]any {
	t.Helper()
	lines := sseDataLines(body)
	if len(lines) == 0 {
		t.Fatalf("没有 SSE data 帧; body=%s", body)
	}
	if lines[len(lines)-1] != "[DONE]" {
		t.Fatalf("末帧 = %q, 想要 [DONE]; body=%s", lines[len(lines)-1], body)
	}
	var chunks []map[string]any
	for _, line := range lines[:len(lines)-1] {
		var chunk map[string]any
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			t.Fatalf("SSE 帧不是合法 JSON: %v; line=%s", err, line)
		}
		chunks = append(chunks, chunk)
	}
	return chunks
}

func ptChoice(t *testing.T, chunk map[string]any) map[string]any {
	t.Helper()
	choices, ok := chunk["choices"].([]any)
	if !ok || len(choices) == 0 {
		t.Fatalf("chunk 缺 choices: %#v", chunk)
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		t.Fatalf("choices[0] 类型异常: %#v", choices[0])
	}
	return choice
}

func ptFinishReasons(chunks []map[string]any) []string {
	var out []string
	for _, chunk := range chunks {
		reason, _ := ptChoiceValue(chunk)["finish_reason"].(string)
		if reason != "" {
			out = append(out, reason)
		}
	}
	return out
}

// ptChoiceValue 是 ptChoice 的无 t 版本(供 ptFinishReasons 复用)。
func ptChoiceValue(chunk map[string]any) map[string]any {
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	choice, _ := choices[0].(map[string]any)
	return choice
}

// ── 1. 非流式:文本含 <tool_call> → tool_calls ───────────────────

func TestProviderToolCallingNonStreamParsesToolCall(t *testing.T) {
	h := ptHandler(3)
	c, w := ptContext()
	req := ptRequest(false)
	tc := &ptCollector{replies: []string{
		"Let me check the weather.\n<tool_call>{\"name\": \"get_weather\", \"arguments\": {\"city\": \"Paris\"}}</tool_call>",
	}}

	h.handleProviderToolCalling(c, req, tc)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, 想要 200; body=%s", w.Code, w.Body.String())
	}
	resp := ptChatCompletion(t, w.Body.Bytes())
	if resp.Model != "deepseek" {
		t.Errorf("model = %q, 想要回显 deepseek", resp.Model)
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %#v, 想要 tool_calls", resp.Choices[0].FinishReason)
	}
	calls := resp.Choices[0].Message.ToolCalls
	if len(calls) != 1 {
		t.Fatalf("tool_calls 数量 = %d, 想要 1; body=%s", len(calls), w.Body.String())
	}
	if calls[0].Function.Name != "get_weather" {
		t.Errorf("name = %q, 想要 get_weather", calls[0].Function.Name)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments 不是合法 JSON: %v; args=%q", err, calls[0].Function.Arguments)
	}
	if args["city"] != "Paris" {
		t.Errorf("arguments.city = %#v, 想要 Paris", args["city"])
	}
	if tc.calls != 1 {
		t.Errorf("收集器调用次数 = %d, 想要 1", tc.calls)
	}
}

// ── 2. 非流式:纯文本 → 原样文本 + stop ─────────────────────────

func TestProviderToolCallingNonStreamPlainText(t *testing.T) {
	h := ptHandler(0) // 无重试:纯文本就是最终答案
	c, w := ptContext()
	tc := &ptCollector{replies: []string{"Paris is sunny today."}}

	h.handleProviderToolCalling(c, ptRequest(false), tc)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, 想要 200; body=%s", w.Code, w.Body.String())
	}
	if tc.calls != 1 {
		t.Errorf("收集器调用次数 = %d, 想要 1", tc.calls)
	}
	resp := ptChatCompletion(t, w.Body.Bytes())
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %#v, 想要 stop", resp.Choices[0].FinishReason)
	}
	if resp.Choices[0].Message.Content != "Paris is sunny today." {
		t.Errorf("content = %q, 想要原样文本", resp.Choices[0].Message.Content)
	}
	if len(resp.Choices[0].Message.ToolCalls) != 0 {
		t.Errorf("不应有 tool_calls: %#v", resp.Choices[0].Message.ToolCalls)
	}
}

// ── 3. 第一次无调用、第二次有 → 重试生效,第二轮带 nudge ─────────

func TestProviderToolCallingRetriesWithNudge(t *testing.T) {
	h := ptHandler(2)
	c, w := ptContext()
	req := ptRequest(false)
	tc := &ptCollector{replies: []string{
		"I can answer that directly.",
		"<tool_call>{\"name\": \"get_weather\", \"arguments\": {\"city\": \"Paris\"}}</tool_call>",
	}}

	h.handleProviderToolCalling(c, req, tc)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, 想要 200; body=%s", w.Code, w.Body.String())
	}
	resp := ptChatCompletion(t, w.Body.Bytes())
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %#v, 想要 tool_calls(重试后应命中)", resp.Choices[0].FinishReason)
	}
	if tc.calls != 2 {
		t.Fatalf("收集器调用次数 = %d, 想要 2", tc.calls)
	}

	nudge := providerToolNudge(req.Tools)
	firstPrompt := ptMessagesText(tc.requests[0].Messages)
	secondPrompt := ptMessagesText(tc.requests[1].Messages)
	if strings.Contains(firstPrompt, nudge) {
		t.Errorf("首轮 prompt 不应含 nudge")
	}
	if !strings.Contains(secondPrompt, nudge) {
		t.Errorf("重试轮 prompt 应含 nudge; prompt=%s", secondPrompt)
	}
	if !strings.Contains(nudge, "get_weather") {
		t.Errorf("nudge 应列出可用工具名; nudge=%s", nudge)
	}
	// nudge 必须是一条 user 消息(追加在末尾),而不是挤进 system。
	last := tc.requests[1].Messages[len(tc.requests[1].Messages)-1]
	if last.Role != "user" || last.Text() != nudge {
		t.Errorf("末条消息应为 nudge user 消息,得到 role=%q text=%q", last.Role, last.Text())
	}
}

// ── 4. ProviderToolRetries=0 → 不重试 ───────────────────────────

func TestProviderToolCallingNoRetryWhenBudgetZero(t *testing.T) {
	h := ptHandler(0)
	c, w := ptContext()
	tc := &ptCollector{replies: []string{"plain answer"}}

	h.handleProviderToolCalling(c, ptRequest(false), tc)

	if tc.calls != 1 {
		t.Fatalf("收集器调用次数 = %d, 想要 1(0 = 只跑一次)", tc.calls)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, 想要 200; body=%s", w.Code, w.Body.String())
	}
	resp := ptChatCompletion(t, w.Body.Bytes())
	if resp.Choices[0].Message.Content != "plain answer" {
		t.Errorf("content = %q, 想要 plain answer", resp.Choices[0].Message.Content)
	}
}

// ── 5. 重试耗尽 → 200 + 文本(绝不 5xx)────────────────────────

func TestProviderToolCallingRetriesExhaustedFallsBackToText(t *testing.T) {
	h := ptHandler(2)
	c, w := ptContext()
	tc := &ptCollector{replies: []string{"attempt one", "attempt two", "attempt three"}}

	h.handleProviderToolCalling(c, ptRequest(false), tc)

	if tc.calls != 3 {
		t.Fatalf("收集器调用次数 = %d, 想要 3(2 次额外重试)", tc.calls)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, 想要 200(降级为普通文本); body=%s", w.Code, w.Body.String())
	}
	resp := ptChatCompletion(t, w.Body.Bytes())
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %#v, 想要 stop", resp.Choices[0].FinishReason)
	}
	if resp.Choices[0].Message.Content != "attempt three" {
		t.Errorf("content = %q, 想要最后一轮文本", resp.Choices[0].Message.Content)
	}
}

// ── 6. 流式:两条路径的帧序列 ───────────────────────────────────

func TestProviderToolCallingStreamToolCalls(t *testing.T) {
	h := ptHandler(0)
	c, w := ptContext()
	tc := &ptCollector{replies: []string{
		"<tool_call>{\"name\": \"get_weather\", \"arguments\": {\"city\": \"Paris\"}}</tool_call>",
	}}

	h.handleProviderToolCalling(c, ptRequest(true), tc)

	chunks := ptStreamChunks(t, w.Body.String())
	if len(chunks) < 3 {
		t.Fatalf("帧数 = %d, 太少: %s", len(chunks), w.Body.String())
	}
	// 首帧:role=assistant
	delta, _ := ptChoice(t, chunks[0])["delta"].(map[string]any)
	if delta["role"] != "assistant" {
		t.Errorf("首帧 role = %#v, 想要 assistant", delta["role"])
	}
	// tool_calls delta:name 与 arguments 都要出现
	var name, arguments string
	for _, chunk := range chunks {
		d, _ := ptChoice(t, chunk)["delta"].(map[string]any)
		rawCalls, _ := d["tool_calls"].([]any)
		for _, raw := range rawCalls {
			call, _ := raw.(map[string]any)
			fn, _ := call["function"].(map[string]any)
			if s, ok := fn["name"].(string); ok && s != "" {
				name = s
			}
			if s, ok := fn["arguments"].(string); ok {
				arguments += s
			}
		}
	}
	if name != "get_weather" {
		t.Errorf("流式 name = %q, 想要 get_weather", name)
	}
	var streamArgs map[string]any
	if err := json.Unmarshal([]byte(arguments), &streamArgs); err != nil {
		t.Fatalf("流式 arguments 不是合法 JSON: %v; raw=%q", err, arguments)
	}
	if streamArgs["city"] != "Paris" {
		t.Errorf("流式 arguments.city = %#v, 想要 Paris", streamArgs["city"])
	}
	// 不应有正文帧(有 calls 时正文被丢弃)
	for _, chunk := range chunks {
		d, _ := ptChoice(t, chunk)["delta"].(map[string]any)
		if s, _ := d["content"].(string); s != "" {
			t.Errorf("工具调用流不应有 content 帧: %q", s)
		}
	}
	reasons := ptFinishReasons(chunks)
	if len(reasons) != 1 || reasons[0] != "tool_calls" {
		t.Errorf("finish_reason 序列 = %#v, 想要唯一 tool_calls", reasons)
	}
	if strings.Contains(w.Body.String(), "data: [DONE]\n\n") == false {
		t.Errorf("流末缺少 [DONE]: %s", w.Body.String())
	}
}

func TestProviderToolCallingStreamPlainText(t *testing.T) {
	h := ptHandler(0)
	c, w := ptContext()
	tc := &ptCollector{replies: []string{"Hello from the stream."}}

	h.handleProviderToolCalling(c, ptRequest(true), tc)

	chunks := ptStreamChunks(t, w.Body.String())
	var content string
	for _, chunk := range chunks {
		d, _ := ptChoice(t, chunk)["delta"].(map[string]any)
		if s, _ := d["content"].(string); s != "" {
			content += s
		}
	}
	if content != "Hello from the stream." {
		t.Errorf("流式正文 = %q", content)
	}
	reasons := ptFinishReasons(chunks)
	if len(reasons) != 1 || reasons[0] != "stop" {
		t.Errorf("finish_reason 序列 = %#v, 想要唯一 stop", reasons)
	}
}

// ── 7. 多工具返回 → index 为 0,1,… ─────────────────────────────

func TestProviderToolCallingMultipleToolCallsIndexed(t *testing.T) {
	h := ptHandler(0)
	c, w := ptContext()
	tc := &ptCollector{replies: []string{
		`<tool_call>{"name": "get_weather", "arguments": {"city": "Paris"}}</tool_call>` +
			`<tool_call>{"name": "search_web", "arguments": {"q": "Paris weather"}}</tool_call>`,
	}}

	h.handleProviderToolCalling(c, ptRequest(false), tc)

	resp := ptChatCompletion(t, w.Body.Bytes())
	calls := resp.Choices[0].Message.ToolCalls
	if len(calls) != 2 {
		t.Fatalf("tool_calls 数量 = %d, 想要 2; body=%s", len(calls), w.Body.String())
	}
	wantNames := []string{"get_weather", "search_web"}
	for i, want := range wantNames {
		if calls[i].Function.Name != want {
			t.Errorf("calls[%d].name = %q, 想要 %q", i, calls[i].Function.Name, want)
		}
		if calls[i].Index != i {
			t.Errorf("calls[%d].index = %d, 想要 %d", i, calls[i].Index, i)
		}
	}
}

// ── 8. 门禁 helper ──────────────────────────────────────────────

func TestProviderToolEmulationEnabled(t *testing.T) {
	cases := []struct {
		name  string
		cfg   *config.Config
		model string
		want  bool
	}{
		{"nil cfg", nil, "deepseek", false},
		{"总开关关", &config.Config{ProviderToolEmulation: false, ProviderToolModels: []string{"deepseek"}}, "deepseek", false},
		{"开关开+空白名单", &config.Config{ProviderToolEmulation: true}, "deepseek", true},
		{"白名单命中", &config.Config{ProviderToolEmulation: true, ProviderToolModels: []string{"deepseek", "glm"}}, "glm", true},
		{"白名单未命中", &config.Config{ProviderToolEmulation: true, ProviderToolModels: []string{"deepseek"}}, "kimi", false},
		{"空白项忽略", &config.Config{ProviderToolEmulation: true, ProviderToolModels: []string{" ", "deepseek", ""}}, "deepseek", true},
		{"全空白名单视为不限", &config.Config{ProviderToolEmulation: true, ProviderToolModels: []string{"", "  "}}, "kimi", true},
		{"精确匹配不做前缀", &config.Config{ProviderToolEmulation: true, ProviderToolModels: []string{"deepseek"}}, "deepseek-chat", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := providerToolEmulationEnabled(tc.cfg, tc.model); got != tc.want {
				t.Errorf("providerToolEmulationEnabled(%v, %q) = %v, 想要 %v", tc.cfg, tc.model, got, tc.want)
			}
		})
	}
}

// ── 9. 注入不污染入参 ───────────────────────────────────────────

func TestProviderToolCallingInjectionDoesNotMutateRequest(t *testing.T) {
	h := ptHandler(0)
	c, _ := ptContext()
	req := ptRequest(false)
	origLen := len(req.Messages)
	origSystem := req.Messages[0].Text()
	origUser := req.Messages[1].Text()

	tc := &ptCollector{replies: []string{"plain answer"}}
	h.handleProviderToolCalling(c, req, tc)

	if len(req.Messages) != origLen {
		t.Fatalf("原请求 messages 长度被改: %d → %d", origLen, len(req.Messages))
	}
	if req.Messages[0].Text() != origSystem || req.Messages[1].Text() != origUser {
		t.Errorf("原请求消息内容被改: %#v", req.Messages)
	}

	injected := tc.requests[0]
	if got := ptSystemCount(injected.Messages); got != 1 {
		t.Errorf("注入后 system 消息数 = %d, 想要 1(并入单条)", got)
	}
	sys := injected.Messages[0]
	if sys.Role != "system" {
		t.Fatalf("首条消息 role = %q, 想要 system", sys.Role)
	}
	if !strings.Contains(sys.Text(), origSystem) {
		t.Errorf("system 应保留原文; got=%q", sys.Text())
	}
	if !strings.Contains(sys.Text(), "<tool_calling_protocol>") || !strings.Contains(sys.Text(), "</tool_calling_protocol>") {
		t.Errorf("system 应含 <tool_calling_protocol> 块; got=%q", sys.Text())
	}
	if !strings.Contains(sys.Text(), "get_weather") {
		t.Errorf("协议块应列出工具名; got=%q", sys.Text())
	}
}

func TestProviderToolCallingInjectionAddsSystemWhenMissing(t *testing.T) {
	h := ptHandler(0)
	c, _ := ptContext()
	req := &officialtypes.APIRequest{
		Model:    "deepseek",
		Messages: []officialtypes.APIMessage{officialtypes.NewTextMessage("user", "hi")},
		Tools:    ptTools(),
	}
	tc := &ptCollector{replies: []string{"ok"}}

	h.handleProviderToolCalling(c, req, tc)

	if len(req.Messages) != 1 {
		t.Fatalf("原请求被改: %#v", req.Messages)
	}
	injected := tc.requests[0].Messages
	if len(injected) != 2 {
		t.Fatalf("注入后消息数 = %d, 想要 2", len(injected))
	}
	if injected[0].Role != "system" || !strings.Contains(injected[0].Text(), "<tool_calling_protocol>") {
		t.Errorf("应在最前插入 system 协议块; got=%#v", injected[0])
	}
}

// ── 10. CollectText 返回错误 → 502 + 错误文本 ───────────────────

func TestProviderToolCallingCollectErrorReturns502(t *testing.T) {
	boom := errors.New("upstream exploded: token pool empty")

	for _, stream := range []bool{false, true} {
		name := "非流式"
		if stream {
			name = "流式"
		}
		t.Run(name, func(t *testing.T) {
			h := ptHandler(3)
			c, w := ptContext()
			tc := &ptCollector{errs: []error{boom}}

			h.handleProviderToolCalling(c, ptRequest(stream), tc)

			if w.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, 想要 502; body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), boom.Error()) {
				t.Errorf("body 应含错误文本; body=%s", w.Body.String())
			}
			if tc.calls != 1 {
				t.Errorf("上游错误不应重试; 调用次数 = %d", tc.calls)
			}
		})
	}
}
