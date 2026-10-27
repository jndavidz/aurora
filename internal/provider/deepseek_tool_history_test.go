package provider

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"aurora/typings/official"

	"github.com/gin-gonic/gin"
)

// ── T02:工具历史翻译(deepseekTurnsFromItems)+ CollectText ──
//
// 格式契约与 ChatGPT 通道一致:assistant 的 tool_calls 回灌为
// `<tool_call>{"name": ..., "arguments": ...}</tool_call>`,工具结果回灌为
// `Tool (Resultado da ferramenta <name>): <content>`(toolcall 系统提示以
// "Tool (...)" 块引用工具结果,换措辞会削弱遵循率)。

// toolCallRefForTest 构造一条历史 tool_call(匿名结构体只能逐字段赋值)。
func toolCallRefForTest(id, name, args string) official.ToolCallRef {
	ref := official.ToolCallRef{Index: 0, ID: id, Type: "function"}
	ref.Function.Name = name
	ref.Function.Arguments = args
	return ref
}

// 表面一:Responses 形态的 function_call item(Text 为复刻围栏 JSON)。
func responsesFunctionCallItem(name, args, callID string) responsesInputItem {
	return parseInputItem(rawInputItem{Type: "function_call", Name: name, Arguments: args, CallID: callID})
}

// TestDeepSeekTurnsTranslatesFunctionCall 断言 assistant 历史调用被翻成
// 精确的 <tool_call> 文本(与 toolcall.SerializeForHistory 输出一致)。
func TestDeepSeekTurnsTranslatesFunctionCall(t *testing.T) {
	items := []responsesInputItem{
		{Type: "message", Role: "user", Text: "读一下 a.txt"},
		responsesFunctionCallItem("read", `{"path":"a.txt"}`, "call_1"),
	}
	turns := deepseekTurnsFromItems(items)
	if len(turns) != 2 {
		t.Fatalf("turns=%d, want 2: %+v", len(turns), turns)
	}
	want := `<tool_call>{"name": "read", "arguments": {"path":"a.txt"}}</tool_call>`
	if turns[1].Text != want {
		t.Fatalf("function_call 翻译不一致:\n got %q\nwant %q", turns[1].Text, want)
	}
	if turns[0].Text != "读一下 a.txt" {
		t.Fatalf("普通文本应原样保留, got %q", turns[0].Text)
	}
}

// TestDeepSeekTurnsTranslatesFunctionCallOutputWithName 断言工具结果被翻成
// 精确的 Tool (...) 文本(item 自带 name 优先)。
func TestDeepSeekTurnsTranslatesFunctionCallOutputWithName(t *testing.T) {
	items := []responsesInputItem{
		{Type: "function_call_output", Role: "tool", Name: "bash", Text: "hello\nworld"},
	}
	turns := deepseekTurnsFromItems(items)
	if len(turns) != 1 {
		t.Fatalf("turns=%d, want 1", len(turns))
	}
	want := "Tool (Resultado da ferramenta bash): hello\nworld"
	if turns[0].Text != want {
		t.Fatalf("function_call_output 翻译不一致:\n got %q\nwant %q", turns[0].Text, want)
	}
}

// TestDeepSeekTurnsTruncatesLongToolResult 断言 >4096 字节的结果按字节截断
// 并追加后缀(防长结果挤占上下文)。
func TestDeepSeekTurnsTruncatesLongToolResult(t *testing.T) {
	content := strings.Repeat("a", 4097)
	turns := deepseekTurnsFromItems([]responsesInputItem{
		{Type: "function_call_output", Role: "tool", Name: "read", Text: content},
	})
	if len(turns) != 1 {
		t.Fatalf("turns=%d, want 1", len(turns))
	}
	prefix := "Tool (Resultado da ferramenta read): "
	want := prefix + strings.Repeat("a", 4096) + "…（已截断）"
	if turns[0].Text != want {
		t.Fatalf("超限结果截断不一致: got %d 字节, want %d 字节", len(turns[0].Text), len(want))
	}
	if !strings.HasPrefix(turns[0].Text, prefix) || !strings.HasSuffix(turns[0].Text, "…（已截断）") {
		t.Fatalf("截断前缀/后缀不对: %q", turns[0].Text)
	}
}

