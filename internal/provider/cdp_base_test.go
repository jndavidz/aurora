package provider

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"aurora/internal/config"
	"aurora/typings/official"

	"github.com/gin-gonic/gin"
)

// ── CDP 桥 wire 回环:假桥 + ChatgptCDP 公开入口 ──
//
// 验证 -chat 后缀归一收进 cdpBase.internalID 后:Responses 表面对外部裸名
// (gpt-5.6)不再 404(修复前 Handles 命中路由进来,基类查 byID[裸名] 落空);
// ChatCompletions 发桥的 model 仍为内部 -chat 形态(与旧行为一致)。
// spec: .scratch/arch-hardening WP2。

// newFakeBridge 起一个假 CDP 桥:流式回标准 OpenAI SSE,非流式回 completion JSON;
// 记录每个请求体的 model 字段供断言。
func newFakeBridge(t *testing.T) (*bridgeRecorder, *httptest.Server) {
	t.Helper()
	rec := &bridgeRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model   string `json:"model"`
			Stream  bool   `json:"stream"`
			Message string `json:"message"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Message != "" {
			body.Model = "" // 单条消息形态(streamPrompt/completePrompt)
		}
		rec.record(body.Model)
		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"你好\"}}]}\n\n"))
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\",世界\"}}]}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"你好,世界"}}]}`))
	}))
	t.Cleanup(srv.Close)
	return rec, srv
}

type bridgeRecorder struct {
	mu     sync.Mutex
	models []string
}

func (b *bridgeRecorder) record(model string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.models = append(b.models, model)
}

func (b *bridgeRecorder) last() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.models) == 0 {
		return ""
	}
	return b.models[len(b.models)-1]
}

func newChatgptCDPForTest(bridgeURL string) *ChatgptCDP {
	return NewChatgptCDP(&config.Config{
		ChatgptCDPURL: bridgeURL,
		CodingEnabled: false,
	})
}

func ginCtxForCDP() (*httptest.ResponseRecorder, *gin.Context) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(nil))
	return rec, c
}

// TestChatgptCDPResponsesNoSuffix404 Responses 表面:外部裸名 gpt-5.6 不再 404,
// 事件序列完整(created → delta → completed)。
func TestChatgptCDPResponsesNoSuffix404(t *testing.T) {
	_, srv := newFakeBridge(t)
	p := newChatgptCDPForTest(srv.URL)

	items := []map[string]any{{"type": "message", "role": "user", "content": "hi"}}
	raw, _ := json.Marshal(items)
	rec, c := ginCtxForCDP()
	p.Responses(c, &official.ResponsesAPIRequest{Input: raw, Model: "gpt-5.6", Stream: true})

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "model_not_found") {
		t.Fatalf("unexpected 404 model_not_found leaked: %s", body)
	}
	for _, want := range []string{"response.created", "response.output_text.delta", "你好", ",世界", "response.completed"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q: %s", want, body)
		}
	}
}

// TestChatgptCDPChatCompletionsSuffix ChatCompletions 表面:裸名归一为内部
// -chat 形态发桥(与修复前手工补丁行为一致),响应正常透传。
func TestChatgptCDPChatCompletionsSuffix(t *testing.T) {
	br, srv := newFakeBridge(t)
	p := newChatgptCDPForTest(srv.URL)

	rec, c := ginCtxForCDP()
	p.ChatCompletions(c, &official.APIRequest{Model: "gpt-5.6", Messages: apiMessages("hi")})

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "你好,世界") {
		t.Fatalf("bridge reply not relayed: %s", rec.Body.String())
	}
	if got := br.last(); got != "gpt-5.6-chat" {
		t.Fatalf("bridge saw model %q, want internal %q", got, "gpt-5.6-chat")
	}
}

// TestChatgptCDPHandlesForms Handles 接受裸名/内部名/auto,拒绝他家前缀。
func TestChatgptCDPHandlesForms(t *testing.T) {
	_, srv := newFakeBridge(t)
	p := newChatgptCDPForTest(srv.URL)
	for _, m := range []string{"gpt-5.6", "gpt-5.6-chat", "gpt-5.6-mini", "auto"} {
		if !p.Handles(m) {
			t.Errorf("Handles(%q) = false, want true", m)
		}
	}
	for _, m := range []string{"gemini-3-flash", "glm-5.2-chat", "gpt-4"} {
		if p.Handles(m) {
			t.Errorf("Handles(%q) = true, want false", m)
		}
	}
}

func apiMessages(s string) []official.APIMessage {
	return []official.APIMessage{{Role: "user", Content: official.MessageContent{TextValue: s}}}
}
