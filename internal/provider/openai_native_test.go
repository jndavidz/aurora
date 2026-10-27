package provider

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"aurora/internal/config"
	"aurora/typings/official"

	"github.com/gin-gonic/gin"
)

// 编译期断言:NativeTools 实现 Provider 接口(ticket 04 §4.5-6)。
var _ Provider = (*NativeTools)(nil)

// nativeToolsJSON 是客户端声明的工具列表(原样转发的基准)。
const nativeToolsJSON = `[{"type":"function","function":{"name":"song_request","description":"点歌","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}]`

// nativeClientBody 是贴近 bridge 真实形态的 /v1/chat/completions 请求体:
// 带 tools / tool_choice / 多轮工具历史(assistant.tool_calls + role=tool)
// 与 aurora 自有字段 artifact_delivery(后者不得透传上游)。
const nativeClientBody = `{
	"model": "bridge-tools",
	"stream": false,
	"tools": ` + nativeToolsJSON + `,
	"tool_choice": "auto",
	"artifact_delivery": "inline",
	"messages": [
		{"role": "system", "content": "你是助手"},
		{"role": "user", "content": "来首歌"},
		{"role": "assistant", "content": "", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "song_request", "arguments": "{\"q\":\"晴天\"}"}}]},
		{"role": "tool", "tool_call_id": "call_1", "name": "song_request", "content": "已播放"}
	]
}`

// nativeNonStreamResp 假上游非流式响应(含 tool_calls),用于逐字节透传断言。
const nativeNonStreamResp = `{"id":"chatcmpl-native-1","object":"chat.completion","created":1,"model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_2","type":"function","function":{"name":"song_request","arguments":"{\"q\":\"晴天\"}"}}]},"finish_reason":"tool_calls"}]}`

// nativeCapture 记录假上游收到的请求(handler 在独立 goroutine,需加锁)。
type nativeCapture struct {
	mu          sync.Mutex
	path        string
	auth        string
	contentType string
	body        []byte
}

func (nc *nativeCapture) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	nc.mu.Lock()
	defer nc.mu.Unlock()
	nc.path = r.URL.Path
	nc.auth = r.Header.Get("Authorization")
	nc.contentType = r.Header.Get("Content-Type")
	nc.body = body
}

func (nc *nativeCapture) snapshot() (path, auth, contentType string, body []byte) {
	nc.mu.Lock()
	defer nc.mu.Unlock()
	return nc.path, nc.auth, nc.contentType, nc.body
}

func newNativeToolsForTest(t *testing.T, base string) *NativeTools {
	t.Helper()
	n := NewNativeTools(&config.Config{
		NativeToolsBase:     base,
		NativeToolsKey:      "test-key",
		NativeToolsModelMap: []string{"bridge-tools=upstream-model"},
	})
	if n == nil {
		t.Fatal("NewNativeTools 返回 nil,期望已注册")
	}
	return n
}

// nativeChatCall 模拟 handler:客户端 JSON 绑定为 official.APIRequest 后调
// ChatCompletions(与 chat_handler 的调用形态一致)。
func nativeChatCall(t *testing.T, n *NativeTools, clientBody string) *httptest.ResponseRecorder {
	t.Helper()
	var req official.APIRequest
	if err := json.Unmarshal([]byte(clientBody), &req); err != nil {
		t.Fatalf("解析客户端请求失败: %v", err)
	}
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(nil))
	n.ChatCompletions(c, &req)
	return rec
}