// TestDeepSeekTurnsResolvesToolResultName 覆盖 name 反查三分支:
// 自带 name → call_id 反查最近一次 function_call → 回落 "tool"。
func TestDeepSeekTurnsResolvesToolResultName(t *testing.T) {
	cases := []struct {
		name  string
		items []responsesInputItem
		want  string
	}{
		{
			name: "自带 name 优先",
			items: []responsesInputItem{
				responsesFunctionCallItem("read", `{"path":"a"}`, "call_1"),
				{Type: "function_call_output", Role: "tool", Name: "grep", CallID: "call_1", Text: "x"},
			},
			want: "Tool (Resultado da ferramenta grep): x",
		},
		{
			name: "call_id 反查最近一次 function_call",
			items: []responsesInputItem{
				responsesFunctionCallItem("read", `{"path":"a"}`, "call_1"),
				{Type: "function_call_output", Role: "tool", CallID: "call_1", Text: "x"},
			},
			want: "Tool (Resultado da ferramenta read): x",
		},
		{
			name: "call_id 不匹配回落 tool",
			items: []responsesInputItem{
				responsesFunctionCallItem("read", `{"path":"a"}`, "call_1"),
				{Type: "function_call_output", Role: "tool", CallID: "call_x", Text: "x"},
			},
			want: "Tool (Resultado da ferramenta tool): x",
		},
		{
			name: "完全没有 name/call_id",
			items: []responsesInputItem{
				{Type: "function_call_output", Role: "tool", Text: "x"},
			},
			want: "Tool (Resultado da ferramenta tool): x",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			turns := deepseekTurnsFromItems(tc.items)
			if len(turns) == 0 {
				t.Fatalf("turns 为空")
			}
			if got := turns[len(turns)-1].Text; got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDeepSeekTurnsFromChatMessages 覆盖 chat.completions 形态:apiMessagesToItems
// 必须补上 Name / CallID(name 缺失时靠 call_id 反查)。
func TestDeepSeekTurnsFromChatMessages(t *testing.T) {
	items := apiMessagesToItems([]official.APIMessage{
		{Role: "user", Content: official.MessageContent{TextValue: "读一下"}},
		{Role: "assistant", ToolCalls: []official.ToolCallRef{toolCallRefForTest("call_1", "read", `{"path":"a.txt"}`)}},
		// 客户端没带 name,只有 tool_call_id
		{Role: "tool", ToolCallID: "call_1", Content: official.MessageContent{TextValue: "文件内容"}},
	})
	turns := deepseekTurnsFromItems(items)
	if len(turns) != 3 {
		t.Fatalf("turns=%d, want 3: %+v", len(turns), turns)
	}
	wantCall := `<tool_call>{"name": "read", "arguments": {"path":"a.txt"}}</tool_call>`
	if turns[1].Text != wantCall {
		t.Fatalf("assistant 历史调用翻译不一致: got %q want %q", turns[1].Text, wantCall)
	}
	wantResult := "Tool (Resultado da ferramenta read): 文件内容"
	if turns[2].Text != wantResult {
		t.Fatalf("工具结果翻译不一致: got %q want %q", turns[2].Text, wantResult)
	}

	// 字段填充本身:apiMessagesToItems 必须把 Name/ToolCallID 落到 item 上。
	if items[2].CallID != "call_1" {
		t.Fatalf("function_call_output item 丢了 CallID: %+v", items[2])
	}
}

// TestDeepSeekTurnsSkipsUnparsableFunctionCall 断言解不出的 function_call
// 被跳过(不 panic、不产生空 turn);缺省 arguments 回落 "{}"(坏 JSON 会
// 让模型无格式可照抄)。
func TestDeepSeekTurnsSkipsUnparsableFunctionCall(t *testing.T) {
	items := []responsesInputItem{
		{Type: "function_call", Role: "assistant", Text: "not json at all"},
		{Type: "function_call", Role: "assistant", Text: `{"type":"tool_calls","tool_calls":{"arguments":"{}"}}`},
		{Type: "function_call", Role: "assistant", Text: `{"type":"tool_calls","tool_calls":{"name":"noargs","arguments":""}}`},
		{Type: "message", Role: "user", Text: "hi"},
	}
	turns := deepseekTurnsFromItems(items)
	if len(turns) != 2 {
		t.Fatalf("解不出的 function_call 应被跳过, got %+v", turns)
	}
	if want := `<tool_call>{"name": "noargs", "arguments": {}}</tool_call>`; turns[0].Text != want {
		t.Fatalf("缺省 arguments 应回落 {}: got %q want %q", turns[0].Text, want)
	}
	if turns[1].Text != "hi" {
		t.Fatalf("普通文本应原样保留, got %q", turns[1].Text)
	}
}

// TestDeepSeekTurnsPlainTextRegression 是 L4 回归判据:无工具历史的输入,
// 输出必须与改动前逐字一致(写死期望值,不引用实现)。
func TestDeepSeekTurnsPlainTextRegression(t *testing.T) {
	items := apiMessagesToItems([]official.APIMessage{
		{Role: "system", Content: official.MessageContent{TextValue: "你是助手"}},
		{Role: "user", Content: official.MessageContent{TextValue: "你好"}},
		{Role: "assistant", Content: official.MessageContent{TextValue: "你好,有什么可以帮你?"}},
		{Role: "user", Content: official.MessageContent{TextValue: "继续"}},
	})
	want := []string{"你是助手", "你好", "你好,有什么可以帮你?", "继续"}
	turns := deepseekTurnsFromItems(items)
	if len(turns) != len(want) {
		t.Fatalf("turns=%d, want %d: %+v", len(turns), len(want), turns)
	}
	for i := range want {
		if turns[i].Text != want[i] {
			t.Fatalf("turn[%d]=%q, want %q", i, turns[i].Text, want[i])
		}
	}
	// 拍平后的 prompt 也必须与改动前一致(deepseek_chat_session.buildRequest 口径)。
	if got, wantPrompt := flattenChatTurns(turns, ""), "你是助手\n\n你好\n\n你好,有什么可以帮你?\n\n继续"; got != wantPrompt {
		t.Fatalf("拍平 prompt 变了: got %q want %q", got, wantPrompt)
	}
}

// ── CollectText(假上游 httptest,全程离线)──

// bodyRecorder 包一层假上游,记录每个请求的原始 body(断言 tools 未透传)。
type bodyRecorder struct {
	inner  http.Handler
	mu     sync.Mutex
	bodies []string
}

func (r *bodyRecorder) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.bodies = append(r.bodies, string(b))
	r.mu.Unlock()
	req.Body = io.NopCloser(bytes.NewReader(b))
	r.inner.ServeHTTP(rw, req)
}

func (r *bodyRecorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.bodies))
	copy(out, r.bodies)
	return out
}

