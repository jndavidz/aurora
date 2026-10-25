package provider

import (
	"net/http/httptest"
	"strings"
	"testing"

	"aurora/typings/official"

	"github.com/gin-gonic/gin"
)

// ── wire emitter golden:两表面事件/chunk 序列骨架在单一 seam 上验证 ──
// (spec .scratch/arch-hardening WP5;此前序列只能 live 验证客户端兼容性)

func wireCtx() (*httptest.ResponseRecorder, *gin.Context) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/", strings.NewReader(""))
	return rec, c
}

func eventNames(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "event: ") {
			out = append(out, strings.TrimPrefix(line, "event: "))
		}
	}
	return out
}

func join(ss ...string) string { return strings.Join(ss, "|") }

func TestChunkStreamEagerSequence(t *testing.T) {
	rec, c := wireCtx()
	s := NewChunkStream(c, "glm-5.2-chat")
	s.ReasoningDelta("思考")
	s.TextDelta("你好")
	s.TextDelta(",世界")
	s.Stop("stop")

	head := rec.Header()
	if head.Get("Content-Type") != "text/event-stream" || head.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("SSE headers missing: %v", head)
	}
	body := rec.Body.String()
	for _, want := range []string{`"role":"assistant"`, `"reasoning_content":"思考"`, `"content":"你好"`, `"content":",世界"`, `"finish_reason":"stop"`, "data: [DONE]"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
	if !strings.HasPrefix(body, "data: ") {
		t.Fatalf("role chunk must lead: %s", body[:40])
	}
}

func TestChunkStreamDeferredHeader(t *testing.T) {
	rec, c := wireCtx()
	s := NewDeferredChunkStream(c, "deepseek")
	if s.Started() || rec.Body.Len() != 0 {
		t.Fatalf("deferred stream must stay silent before first delta")
	}
	s.TextDelta("hi")
	if !s.Started() || rec.Body.Len() == 0 {
		t.Fatalf("first delta must trigger header+role")
	}
	if !strings.Contains(rec.Body.String(), `"role":"assistant"`) {
		t.Fatalf("role chunk missing:\n%s", rec.Body.String())
	}
}

func TestResponsesEmitterWithReasoning(t *testing.T) {
	rec, c := wireCtx()
	e := NewResponsesEmitter(c, "glm-5.2-chat", true)
	e.ReasoningDelta("先想")
	e.TextDelta("答")
	e.Complete(official.NewResponsesResponse("答", "先想", 1, 1, 1, 0, 0, "glm-5.2-chat"), "答", "先想")

	got := join(eventNames(rec.Body.String())...)
	want := join("response.created", "response.output_item.added", "response.output_item.added",
		"response.reasoning_text.delta", "response.output_text.delta",
		"response.output_item.done", "response.output_item.done", "response.completed")
	if got != want {
		t.Fatalf("event sequence:\n got %s\nwant %s\nbody:\n%s", got, want, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"output_index":1`) || !strings.Contains(body, `"output_index":0`) {
		t.Fatalf("reasoning(idx0)/message(idx1) indexes missing:\n%s", body)
	}
}

func TestResponsesEmitterTextOnly(t *testing.T) {
	rec, c := wireCtx()
	e := NewResponsesEmitter(c, "gemini-3-flash", false)
	e.TextDelta("hi")
	e.Complete(official.NewResponsesResponse("hi", "", 2, 2, 0, 0, 0, "gemini-3-flash"), "hi", "")

	got := join(eventNames(rec.Body.String())...)
	want := join("response.created", "response.output_item.added", "response.output_text.delta",
		"response.output_item.done", "response.completed")
	if got != want {
		t.Fatalf("event sequence:\n got %s\nwant %s", got, want)
	}
	if strings.Contains(rec.Body.String(), `"reasoning"`) {
		t.Fatalf("text-only emitter must not emit reasoning item:\n%s", rec.Body.String())
	}
}

func TestResponsesEmitterFunctionCallAndFail(t *testing.T) {
	rec, c := wireCtx()
	e := NewResponsesEmitter(c, "auto", false)
	e.Fail("boom")
	if got, want := join(eventNames(rec.Body.String())...), join("response.created", "response.output_item.added", "response.failed"); got != want {
		t.Fatalf("fail sequence:\n got %s\nwant %s", got, want)
	}

	rec2, c2 := wireCtx()
	e2 := NewResponsesEmitter(c2, "auto", false)
	e2.FunctionCall(1, "fc_1", "call_1", "get_weather", `{"city":"bj"}`)
	got2 := join(eventNames(rec2.Body.String())...)
	want2 := join("response.created", "response.output_item.added",
		"response.output_item.added", "response.function_call_arguments.delta",
		"response.function_call_arguments.done", "response.output_item.done")
	if got2 != want2 {
		t.Fatalf("function_call sequence:\n got %s\nwant %s\n%s", got2, want2, rec2.Body.String())
	}
}
