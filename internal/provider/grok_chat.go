package provider

import (
	"fmt"
	"net/http"
	"strings"

	"aurora/internal/apierrors"
	"aurora/internal/grokweb"
	"aurora/typings/official"
	"aurora/util"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// chatResponses 处理 Grok chat 变体(/v1/responses)。
//
// Grok 网页版模型自动使用原生搜索/沙盒,chat 变体只需纯文本对话。
// 多轮无服务端历史?否 —— Grok 会话按 parent_response_id 记忆,
// 但 aurora 每请求新会话,需把 input 全量拍平进 prompt。
func (d *Grok) chatResponses(c *gin.Context, m *grokModel, req *official.ResponsesAPIRequest) {
	client := d.webClient()
	if client == nil || !client.HasAccount() {
		apierrors.JSONError(c, 502, "api_error", "grok web client unavailable: missing GROK_COOKIES?", nil, "upstream_error")
		return
	}
	prompt := flattenChatInput(req, true) // chat 变体剥离 tools
	streamReq := grokweb.CompletionRequest{Prompt: prompt}
	if req.Stream {
		d.chatResponsesStream(c, req, client, streamReq)
		return
	}
	d.chatResponsesNonStream(c, req, client, streamReq)
}

// chatResponsesStream 流式输出 Responses 事件。
func (d *Grok) chatResponsesStream(c *gin.Context, req *official.ResponsesAPIRequest, client *grokweb.Client, streamReq grokweb.CompletionRequest) {
	w := newSSEWriter(c)
	respID := "resp_" + uuid.NewString()
	reasoningItemID := "rs_" + uuid.NewString()
	messageItemID := "msg_" + uuid.NewString()

	w.event("response.created", createdEvent(respID, req.Model))
	w.event("response.output_item.added", outputItemAddedEvent(0, map[string]any{"id": reasoningItemID, "type": "reasoning", "status": "in_progress"}))
	w.event("response.output_item.added", outputItemAddedEvent(1, map[string]any{"id": messageItemID, "type": "message", "status": "in_progress", "role": "assistant"}))

	var fullText, fullReasoning string
	// 流式剥离 "Thinking about your request" 前缀:前缀(可能跨 delta 分片)
	// 不输出正文;前缀前的文本作为推理输出。
	const prefix = "Thinking about your request"
	prefixBuf := ""
	prefixDone := false
	res := client.Complete(streamReq, func(delta grokweb.Delta) {
		if delta.Text == "" {
			return
		}
		if !prefixDone {
			prefixBuf += delta.Text
			if idx := strings.Index(prefixBuf, prefix); idx >= 0 {
				if pre := strings.TrimSpace(prefixBuf[:idx]); pre != "" {
					fullReasoning = pre
					w.event("response.reasoning_text.delta", map[string]any{
						"type": "response.reasoning_text.delta", "item_id": reasoningItemID,
						"output_index": 0, "content_index": 0, "delta": pre,
					})
				}
				// 前缀后的内容输出
				rest := prefixBuf[idx+len(prefix):]
				prefixBuf = ""
				prefixDone = true
				if rest != "" {
					fullText += rest
					w.event("response.output_text.delta", map[string]any{
						"type": "response.output_text.delta", "item_id": messageItemID,
						"output_index": 1, "content_index": 0, "delta": rest,
					})
				}
			}
			return
		}
		fullText += delta.Text
		w.event("response.output_text.delta", map[string]any{
			"type": "response.output_text.delta", "item_id": messageItemID,
			"output_index": 1, "content_index": 0, "delta": delta.Text,
		})
	})
	if !prefixDone && strings.TrimSpace(prefixBuf) != "" {
		// 整个流都没出现前缀:prefixBuf 是普通正文
		fullText = prefixBuf + fullText
	}
	if res.Err != "" && fullText == "" && fullReasoning == "" {
		w.event("response.failed", failedEvent(res.Err))
		return
	}
	w.event("response.output_item.done", outputItemDoneEvent(0, reasoningItem(reasoningItemID, fullReasoning, "completed")))
	w.event("response.output_item.done", outputItemDoneEvent(1, messageItem(messageItemID, fullText, "completed")))
	outResp := official.NewResponsesResponse(fullText, fullReasoning, countInputChars(req), util.CountToken(fullText), util.CountToken(fullReasoning), 0, 0, req.Model)
	w.event("response.completed", completedEvent(outResp))
}

// chatResponsesNonStream 非流式。
func (d *Grok) chatResponsesNonStream(c *gin.Context, req *official.ResponsesAPIRequest, client *grokweb.Client, streamReq grokweb.CompletionRequest) {
	var fullText string
	res := client.Complete(streamReq, func(delta grokweb.Delta) { fullText += delta.Text })
	fullText, reasoning := splitGrokThinking(fullText)
	if res.Err != "" && fullText == "" && reasoning == "" {
		apierrors.JSONError(c, 502, "api_error", res.Err, nil, "upstream_error")
		return
	}
	outResp := official.NewResponsesResponse(fullText, reasoning, countInputChars(req), util.CountToken(fullText), util.CountToken(reasoning), 0, 0, req.Model)
	c.JSON(200, outResp)
}

// chatCompletions 处理 Grok chat 变体(/v1/chat/completions)。
func (d *Grok) chatCompletions(c *gin.Context, m *grokModel, req *official.APIRequest) {
	client := d.webClient()
	if client == nil || !client.HasAccount() {
		apierrors.JSONError(c, 502, "api_error", "grok web client unavailable: missing GROK_COOKIES?", nil, "upstream_error")
		return
	}
	prompt := flattenChatInputAPI(req)
	streamReq := grokweb.CompletionRequest{Prompt: prompt}
	if req.Stream {
		d.chatCompletionsStream(c, req, client, streamReq)
		return
	}
	d.chatCompletionsNonStream(c, req, client, streamReq)
}

// chatCompletionsStream 流式 chat.completion.chunk。
func (d *Grok) chatCompletionsStream(c *gin.Context, req *official.APIRequest, client *grokweb.Client, streamReq grokweb.CompletionRequest) {
	model := req.Model
	if model == "" {
		model = "auto"
	}
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := c.Writer.(http.Flusher)
	writeChunk := func(chunk official.ChatCompletionChunk) {
		c.Writer.WriteString("data: " + chunk.String() + "\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
	roleChunk := official.NewChatCompletionChunk("", model)
	roleChunk.Choices[0].Delta.Role = "assistant"
	writeChunk(roleChunk)
	var fullText string
	_ = client.Complete(streamReq, func(delta grokweb.Delta) {
		if delta.Text == "" {
			return
		}
		fullText += delta.Text
		writeChunk(official.NewChatCompletionChunk(delta.Text, model))
	})
	// 流式完成后补 reasoning(如果有 Thinking 前缀)
	clean, reasoning := splitGrokThinking(fullText)
	if reasoning != "" {
		// 流式已发出原始文本,这里不做回溯;非流式才有完整分离。
		_ = clean
	}
	writeChunk(official.StopChunk("stop", model))
	c.Writer.WriteString("data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// chatCompletionsNonStream 非流式 ChatCompletion。
func (d *Grok) chatCompletionsNonStream(c *gin.Context, req *official.APIRequest, client *grokweb.Client, streamReq grokweb.CompletionRequest) {
	var fullText string
	res := client.Complete(streamReq, func(delta grokweb.Delta) { fullText += delta.Text })
	clean, reasoning := splitGrokThinking(fullText)
	if res.Err != "" && clean == "" && reasoning == "" {
		apierrors.JSONError(c, 502, "api_error", res.Err, nil, "upstream_error")
		return
	}
	outResp := official.NewChatCompletionWithMetadataAndReasoning(clean, reasoning, countMessagesChars(req.Messages), util.CountToken(clean), req.Model, "", nil)
	c.JSON(200, outResp)
}

// splitGrokThinking 剥离 Grok 的 "Thinking about your request" 前缀。
//
// Grok 把思考文本直接混在正文里,以固定前缀 "Thinking about your request" 开头,
// 但**没有明确的思考结束标记**(思考与正文无缝拼接)。因此这里只剥离前缀,
// 后续文本全部当正文(不做推理分离——无法可靠判定边界)。
func splitGrokThinking(text string) (body, reasoning string) {
	idx := strings.Index(text, "Thinking about your request")
	if idx < 0 {
		return text, ""
	}
	// 前缀之前的文本(若有)视为推理(可能含思考内容)
	prefix := strings.TrimSpace(text[:idx])
	rest := text[idx+len("Thinking about your request"):]
	return rest, prefix
}

var _ = fmt.Sprintf
