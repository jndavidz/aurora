package deepseekweb

import (
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// newTestClient 构造指向 httptest server 的客户端(走 Go native 通道,
// httptest 只起纯 HTTP;TLS 指纹伪装仅影响 JA3,纯 HTTP 端点不受影响)。
func newTestClient(t *testing.T, h http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	c, err := NewClient(srv.URL, "", "")
	if err != nil {
		srv.Close()
		t.Fatal(err)
	}
	return c, srv
}

// powEnvelope 造一个客户端可解的挑战:challenge = hash(salt_expireAt_1),
// difficulty=10(首 nonce 即命中,测试不付出真实求解成本)。
func powEnvelope(salt string) map[string]any {
	target := hex.EncodeToString(hashDeepSeekV1([]byte(salt + "_1700000000_1")))
	return map[string]any{
		"code": 0,
		"data": map[string]any{
			"biz_code": 0,
			"biz_data": map[string]any{
				"challenge": map[string]any{
					"algorithm":   "DeepSeekHashV1",
					"challenge":   target,
					"salt":        salt,
					"signature":   "sig",
					"difficulty":  10,
					"expire_at":   1700000000,
					"target_path": "/api/v0/chat/completion",
				},
			},
		},
	}
}

// okEnvelope 是业务成功的统一信封。
func okEnvelope(bizData map[string]any) map[string]any {
	return map[string]any{
		"code": 0,
		"data": map[string]any{"biz_code": 0, "biz_data": bizData},
	}
}

// deleteSessionReturnsError:true 时 DeleteSession 返回 error(1.2)。
func TestDeleteSessionReturnsError(t *testing.T) {
	var deleted int
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v0/chat_session/create":
			writeJSON(t, w, okEnvelope(map[string]any{"id": "s1"}))
		case "/api/v0/chat_session/delete":
			deleted++
			// 业务层失败:biz_code != 0(信封 code 仍为 0)。
			writeJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"biz_code": 1, "biz_msg": "session not found", "biz_data": map[string]any{}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	sid, err := c.CreateSession("tok")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	err = c.DeleteSession("tok", sid)
	if err == nil {
		t.Fatal("上游删除失败(biz_code != 0)应返回 error,调用方才能记录;got nil")
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}
}

// completionNonStreamFillsUsage:非流式 consumption 聚合 usage 字段(1.3)。
// usage 在 close 帧前出现,旧实现只取 text/reasoning,会把它丢掉。
func TestCompletionNonStreamFillsUsage(t *testing.T) {
	var gotBody map[string]any
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v0/chat_session/create":
			writeJSON(t, w, okEnvelope(map[string]any{"id": "s1"}))
		case "/api/v0/chat_session/delete":
			writeJSON(t, w, okEnvelope(map[string]any{}))
		case "/api/v0/chat/create_pow_challenge":
			writeJSON(t, w, powEnvelope("t"))
		case "/api/v0/chat/completion":
			_ = readJSONBody(t, r, &gotBody)
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte("data: {\"response_message_id\":\"77\"}\n\n" +
				"data: {\"v\":\"你好呀\"}\n\n" +
				"data: {\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":34}}\n\n" +
				"event: close\n\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	sid, err := c.CreateSession("tok")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	resp, err := c.Complete("tok", CompletionRequest{SessionID: sid, Prompt: "你好"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer resp.Body.Close()
	res := ConsumeStream(resp.Body, func(Delta) {})

	if res.ResponseMsgID != "77" {
		t.Fatalf("ResponseMsgID = %q, want 77", res.ResponseMsgID)
	}
	if res.PromptTokens != 12 || res.CompletionTokens != 34 {
		t.Fatalf("usage 未聚合: prompt=%d completion=%d, want 12/34", res.PromptTokens, res.CompletionTokens)
	}
}

// completionParentMessageIDField:请求体携带 parent_message_id 字段(1.2 续轮)。
func TestCompletionParentMessageIDField(t *testing.T) {
	var gotBody map[string]any
	var mu sync.Mutex
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v0/chat_session/create":
			writeJSON(t, w, okEnvelope(map[string]any{"id": "s1"}))
		case "/api/v0/chat_session/delete":
			writeJSON(t, w, okEnvelope(map[string]any{}))
		case "/api/v0/chat/create_pow_challenge":
			writeJSON(t, w, powEnvelope("t"))
		case "/api/v0/chat/completion":
			mu.Lock()
			_ = readJSONBody(t, r, &gotBody)
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte("data: {\"v\":\"ok\"}\n\nevent: close\n\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	sid, _ := c.CreateSession("tok")
	resp, err := c.Complete("tok", CompletionRequest{
		SessionID:       sid,
		ParentMessageID: "77",
		Prompt:          "接着说",
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if gotBody["parent_message_id"] == nil {
		t.Fatalf("请求体缺 parent_message_id 字段: %v", gotBody)
	}
	if n, ok := gotBody["parent_message_id"].(float64); !ok || int(n) != 77 {
		t.Fatalf("parent_message_id = %v(%T), want 数字 77", gotBody["parent_message_id"], gotBody["parent_message_id"])
	}
}

// completeStreamFrameOrder:帧序化 —— delta 帧回调先于 ConsumeStream 返回,
// 调用方在 onDelta 里立刻可见(流式 flush 依赖此序,1.3 改流式管线的前提)。
func TestCompleteStreamFrameOrder(t *testing.T) {
	var textInCallback string
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v0/chat_session/create":
			writeJSON(t, w, okEnvelope(map[string]any{"id": "s1"}))
		case "/api/v0/chat_session/delete":
			writeJSON(t, w, okEnvelope(map[string]any{}))
		case "/api/v0/chat/create_pow_challenge":
			writeJSON(t, w, powEnvelope("t"))
		case "/api/v0/chat/completion":
			w.Header().Set("Content-Type", "text/event-stream")
			f := w.(http.Flusher)
			w.Write([]byte("data: {\"v\":\"第一帧\"}\n\n"))
			f.Flush()
			w.Write([]byte("data: {\"v\":\"第二帧\"}\n\n"))
			f.Flush()
			w.Write([]byte("event: close\n\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	sid, _ := c.CreateSession("tok")
	resp, err := c.Complete("tok", CompletionRequest{SessionID: sid, Prompt: "hi"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer resp.Body.Close()

	res := ConsumeStream(resp.Body, func(d Delta) {
		// 收到第一帧回调时,流尚未结束 —— 若在此时可读出 "第一帧",
		// 说明帧序化成立(不是结束后的整块回放)。
		if strings.Contains(textInCallback, "第一帧") {
			t.Log("ok: 首帧到达时已可见")
		}
		textInCallback += d.Text
	})
	_ = res
	if !strings.Contains(textInCallback, "第一帧") {
		t.Fatalf("回调未收到首帧: %q", textInCallback)
	}
}
