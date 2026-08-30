package provider

import (
	"encoding/json"
	"net/http"
	"time"

	"aurora/typings/official"

	"github.com/gin-gonic/gin"
)

// sseWriter 是 Responses 流式事件输出器,封装 gin 的 SSE 写入与 Flush。
type sseWriter struct {
	c       *gin.Context
	flusher http.Flusher
}

func newSSEWriter(c *gin.Context) *sseWriter {
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := c.Writer.(http.Flusher)
	return &sseWriter{c: c, flusher: flusher}
}

func (w *sseWriter) event(name string, data any) {
	b, _ := json.Marshal(data)
	w.c.Writer.WriteString("event: " + name + "\ndata: " + string(b) + "\n\n")
	if w.flusher != nil {
		w.flusher.Flush()
	}
}

// createdEvent 生成 response.created 事件。
func createdEvent(respID, model string) map[string]any {
	return map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id": respID, "object": "response", "created_at": nowUnix(),
			"model": model, "status": "in_progress",
		},
	}
}

func outputItemAddedEvent(outputIndex int, item map[string]any) map[string]any {
	return map[string]any{"type": "response.output_item.added", "output_index": outputIndex, "item": item}
}

func outputItemDoneEvent(outputIndex int, item map[string]any) map[string]any {
	return map[string]any{"type": "response.output_item.done", "output_index": outputIndex, "item": item}
}

func failedEvent(msg string) map[string]any {
	return map[string]any{
		"type":     "response.failed",
		"response": map[string]any{"error": map[string]any{"message": msg, "type": "server_error"}},
	}
}

func completedEvent(resp official.ResponsesResponse) map[string]any {
	return map[string]any{"type": "response.completed", "response": resp}
}

func messageItem(id, text string, status string) map[string]any {
	return map[string]any{
		"id": id, "type": "message", "status": status, "role": "assistant",
		"content": []map[string]any{{"type": "output_text", "text": text}},
	}
}

func reasoningItem(id, text string, status string) map[string]any {
	return map[string]any{
		"id": id, "type": "reasoning", "status": status,
		"content": []map[string]any{{"type": "reasoning_text", "text": text}},
	}
}

func functionCallItem(id, callID, name, arguments, status string) map[string]any {
	return map[string]any{
		"id": id, "type": "function_call", "status": status,
		"call_id": callID, "name": name, "arguments": arguments,
	}
}

func nowUnix() int64 { return time.Now().Unix() }

// jsonNowUnix 与 nowUnix 等价,保留别名以免调用处歧义。
func jsonNowUnix() int64 { return time.Now().Unix() }
