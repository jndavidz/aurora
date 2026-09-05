package provider

import (
	"net/http"

	"aurora/internal/apierrors"
	"aurora/internal/doubaoweb"
	"aurora/typings/official"
	"aurora/util"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// chatResponses 处理豆包 chat 变体(/v1/responses)。
// 豆包多轮靠 messages 全量回放(不靠服务端 conversation 记忆)。
func (d *Doubao) chatResponses(c *gin.Context, m *doubaoModel, req *official.ResponsesAPIRequest) {
	client := d.webClient()
	if client == nil || !client.HasAccount() {
		apierrors.JSONError(c, 502, "api_error", "doubao web client unavailable: missing DOUBAO_ACCOUNTS?", nil, "upstream_error")
		return
	}
	messages := doubaoMessagesFromResponses(req, true) // chat 剥离工具
	streamReq := doubaoweb.CompletionRequest{Messages: messages, Prompt: lastUserText(req)}
	if req.Stream {
		d.chatResponsesStream(c, req, client, streamReq)
		return
	}
	d.chatResponsesNonStream(c, req, client, streamReq)
}

// doubaoMessagesFromResponses 把 Responses input 转成豆包消息列表。
// stripTools 为 true 时跳过 function_call/function_call_output。
func doubaoMessagesFromResponses(req *official.ResponsesAPIRequest, stripTools bool) []doubaoweb.Message {
	items := responsesInputItems(req.Input)
	var msgs []doubaoweb.Message
	if instr := rawResponsesText(req.Instructions); instr != "" {
		msgs = append(msgs, doubaoweb.Message{Role: "user", Content: instr})
	}
	for _, it := range items {
		if stripTools && (it.Type == "function_call" || it.Type == "function_call_output") {
			continue
		}
		if it.Text == "" {
			continue
		}
		role := it.Role
		if role == "assistant" {
			msgs = append(msgs, doubaoweb.Message{Role: "assistant", Content: it.Text})
		} else {
			msgs = append(msgs, doubaoweb.Message{Role: "user", Content: it.Text})
		}
	}
	return msgs
}

// lastUserText 取最后一条用户消息文本(豆包新消息)。
func lastUserText(req *official.ResponsesAPIRequest) string {
	items := responsesInputItems(req.Input)
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Type == "message" && items[i].Role != "assistant" && items[i].Text != "" {
			return items[i].Text
		}
	}
	return ""
}

// chatResponsesStream 流式输出 Responses 事件。
func (d *Doubao) chatResponsesStream(c *gin.Context, req *official.ResponsesAPIRequest, client *doubaoweb.Client, streamReq doubaoweb.CompletionRequest) {
	w := newSSEWriter(c)
	respID := "resp_" + uuid.NewString()
	messageItemID := "msg_" + uuid.NewString()
	w.event("response.created", createdEvent(respID, req.Model))
	w.event("response.output_item.added", outputItemAddedEvent(0, map[string]any{"id": messageItemID, "type": "message", "status": "in_progress", "role": "assistant"}))

	var fullText string
	res := client.Complete(streamReq, func(delta doubaoweb.Delta) {
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
func (d *Doubao) chatResponsesNonStream(c *gin.Context, req *official.ResponsesAPIRequest, client *doubaoweb.Client, streamReq doubaoweb.CompletionRequest) {
	var fullText string
	res := client.Complete(streamReq, func(delta doubaoweb.Delta) { fullText += delta.Text })
	if res.Err != "" && fullText == "" {
		apierrors.JSONError(c, 502, "api_error", res.Err, nil, "upstream_error")
		return
	}
	outResp := official.NewResponsesResponse(fullText, "", countInputChars(req), util.CountToken(fullText), 0, 0, 0, req.Model)
	c.JSON(200, outResp)
}

// chatCompletions 处理豆包 chat 变体(/v1/chat/completions)。
func (d *Doubao) chatCompletions(c *gin.Context, m *doubaoModel, req *official.APIRequest) {
	client := d.webClient()
	if client == nil || !client.HasAccount() {
		apierrors.JSONError(c, 502, "api_error", "doubao web client unavailable: missing DOUBAO_ACCOUNTS?", nil, "upstream_error")
		return
	}
	messages := doubaoMessagesFromAPI(req)
	streamReq := doubaoweb.CompletionRequest{Messages: messages, Prompt: lastAPIMessageText(req)}
	if req.Stream {
		d.chatCompletionsStream(c, req, client, streamReq)
		return
	}
	d.chatCompletionsNonStream(c, req, client, streamReq)
}

// doubaoMessagesFromAPI 把 chat.completions messages 转成豆包消息。
func doubaoMessagesFromAPI(req *official.APIRequest) []doubaoweb.Message {
	var msgs []doubaoweb.Message
	for _, msg := range req.Messages {
		if t := msg.Text(); t != "" {
			if msg.Role == "assistant" {
				msgs = append(msgs, doubaoweb.Message{Role: "assistant", Content: t})
			} else {
				msgs = append(msgs, doubaoweb.Message{Role: "user", Content: t})
			}
		}
	}
	return msgs
}

// lastAPIMessageText 取最后一条用户消息。
func lastAPIMessageText(req *official.APIRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role != "assistant" {
			if t := req.Messages[i].Text(); t != "" {
				return t
			}
		}
	}
	return ""
}

// chatCompletionsStream 流式 chat.completion.chunk。
func (d *Doubao) chatCompletionsStream(c *gin.Context, req *official.APIRequest, client *doubaoweb.Client, streamReq doubaoweb.CompletionRequest) {
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
	_ = client.Complete(streamReq, func(delta doubaoweb.Delta) {
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

// chatCompletionsNonStream 非流式。
func (d *Doubao) chatCompletionsNonStream(c *gin.Context, req *official.APIRequest, client *doubaoweb.Client, streamReq doubaoweb.CompletionRequest) {
	var fullText string
	res := client.Complete(streamReq, func(delta doubaoweb.Delta) { fullText += delta.Text })
	if res.Err != "" && fullText == "" {
		apierrors.JSONError(c, 502, "api_error", res.Err, nil, "upstream_error")
		return
	}
	outResp := official.NewChatCompletionWithMetadataAndReasoning(fullText, "", countMessagesChars(req.Messages), util.CountToken(fullText), req.Model, "", nil)
	c.JSON(200, outResp)
}