// 非流式:model 换成映射值;tools/tool_choice/tool 历史原样到达;
// 响应 body 逐字节透传。
func TestNativeToolsPassthroughNonStream(t *testing.T) {
	var capture nativeCapture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capture.record(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, nativeNonStreamResp)
	}))
	defer srv.Close()

	n := newNativeToolsForTest(t, srv.URL+"/v1")
	rec := nativeChatCall(t, n, nativeClientBody)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != nativeNonStreamResp {
		t.Errorf("响应未逐字节透传:\n got %s\nwant %s", got, nativeNonStreamResp)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	path, auth, contentType, body := capture.snapshot()
	if path != "/v1/chat/completions" {
		t.Errorf("上游 path = %q, want /v1/chat/completions", path)
	}
	if auth != "Bearer test-key" {
		t.Errorf("Authorization = %q, want Bearer test-key", auth)
	}
	if contentType != "application/json" {
		t.Errorf("请求 Content-Type = %q", contentType)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("上游请求体不是合法 JSON: %v (%s)", err, body)
	}

	// model 必须是映射后的上游 id
	var model string
	_ = json.Unmarshal(got["model"], &model)
	if model != "upstream-model" {
		t.Errorf("上游 model = %q, want upstream-model", model)
	}

	// tools 语义等价(格式化差异不算丢字段)
	var wantTools, gotTools any
	_ = json.Unmarshal([]byte(nativeToolsJSON), &wantTools)
	_ = json.Unmarshal(got["tools"], &gotTools)
	if !reflect.DeepEqual(wantTools, gotTools) {
		t.Errorf("tools 未原样转发:\n got %s\nwant %s", got["tools"], nativeToolsJSON)
	}

	// tool_choice 必须保持字符串形态 "auto"(不能被 re-marshal 成 {"type":"auto"})
	var toolChoice string
	if err := json.Unmarshal(got["tool_choice"], &toolChoice); err != nil || toolChoice != "auto" {
		t.Errorf("tool_choice 变形: %s (err=%v)", got["tool_choice"], err)
	}

	// aurora 自有字段不得透传
	if _, ok := got["artifact_delivery"]; ok {
		t.Errorf("artifact_delivery 不应转发上游: %s", got["artifact_delivery"])
	}

	// stream 原样
	var stream bool
	if err := json.Unmarshal(got["stream"], &stream); err != nil || stream {
		t.Errorf("stream = %s, want false", got["stream"])
	}

	// messages:4 条,assistant.tool_calls 不带 index,role=tool 原样
	var msgs []map[string]json.RawMessage
	if err := json.Unmarshal(got["messages"], &msgs); err != nil {
		t.Fatalf("messages 解析失败: %v", err)
	}
	if len(msgs) != 4 {
		t.Fatalf("messages 条数 = %d, want 4", len(msgs))
	}
	var calls []map[string]json.RawMessage
	if err := json.Unmarshal(msgs[2]["tool_calls"], &calls); err != nil || len(calls) != 1 {
		t.Fatalf("assistant tool_calls 丢失: %s (err=%v)", msgs[2]["tool_calls"], err)
	}
	if string(calls[0]["id"]) != `"call_1"` {
		t.Errorf("tool_call id = %s, want \"call_1\"", calls[0]["id"])
	}
	if _, ok := calls[0]["index"]; ok {
		t.Errorf("assistant tool_calls 不应带客户端不认识的 index: %s", calls[0]["index"])
	}
	var role, toolCallID, content string
	_ = json.Unmarshal(msgs[3]["role"], &role)
	_ = json.Unmarshal(msgs[3]["tool_call_id"], &toolCallID)
	_ = json.Unmarshal(msgs[3]["content"], &content)
	if role != "tool" || toolCallID != "call_1" || content != "已播放" {
		t.Errorf("role=tool 消息丢失: role=%q tool_call_id=%q content=%q", role, toolCallID, content)
	}
}

