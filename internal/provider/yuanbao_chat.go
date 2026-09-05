package provider

import (
	"fmt"
	"net/http"

	"aurora/internal/apierrors"
	"aurora/internal/yuanbaoweb"
	"aurora/typings/official"
	"aurora/util"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// yuanbaoChatResponses 处理元宝 chat 变体(/v1/responses)。
//
// 硬规则(与 DeepSeek chat 相同):上游只发「真人对话」形态 —— 剥离客户端 tools,
// prompt 用 flattenChatItems 拍平纯文本;仅携带网页模型开关(自动联网搜索)。
func (d *Yuanbao) yuanbaoChatResponses(c *gin.Context, m *yuanbaoModel, req *official.ResponsesAPIRequest) {
	prompt := flattenChatItems(responsesInputItems(req.Input), rawResponsesText(req.Instructions))
	streamReq := yuanbaoweb.ChatRequest{
		ChatModelID: m.ChatModelID,
		Prompt:      prompt,
		WebSearch:   true, // 网页默认开启自动联网搜索
	}
	if req.Stream {
		d.yuanbaoChatResponsesStream(c, m, req, streamReq)
		return
	}
	d.yuanbaoChatResponsesNonStream(c, m, req, streamReq)
}

// yuanbaoChatCompletions 处理元宝 chat 变体(/v1/chat/completions)。
func (d *Yuanbao) yuanbaoChatCompletions(c *gin.Context, m *yuanbaoModel, req *official.APIRequest) {
	prompt := flattenChatItems(apiMessagesToItems(req.Messages), "")
	streamReq := yuanbaoweb.ChatRequest{
		ChatModelID: m.ChatModelID,
		Prompt:      prompt,
		WebSearch:   true,
	}
	if req.Stream {
		d.yuanbaoChatCompletionsStream(c, m, req, streamReq)
		return
	}
	d.yuanbaoChatCompletionsNonStream(c, m, req, streamReq)
}

// yuanbaoRequest 用当前池凭据发起 chat;失败则轮换池内其它凭据重试。
func (d *Yuanbao) yuanbaoRequest(c *gin.Context, client *yuanbaoweb.Client, streamReq yuanbaoweb.ChatRequest) (*http.Response, error) {
	if client == nil || !client.HasToken() {
		return nil, fmt.Errorf("yuanbao web client unavailable: missing YUANBAO_WEB_TOKENS?")
	}
	for attempt := 0; attempt < client.PoolSize(); attempt++ {
		uskey, cookie := client.NextToken()
		cred := uskey + "\x00" + cookie
		if cred == "" || (attempt > 0 && cred == d.lastCred) {
			break
		}
		client.SetCredential(uskey, cookie)
		d.lastCred = cred
		resp, err := client.Chat(streamReq)
		if err == nil {
			return resp, nil
		}
	}
	return nil, fmt.Errorf("yuanbao: all credentials failed")
}

// yuanbaoChatResponsesStream 流式输出 Responses 事件。
func (d *Yuanbao) yuanbaoChatResponsesStream(c *gin.Context, m *yuanbaoModel, req *official.ResponsesAPIRequest, streamReq yuanbaoweb.ChatRequest) {
	client := d.webClient()
	resp, err := d.yuanbaoRequest(c, client, streamReq)
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
	res := yuanbaoweb.ConsumeStream(resp.Body, func(delta yuanbaoweb.Delta) {
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

// yuanbaoChatResponsesNonStream 非流式。
func (d *Yuanbao) yuanbaoChatResponsesNonStream(c *gin.Context, m *yuanbaoModel, req *official.ResponsesAPIRequest, streamReq yuanbaoweb.ChatRequest) {
	client := d.webClient()
	resp, err := d.yuanbaoRequest(c, client, streamReq)
	if err != nil {
		apierrors.JSONError(c, 502, "api_error", err.Error(), nil, "upstream_error")
		return
	}
	defer resp.Body.Close()
	var fullText string
	res := yuanbaoweb.ConsumeStream(resp.Body, func(delta yuanbaoweb.Delta) {
		fullText += delta.Text
	})
	if res.Err != "" && fullText == "" {
		apierrors.JSONError(c, 502, "api_error", res.Err, nil, "upstream_error")
		return
	}
	outResp := official.NewResponsesResponse(fullText, "", countInputChars(req), util.CountToken(fullText), 0, 0, 0, req.Model)
	c.JSON(200, outResp)
}

// yuanbaoChatCompletionsStream 流式 chat.completion.chunk。
func (d *Yuanbao) yuanbaoChatCompletionsStream(c *gin.Context, m *yuanbaoModel, req *official.APIRequest, streamReq yuanbaoweb.ChatRequest) {
	client := d.webClient()
	resp, err := d.yuanbaoRequest(c, client, streamReq)
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
	_ = yuanbaoweb.ConsumeStream(resp.Body, func(delta yuanbaoweb.Delta) {
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

// yuanbaoChatCompletionsNonStream 非流式 ChatCompletion。
func (d *Yuanbao) yuanbaoChatCompletionsNonStream(c *gin.Context, m *yuanbaoModel, req *official.APIRequest, streamReq yuanbaoweb.ChatRequest) {
	client := d.webClient()
	resp, err := d.yuanbaoRequest(c, client, streamReq)
	if err != nil {
		apierrors.JSONError(c, 502, "api_error", err.Error(), nil, "upstream_error")
		return
	}
	defer resp.Body.Close()
	var fullText string
	res := yuanbaoweb.ConsumeStream(resp.Body, func(delta yuanbaoweb.Delta) {
		fullText += delta.Text
	})
	if res.Err != "" && fullText == "" {
		apierrors.JSONError(c, 502, "api_error", res.Err, nil, "upstream_error")
		return
	}
	outResp := official.NewChatCompletionWithMetadataAndReasoning(fullText, "", countMessagesChars(req.Messages), util.CountToken(fullText), req.Model, "", nil)
	c.JSON(200, outResp)
}