// TestDeepSeekCollectTextNewSessionCarriesInjection 验收 CollectText:
//   - 每轮新开会话(忽略 user 字段/请求头,clientKey 恒空)→ 注入片段不丢;
//   - 送给上游的 prompt 含注入片段与工具历史;
//   - req.Tools 不透传上游;
//   - 返回文本正确,不写 gin.Context。
func TestDeepSeekCollectTextNewSessionCarriesInjection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := &wireUpstream{}
	rec := &bodyRecorder{inner: w.handler(t)}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	d := newWireProvider(t, srv.URL)

	const marker = "INJECT-MARKER: 工具调用协议"
	req := official.APIRequest{
		Model:  "deepseek",
		User:   "same-user", // 即便带 user 字段也必须每轮新建
		Stream: true,        // CollectText 恒非流式:该标志应被忽略
		Messages: []official.APIMessage{
			{Role: "system", Content: official.MessageContent{TextValue: marker}},
			{Role: "user", Content: official.MessageContent{TextValue: "读一下 a.txt"}},
			{Role: "assistant", ToolCalls: []official.ToolCallRef{toolCallRefForTest("call_1", "read", `{"path":"a.txt"}`)}},
			{Role: "tool", ToolCallID: "call_1", Content: official.MessageContent{TextValue: "文件内容"}},
			{Role: "user", Content: official.MessageContent{TextValue: "继续"}},
		},
		Tools: []official.Tool{{Type: "function", Function: official.ToolFunction{Name: "read"}}},
	}

	ctxRec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(ctxRec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(nil))

	text, err := d.CollectText(c, &req)
	if err != nil {
		t.Fatalf("CollectText: %v", err)
	}
	if text != "回复1" {
		t.Fatalf("text=%q, want 回复1", text)
	}
	// 第二轮:同 user 字段,仍必须新开会话(否则续轮只发末条 message,注入丢失)。
	if _, err := d.CollectText(c, &req); err != nil {
		t.Fatalf("CollectText 第二轮: %v", err)
	}

	if w.creates != 2 || w.deletes != 2 {
		t.Fatalf("每轮应新开并删除一个会话, creates=%d deletes=%d", w.creates, w.deletes)
	}
	if len(w.completions) != 2 {
		t.Fatalf("completions=%d, want 2", len(w.completions))
	}
	for i, comp := range w.completions {
		if comp.ParentMessageID != nil {
			t.Fatalf("第 %d 轮不应有续轮锚点, got %v", i+1, comp.ParentMessageID)
		}
		if !strings.Contains(comp.Prompt, marker) {
			t.Fatalf("第 %d 轮 prompt 丢了注入片段: %q", i+1, comp.Prompt)
		}
		if !strings.Contains(comp.Prompt, `<tool_call>{"name": "read", "arguments": {"path":"a.txt"}}</tool_call>`) {
			t.Fatalf("第 %d 轮 prompt 丢了工具历史调用: %q", i+1, comp.Prompt)
		}
		if !strings.Contains(comp.Prompt, "Tool (Resultado da ferramenta read): 文件内容") {
			t.Fatalf("第 %d 轮 prompt 丢了工具结果: %q", i+1, comp.Prompt)
		}
		if !strings.Contains(comp.Prompt, "继续") {
			t.Fatalf("第 %d 轮 prompt 丢了末条消息: %q", i+1, comp.Prompt)
		}
	}
	// 硬规则:tools 不得透传上游(请求体里连字段都不该有)。
	for _, body := range rec.all() {
		if strings.Contains(body, `"tools"`) || strings.Contains(body, `"tool_choice"`) {
			t.Fatalf("上游请求体不得含 tools/tool_choice: %s", body)
		}
	}
	// 非流式取回:CollectText 不写 gin.Context。
	if ctxRec.Body.Len() != 0 {
		t.Fatalf("CollectText 不应写 gin.Context, got %q", ctxRec.Body.String())
	}
}