// 流式:3 帧 SSE 原样到达客户端(不解析 delta.tool_calls)。
func TestNativeToolsPassthroughStream(t *testing.T) {
	frames := []string{
		`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_3","type":"function","function":{"name":"song_request","arguments":"{\"q\":"}}]}}]}`,
		`[DONE]`,
	}
	var capture nativeCapture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capture.record(r)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, f := range frames {
			_, _ = io.WriteString(w, "data: "+f+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	n := newNativeToolsForTest(t, srv.URL)
	clientBody := strings.Replace(nativeClientBody, `"stream": false`, `"stream": true`, 1)
	rec := nativeChatCall(t, n, clientBody)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var want strings.Builder
	for _, f := range frames {
		want.WriteString("data: " + f + "\n\n")
	}
	if got := rec.Body.String(); got != want.String() {
		t.Errorf("流式帧未原样透传:\n got %q\nwant %q", got, want.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	// 流式必须带齐 SSE 头四件套(同包 sseHeaders)
	for header, want := range map[string]string{
		"Cache-Control":     "no-cache",
		"Connection":        "keep-alive",
		"X-Accel-Buffering": "no",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("流式响应头 %s = %q, want %q", header, got, want)
		}
	}
	var upstreamBody map[string]json.RawMessage
	_, _, _, body := capture.snapshot()
	if err := json.Unmarshal(body, &upstreamBody); err != nil {
		t.Fatalf("上游请求体解析失败: %v", err)
	}
	var stream bool
	_ = json.Unmarshal(upstreamBody["stream"], &stream)
	if !stream {
		t.Errorf("上游 stream = %s, want true", upstreamBody["stream"])
	}
}

// 上游 429:状态码与响应体原样回吐(bridge 只认 error 字段)。
func TestNativeToolsUpstreamErrorPassthrough(t *testing.T) {
	const errBody = `{"error":{"message":"rate limited","type":"rate_limit_error","code":"rate_limit_exceeded"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, errBody)
	}))
	defer srv.Close()

	n := newNativeToolsForTest(t, srv.URL)
	rec := nativeChatCall(t, n, nativeClientBody)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := rec.Body.String(); got != errBody {
		t.Errorf("上游错误体未原样回吐: got %s want %s", got, errBody)
	}
}

// 映射解析:非法/空/重复左值 → nil(fail-closed);合法 → Models 保序、Handles 精确。
func TestNewNativeToolsModelMap(t *testing.T) {
	baseCfg := func() *config.Config {
		return &config.Config{NativeToolsBase: "https://up.example/v1", NativeToolsKey: "k"}
	}

	bad := baseCfg()
	bad.NativeToolsModelMap = []string{"a=b", "", "bad", "c="}
	if got := NewNativeTools(bad); got != nil {
		t.Errorf("非法映射应返回 nil, got %+v", got)
	}

	empty := baseCfg()
	if got := NewNativeTools(empty); got != nil {
		t.Errorf("空映射应返回 nil, got %+v", got)
	}

	dup := baseCfg()
	dup.NativeToolsModelMap = []string{"a=b", "a=c"}
	if got := NewNativeTools(dup); got != nil {
		t.Errorf("重复左值应返回 nil, got %+v", got)
	}

	ok := baseCfg()
	ok.NativeToolsModelMap = []string{"a=b", "c=d"}
	n := NewNativeTools(ok)
	if n == nil {
		t.Fatal("合法映射应返回 provider")
	}
	if n.Name() != "openai-native" {
		t.Errorf("Name() = %q, want openai-native", n.Name())
	}
	var ids []string
	for _, m := range n.Models() {
		ids = append(ids, m.ID)
	}
	if !reflect.DeepEqual(ids, []string{"a", "c"}) {
		t.Errorf("Models() ids = %v, want [a c]", ids)
	}
	hasCaps := false
	for _, m := range n.Models() {
		for _, cap := range m.Caps {
			if cap == CapFunctionCall {
				hasCaps = true
			}
		}
	}
	if !hasCaps {
		t.Error("Models() 应标注 CapFunctionCall")
	}
	if !n.Handles("a") {
		t.Error(`Handles("a") 应为 true`)
	}
	if n.Handles("b") {
		t.Error(`Handles("b") 应为 false(左值才是客户端 id)`)
	}
}

// /v1/responses 本轮不支持:400 invalid_request_error,文本明确仅支持 chat/completions。
func TestNativeToolsResponsesUnsupported(t *testing.T) {
	n := newNativeToolsForTest(t, "https://up.example/v1")
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))

	n.Responses(c, &official.ResponsesAPIRequest{Model: "bridge-tools"})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "chat/completions") || !strings.Contains(body, "仅支持") {
		t.Errorf("错误文本未说明仅支持 chat/completions: %s", body)
	}
	var envelope struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("错误响应不是 JSON: %v", err)
	}
	if envelope.Error.Type != "invalid_request_error" {
		t.Errorf("error.type = %q, want invalid_request_error", envelope.Error.Type)
	}
}

// nativeForward 起一个假上游(固定 200 JSON),把 clientBody 原样过一遍
// ChatCompletions,返回假上游实际收到的请求体字段(供序列化口径断言)。
func nativeForward(t *testing.T, clientBody string) map[string]json.RawMessage {
	t.Helper()
	var capture nativeCapture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capture.record(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	n := newNativeToolsForTest(t, srv.URL)
	rec := nativeChatCall(t, n, clientBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	_, _, _, body := capture.snapshot()
	var got map[string]json.RawMessage
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("上游请求体不是合法 JSON: %v (%s)", err, body)
	}
	return got
}

// attachments 是 aurora 扩展,重序列化按白名单剥掉,不得泄漏给严格上游(7.9)。
func TestNativeToolsStripsAttachments(t *testing.T) {
	const bodyWithAttachment = `{
		"model": "bridge-tools",
		"messages": [{"role": "user", "content": "看图",
			"attachments": [{"file_id": "file-1", "name": "a.png", "mime_type": "image/png"}]}]
	}`

	got := nativeForward(t, bodyWithAttachment)

	var msgs []map[string]json.RawMessage
	if err := json.Unmarshal(got["messages"], &msgs); err != nil {
		t.Fatalf("messages 解析失败: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("messages 条数 = %d, want 1", len(msgs))
	}
	if _, ok := msgs[0]["attachments"]; ok {
		t.Errorf("attachments 不得转发上游: %s", msgs[0]["attachments"])
	}
	var role, content string
	_ = json.Unmarshal(msgs[0]["role"], &role)
	_ = json.Unmarshal(msgs[0]["content"], &content)
	if role != "user" || content != "看图" {
		t.Errorf("标准字段丢失: role=%q content=%q", role, content)
	}
}

// stop 空数组不得变成 {"Values":null},应直接删掉该字段(7.10)。
func TestNativeToolsEmptyStopDropped(t *testing.T) {
	clientBody := strings.Replace(nativeClientBody, `"tool_choice": "auto",`, `"tool_choice": "auto", "stop": [],`, 1)

	got := nativeForward(t, clientBody)

	if raw, ok := got["stop"]; ok {
		t.Errorf("空 stop 应删除该字段,得到: %s", raw)
	}
}

// stop 单元素数组还原成字符串(既有语义不回归)。
func TestNativeToolsSingleStopString(t *testing.T) {
	clientBody := strings.Replace(nativeClientBody, `"tool_choice": "auto",`, `"tool_choice": "auto", "stop": ["\n\n"],`, 1)

	got := nativeForward(t, clientBody)

	var stop string
	if err := json.Unmarshal(got["stop"], &stop); err != nil || stop != "\n\n" {
		t.Errorf("单元素 stop 应还原成字符串: %s (err=%v)", got["stop"], err)
	}
}

// tool_choice 的对象形态(function)按 OpenAI 线上形态重写(7.13)。
func TestNativeToolsToolChoiceFunctionObject(t *testing.T) {
	clientBody := strings.Replace(nativeClientBody, `"tool_choice": "auto",`,
		`"tool_choice": {"type": "function", "function": {"name": "song_request"}},`, 1)

	got := nativeForward(t, clientBody)

	var choice struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(got["tool_choice"], &choice); err != nil {
		t.Fatalf("tool_choice 解析失败: %v (%s)", err, got["tool_choice"])
	}
	if choice.Type != "function" || choice.Function.Name != "song_request" {
		t.Errorf("tool_choice 对象形态失真: %s", got["tool_choice"])
	}
}

// 上游 500(非 429)同样原样透传状态码与 body(7.13)。
func TestNativeToolsUpstream500Passthrough(t *testing.T) {
	const errBody = `{"error":{"message":"upstream boom","type":"server_error"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, errBody)
	}))
	defer srv.Close()

	n := newNativeToolsForTest(t, srv.URL)
	rec := nativeChatCall(t, n, nativeClientBody)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := rec.Body.String(); got != errBody {
		t.Errorf("上游 500 错误体未原样回吐: got %s want %s", got, errBody)
	}
}
