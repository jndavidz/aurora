package provider

import (
	"net/http"

	"aurora/internal/apierrors"
	"aurora/internal/geminweb"
	"aurora/typings/official"
	"aurora/util"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// chatResponses 处理 Gemini chat 变体(/v1/responses)。
//
// Gemini 网页版模型自动使用原生搜索/地图等能力,chat 变体只需纯文本对话。
// 多轮:首轮返回 rcid,后续请求用 PrevRCID 续聊。
func (d *Gemini) chatResponses(c *gin.Context, m *geminiModel, req *official.ResponsesAPIRequest) {
	client := d.webClient()
	if client == nil || !client.HasAccount() {
		apierrors.JSONError(c, 502, "api_error", "gemini web client unavailable: missing GEMINI_ACCOUNTS?", nil, "upstream_error")
		return
	}
	prompt := flattenChatInput(req, true) // chat 变体剥离 tools
	streamReq := geminweb.CompletionRequest{Prompt: prompt}
	if req.Stream {
		d.chatResponsesStream(c, req, client, streamReq)
		return
	}
	d.chatResponsesNonStream(c, req, client, streamReq)
}

// chatResponsesStream 流式输出 Responses 事件。
func (d *Gemini) chatResponsesStream(c *gin.Context, req *official.ResponsesAPIRequest, client *geminweb.Client, streamReq geminweb.CompletionRequest) {
	w := newSSEWriter(c)
	respID := "resp_" + uuid.NewString()
	messageItemID := "msg_" + uuid.NewString()
	w.event("response.created", createdEvent(respID, req.Model))
	w.event("response.output_item.added", outputItemAddedEvent(0, map[string]any{"id": messageItemID, "type": "message", "status": "in_progress", "role": "assistant"}))

	var fullText string
	res := client.Complete(streamReq, func(delta geminweb.Delta) {
		if delta.Text == "" {
			return
		}
		fullText += delta.Text
		w.event("response.output_text.delta", map[string]any{
			"type": "response.output_text.delta", "item_id": messageItemID,
			"output_index": 0, "content_index": 0, "delta": delta.Text,
		})
	})
	if res.Err != "" && fullText == "" {
		w.event("response.failed", failedEvent(res.Err))
		return
	}
	w.event("response.output_item.done", outputItemDoneEvent(0, messageItem(messageItemID, fullText, "completed")))
	outResp := official.NewResponsesResponse(fullText, "", countInputChars(req), util.CountToken(fullText), 0, 0, 0, req.Model)
	w.event("response.completed", completedEvent(outResp))
}

// chatResponsesNonStream 非流式。
func (d *Gemini) chatResponsesNonStream(c *gin.Context, req *official.ResponsesAPIRequest, client *geminweb.Client, streamReq geminweb.CompletionRequest) {
	var fullText string
	res := client.Complete(streamReq, func(delta geminweb.Delta) { fullText += delta.Text })
	if res.Err != "" && fullText == "" {
		apierrors.JSONError(c, 502, "api_error", res.Err, nil, "upstream_error")
		return
	}
	outResp := official.NewResponsesResponse(fullText, "", countInputChars(req), util.CountToken(fullText), 0, 0, 0, req.Model)
	c.JSON(200, outResp)
}

// chatCompletions 处理 Gemini chat 变体(/v1/chat/completions)。
func (d *Gemini) chatCompletions(c *gin.Context, m *geminiModel, req *official.APIRequest) {
	client := d.webClient()
	if client == nil || !client.HasAccount() {
		apierrors.JSONError(c, 502, "api_error", "gemini web client unavailable: missing GEMINI_ACCOUNTS?", nil, "upstream_error")
		return
	}
	prompt := flattenChatInputAPI(req)
	streamReq := geminweb.CompletionRequest{Prompt: prompt}
	if req.Stream {
		d.chatCompletionsStream(c, req, client, streamReq)
		return
	}
	d.chatCompletionsNonStream(c, req, client, streamReq)
}

// chatCompletionsStream 流式 chat.completion.chunk。
func (d *Gemini) chatCompletionsStream(c *gin.Context, req *official.APIRequest, client *geminweb.Client, streamReq geminweb.CompletionRequest) {
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
	_ = client.Complete(streamReq, func(delta geminweb.Delta) {
		if delta.Text != "" {
			writeChunk(official.NewChatCompletionChunk(delta.Text, model))
		}
	})
	writeChunk(official.StopChunk("stop", model))
	c.Writer.WriteString("data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// chatCompletionsNonStream 非流式 ChatCompletion。
func (d *Gemini) chatCompletionsNonStream(c *gin.Context, req *official.APIRequest, client *geminweb.Client, streamReq geminweb.CompletionRequest) {
	var fullText string
	res := client.Complete(streamReq, func(delta geminweb.Delta) { fullText += delta.Text })
	if res.Err != "" && fullText == "" {
		apierrors.JSONError(c, 502, "api_error", res.Err, nil, "upstream_error")
		return
	}
	outResp := official.NewChatCompletionWithMetadataAndReasoning(fullText, "", countMessagesChars(req.Messages), util.CountToken(fullText), req.Model, "", nil)
	c.JSON(200, outResp)
}
