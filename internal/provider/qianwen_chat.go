package provider

import (
	"fmt"
	"net/http"

	"aurora/internal/apierrors"
	"aurora/internal/qianwenweb"
	"aurora/typings/official"
	"aurora/util"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// qianwenChatResponses 处理千问(/v1/responses)。
// 千问网页 API 不支持工具调用,纯对话:全量历史 + 每轮新会话(first_turn)。
func (d *Qianwen) qianwenChatResponses(c *gin.Context, req *official.ResponsesAPIRequest) {
	streamReq := qianwenweb.ChatRequest{
		Model:    upstreamSlug(req.Model),
		Messages: qianwenMessagesFromResponses(req),
	}
	if req.Stream {
		d.qianwenChatResponsesStream(c, req, streamReq)
		return
	}
	d.qianwenChatResponsesNonStream(c, req, streamReq)
}

// qianwenRequest 用当前池 cookie 发起 chat;失败则轮换池内其它 cookie header 重试。
// (千问凭据直接可用,无 GLM 式 refresh 换发,故轮换挂在请求失败路径上。)
func (d *Qianwen) qianwenRequest(c *gin.Context, client *qianwenweb.Client, streamReq qianwenweb.ChatRequest) (*http.Response, error) {
	if client == nil || !client.HasToken() {
		return nil, fmt.Errorf("qianwen web client unavailable: missing QIANWEN_WEB_TOKENS?")
	}
	for attempt := 0; attempt < client.PoolSize(); attempt++ {
		cookieHeader := client.NextToken()
		if cookieHeader == "" || (attempt > 0 && cookieHeader == d.lastCookie) {
			break
		}
		client.SetCookieHeader(cookieHeader)
		d.lastCookie = cookieHeader
		resp, err := client.Complete(streamReq)
		if err == nil {
			return resp, nil
		}
	}
	return nil, fmt.Errorf("qianwen: all cookie headers failed")
}

// qianwenMessagesFromResponses 把 Responses input 转成千问 messages(纯对话,剥离工具 item)。
func qianwenMessagesFromResponses(req *official.ResponsesAPIRequest) []qianwenweb.Message {
	items := responsesInputItems(req.Input)
	var msgs []qianwenweb.Message
	// instructions → 第一条 user 消息(千问无 system role)
	if instr := rawResponsesText(req.Instructions); instr != "" {
		msgs = append(msgs, qianwenweb.UserMsg(instr))
	}
	for _, it := range items {
		if it.Type == "function_call" || it.Type == "function_call_output" {
			continue // 千问不支持工具,剥离
		}
		role := it.Role
		if role == "" || role == "system" {
			role = "user"
		}
		if it.Text == "" {
			continue
		}
		if role == "assistant" {
			msgs = append(msgs, qianwenweb.AssistantMsg(it.Text))
		} else {
			msgs = append(msgs, qianwenweb.UserMsg(it.Text))
		}
	}
	if len(msgs) == 0 {
		msgs = append(msgs, qianwenweb.UserMsg(""))
	}
	return msgs
}

// qianwenMessagesFromAPI 把 chat.completions 的 messages 转成千问 messages。
func qianwenMessagesFromAPI(req *official.APIRequest) []qianwenweb.Message {
	var msgs []qianwenweb.Message
	for _, msg := range req.Messages {
		role := msg.Role
		if role == "system" {
			role = "user" // 千问无 system role
		}
		if role == "tool" || role == "function" {
			continue // 千问不支持工具消息
		}
		if t := msg.Text(); t != "" {
			if role == "assistant" {
				msgs = append(msgs, qianwenweb.AssistantMsg(t))
			} else {
				msgs = append(msgs, qianwenweb.UserMsg(t))
			}
		}
	}
	if len(msgs) == 0 {
		msgs = append(msgs, qianwenweb.UserMsg(""))
	}
	return msgs
}

// qianwenChatResponsesStream 流式输出 Responses 事件。
func (d *Qianwen) qianwenChatResponsesStream(c *gin.Context, req *official.ResponsesAPIRequest, streamReq qianwenweb.ChatRequest) {
	client := d.webClient()
	resp, err := d.qianwenRequest(c, client, streamReq)
	if err != nil {
		apierrors.JSONError(c, 502, "api_error", err.Error(), nil, "upstream_error")
		return
	}
	defer resp.Body.Close()

	w := newSSEWriter(c)
	respID := "resp_" + uuid.NewString()
	messageItemID := "msg_" + uuid.NewString()

	w.event("response.created", createdEvent(respID, req.Model))
	w.event("response.output_item.added", outputItemAddedEvent(0, map[string]any{"id": messageItemID, "type": "message", "status": "in_progress", "role": "assistant"}))

	var fullText string
	res := qianwenweb.ConsumeStream(resp.Body, func(delta qianwenweb.Delta) {
		if delta.Text != "" {
			fullText += delta.Text
			w.event("response.output_text.delta", map[string]any{
				"type": "response.output_text.delta", "item_id": messageItemID,
				"output_index": 0, "content_index": 0, "delta": delta.Text,
			})
		}
	})
	if res.Err != "" && fullText == "" {
		w.event("response.failed", failedEvent(res.Err))
		return
	}
	w.event("response.output_item.done", outputItemDoneEvent(0, messageItem(messageItemID, fullText, "completed")))
	outResp := official.NewResponsesResponse(fullText, "", countInputChars(req), util.CountToken(fullText), 0, 0, 0, req.Model)
	w.event("response.completed", completedEvent(outResp))
}

// qianwenChatResponsesNonStream 非流式。
func (d *Qianwen) qianwenChatResponsesNonStream(c *gin.Context, req *official.ResponsesAPIRequest, streamReq qianwenweb.ChatRequest) {
	client := d.webClient()
	resp, err := d.qianwenRequest(c, client, streamReq)
	if err != nil {
		apierrors.JSONError(c, 502, "api_error", err.Error(), nil, "upstream_error")
		return
	}
	defer resp.Body.Close()
	var fullText string
	res := qianwenweb.ConsumeStream(resp.Body, func(delta qianwenweb.Delta) {
		fullText += delta.Text
	})
	if res.Err != "" && fullText == "" {
		apierrors.JSONError(c, 502, "api_error", res.Err, nil, "upstream_error")
		return
	}
	outResp := official.NewResponsesResponse(fullText, "", countInputChars(req), util.CountToken(fullText), 0, 0, 0, req.Model)
	c.JSON(200, outResp)
}

// qianwenChatCompletions 处理千问(/v1/chat/completions)。
func (d *Qianwen) qianwenChatCompletions(c *gin.Context, req *official.APIRequest) {
	streamReq := qianwenweb.ChatRequest{
		Model:    upstreamSlug(req.Model),
		Messages: qianwenMessagesFromAPI(req),
	}
	if req.Stream {
		d.qianwenChatCompletionsStream(c, req, streamReq)
		return
	}
	d.qianwenChatCompletionsNonStream(c, req, streamReq)
}

// qianwenChatCompletionsStream 流式 chat.completion.chunk。
func (d *Qianwen) qianwenChatCompletionsStream(c *gin.Context, req *official.APIRequest, streamReq qianwenweb.ChatRequest) {
	client := d.webClient()
	resp, err := d.qianwenRequest(c, client, streamReq)
	if err != nil {
		apierrors.JSONError(c, 502, "api_error", err.Error(), nil, "upstream_error")
		return
	}
	defer resp.Body.Close()

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
	_ = qianwenweb.ConsumeStream(resp.Body, func(delta qianwenweb.Delta) {
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

// qianwenChatCompletionsNonStream 非流式 ChatCompletion。
func (d *Qianwen) qianwenChatCompletionsNonStream(c *gin.Context, req *official.APIRequest, streamReq qianwenweb.ChatRequest) {
	client := d.webClient()
	resp, err := d.qianwenRequest(c, client, streamReq)
	if err != nil {
		apierrors.JSONError(c, 502, "api_error", err.Error(), nil, "upstream_error")
		return
	}
	defer resp.Body.Close()
	var fullText string
	res := qianwenweb.ConsumeStream(resp.Body, func(delta qianwenweb.Delta) {
		fullText += delta.Text
	})
	if res.Err != "" && fullText == "" {
		apierrors.JSONError(c, 502, "api_error", res.Err, nil, "upstream_error")
		return
	}
	outResp := official.NewChatCompletionWithMetadataAndReasoning(fullText, "", countMessagesChars(req.Messages), util.CountToken(fullText), req.Model, "", nil)
	c.JSON(200, outResp)
}
