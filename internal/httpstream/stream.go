package httpstream

import (
	"encoding/json"
	"strings"

	"github.com/gin-gonic/gin"
)

// WriteSSEHeader 设置标准 SSE 响应头。
func WriteSSEHeader(c *gin.Context) {
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(200)
}

// WriteSSEEvent 写入一个 SSE 事件。
func WriteSSEEvent(c *gin.Context, event string, payload interface{}) bool {
	data, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	if event != "" {
		if _, err := c.Writer.WriteString("event: " + event + "\n"); err != nil {
			return false
		}
	}
	if _, err := c.Writer.WriteString("data: "); err != nil {
		return false
	}
	if _, err := c.Writer.Write(data); err != nil {
		return false
	}
	if _, err := c.Writer.WriteString("\n\n"); err != nil {
		return false
	}
	c.Writer.Flush()
	return true
}

// WriteDone 写入 SSE 终止标记。
func WriteDone(c *gin.Context) bool {
	if _, err := c.Writer.WriteString("data: [DONE]\n\n"); err != nil {
		return false
	}
	c.Writer.Flush()
	return true
}

// WriteChatCompletionDone 写入 chat completion 的 stop chunk 和 [DONE]。
func WriteChatCompletionDone(c *gin.Context, stopSent bool, model string, conversationID string) {
	if !stopSent {
		chunk := map[string]interface{}{
			"id":              "chatcmpl-QXlha2FBbmROaXhpZUFyZUF3ZXNvbWUK",
			"object":          "chat.completion.chunk",
			"conversation_id": conversationID,
			"choices": []interface{}{
				map[string]interface{}{
					"index":         0,
					"delta":         map[string]interface{}{},
					"finish_reason": "stop",
				},
			},
		}
		data, _ := json.Marshal(chunk)
		c.Writer.WriteString("data: " + string(data) + "\n\n")
		c.Writer.Flush()
	}
	c.Writer.WriteString("data: [DONE]\n\n")
	c.Writer.Flush()
}

// WriteUsageChunk 写入 token usage 的 SSE chunk。
// cachedTokens / cacheWriteTokens 用于填充 prompt_tokens_details（缓存命中/写入 token 数）。
// msSinceStart / msTTFT 为耗时信息（毫秒），嵌入 chunk 的元字段（HTTP headers 在首次 Flush 后不可写）。
func WriteUsageChunk(c *gin.Context, model string, inputTokens, outputTokens, cachedTokens, cacheWriteTokens int, msSinceStart, msTTFT int64, ttftSet bool) {
	chunk := map[string]interface{}{
		"id":      "chatcmpl-QXlha2FBbmROaXhpZUFyZUF3ZXNvbWUK",
		"object":  "chat.completion.chunk",
		"created": 0,
		"model":   model,
		"choices": []interface{}{},
		"usage": map[string]interface{}{
			"prompt_tokens":     inputTokens,
			"completion_tokens": outputTokens,
			"total_tokens":      inputTokens + outputTokens,
			"prompt_tokens_details": map[string]interface{}{
				"cached_tokens":      cachedTokens,
				"cache_write_tokens": cacheWriteTokens,
			},
		},
		"ms_since_start": msSinceStart,
	}
	if ttftSet {
		chunk["ms_ttft"] = msTTFT
	}
	data, _ := json.Marshal(chunk)
	c.Writer.WriteString("data: " + string(data) + "\n\n")
	c.Writer.Flush()
}

// ── Stream 参数解析 ──

// RequestStreamFlag 解析 stream 参数,支持 JSON body 的 stream 字段或 ?stream=true 查询参数。
// multipart/form-data 也支持 stream 字段(字符串 "true"/"1")。
func RequestStreamFlag(c *gin.Context, jsonStream bool) bool {
	if jsonStream {
		return true
	}
	if v := strings.ToLower(strings.TrimSpace(c.Query("stream"))); v == "true" || v == "1" || v == "yes" {
		return true
	}
	if v := strings.ToLower(strings.TrimSpace(c.PostForm("stream"))); v == "true" || v == "1" || v == "yes" {
		return true
	}
	return false
}

// IsStreamTrue 把任意形式的 stream 字段值转换为 bool。
func IsStreamTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}