// TestDeepSeekCollectTextUpstreamError 断言上游失败透传为 error(handler 侧转 502)。
func TestDeepSeekCollectTextUpstreamError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := &wireUpstream{failCompletions: map[int]bool{0: true}}
	rec := &bodyRecorder{inner: w.handler(t)}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	d := newWireProvider(t, srv.URL)

	req := official.APIRequest{Model: "deepseek", Messages: []official.APIMessage{
		{Role: "user", Content: official.MessageContent{TextValue: "你好"}},
	}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(nil))

	if _, err := d.CollectText(c, &req); err == nil {
		t.Fatal("上游失败应返回 error")
	}
}

// TestDeepSeekCollectTextUnknownModel 断言未知模型返回 error 而非 panic。
func TestDeepSeekCollectTextUnknownModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := &wireUpstream{}
	srv := httptest.NewServer(w.handler(t))
	defer srv.Close()
	d := newWireProvider(t, srv.URL)

	req := official.APIRequest{Model: "no-such-model", Messages: []official.APIMessage{
		{Role: "user", Content: official.MessageContent{TextValue: "你好"}},
	}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(nil))

	if _, err := d.CollectText(c, &req); err == nil {
		t.Fatal("未知模型应返回 error")
	}
}

// TestDeepSeekTurnsTruncatesLongToolResultMultibyte 覆盖 4096 字节边界落在
// 多字节字符(4 字节 emoji)中间的截断:结果必须是合法 UTF-8、payload 不超过
// 4096 字节,且后缀保留。
func TestDeepSeekTurnsTruncatesLongToolResultMultibyte(t *testing.T) {
	// 边界正好切进 emoji:4094 个 'a' 之后是 4 字节字符。
	content := strings.Repeat("a", 4094) + "😀" + strings.Repeat("b", 100)
	turns := deepseekTurnsFromItems([]responsesInputItem{
		{Type: "function_call_output", Role: "tool", Name: "read", Text: content},
	})
	if len(turns) != 1 {
		t.Fatalf("turns=%d, want 1", len(turns))
	}
	prefix := "Tool (Resultado da ferramenta read): "
	text := turns[0].Text
	if !strings.HasPrefix(text, prefix) {
		t.Fatalf("前缀丢失: %q", text)
	}
	if !strings.HasSuffix(text, toolResultTruncatedTail) {
		t.Fatalf("截断后缀丢失: %q", text)
	}
	if !utf8.ValidString(text) {
		t.Fatal("截断结果不是合法 UTF-8")
	}
	payload := strings.TrimSuffix(strings.TrimPrefix(text, prefix), toolResultTruncatedTail)
	if len(payload) > toolResultMaxBytes {
		t.Fatalf("payload = %d 字节, 超过上限 %d", len(payload), toolResultMaxBytes)
	}
	if payload != strings.Repeat("a", 4094) {
		t.Fatalf("跨边界的半个多字节字符应被丢弃: payload 长度=%d", len(payload))
	}
}
