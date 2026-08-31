package provider

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"aurora/internal/glmweb"
	"aurora/typings/official"
	"aurora/util"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// glmRefreshSkew 是 access_token 的提前换发余量(GLM access ~2h)。
// 距过期不足 10 分钟即视为需要换发,避免拿到临界票打到一半过期。
const glmRefreshSkew = 10 * time.Minute

// chatResponses 处理智谱 chat 变体(/v1/responses)。
//
// 与 DeepSeek 差异:智谱用 messages 数组 + conversation_id 服务端会话
// (非拍平 prompt)。chat 变体剥离 tools,纯对话 + 深度思考/联网开关。
func (d *Glm) chatResponses(c *gin.Context, m *glmModel, req *official.ResponsesAPIRequest) {
	client := d.webClient()
	if err := d.ensureToken(client); err != nil {
		c.JSON(502, gin.H{"error": err.Error()})
		return
	}

	messages := glmMessagesFromResponses(req, true) // chat 变体剥离 tools
	streamReq := glmweb.CompletionRequest{
		Messages:     messages,
		ChatMode:     m.Mode, // "speed"(快速,默认)|"thinking"(思考),由模型 id 挡位决定
		IsNetworking: true,
	}
	if req.Stream {
		d.chatResponsesStream(c, req, client, streamReq)
		return
	}
	d.chatResponsesNonStream(c, req, client, streamReq)
}

// ensureToken 确保有有效 access_token(必要时换发;换发失败轮询下一个池 token)。
func (d *Glm) ensureToken(client *glmweb.Client) error {
	if client == nil || !client.HasToken() {
		return fmt.Errorf("glm web client unavailable: missing GLM_WEB_TOKENS?")
	}
	if client.HasAccessToken() && !client.AccessTokenNearExpiry(glmRefreshSkew) {
		return nil
	}
	for attempt := 0; attempt <= client.PoolSize(); attempt++ {
		token := client.NextToken()
		if token == "" || (attempt > 0 && token == d.lastToken) {
			break
		}
		client.SetRefreshToken(token)
		d.lastToken = token
		if err := client.RefreshAccessToken(); err == nil {
			return nil
		}
	}
	return fmt.Errorf("glm: all refresh tokens failed")
}

// completeWithAuth 调 client.Complete;请求失败即清票(401/403 或传输错误),
// 下一次请求经 ensureToken 重换发 —— 修复"access_token 过期后拿废票打到进程重启"的短路。
// 对传输类瞬时错误多清一次票的代价只是一次额外换发(纯 HTTP,毫秒级),可接受。
func (d *Glm) completeWithAuth(client *glmweb.Client, req glmweb.CompletionRequest) (*http.Response, error) {
	resp, err := client.Complete(req)
	if err != nil {
		client.ClearAccessToken()
	}
	return resp, err
}

// glmMessagesFromResponses 把 Responses input 转成智谱 messages(chat 变体剥离 tools)。
func glmMessagesFromResponses(req *official.ResponsesAPIRequest, stripTools bool) []glmweb.Message {
	items := responsesInputItems(req.Input)
	var msgs []glmweb.Message
	// instructions → 第一条 system 消息
	if instr := rawResponsesText(req.Instructions); instr != "" {
		msgs = append(msgs, glmweb.Message{Role: "user", Content: []glmweb.Content{{Type: "text", Text: instr}}})
	}
	for _, it := range items {
		if stripTools && (it.Type == "function_call" || it.Type == "function_call_output") {
			continue
		}
		role := it.Role
		if role == "" || role == "system" {
			role = "user" // 智谱无 system role,归入 user
		}
		if it.Text == "" {
			continue
		}
		msgs = append(msgs, glmweb.Message{Role: role, Content: []glmweb.Content{{Type: "text", Text: it.Text}}})
	}
	if len(msgs) == 0 {
		msgs = append(msgs, glmweb.Message{Role: "user", Content: []glmweb.Content{{Type: "text", Text: ""}}})
	}
	return msgs
}

// chatResponsesStream 流式输出 Responses 事件。
func (d *Glm) chatResponsesStream(c *gin.Context, req *official.ResponsesAPIRequest, client *glmweb.Client, streamReq glmweb.CompletionRequest) {
	resp, err := d.completeWithAuth(client, streamReq)
	if err != nil {
		c.JSON(502, gin.H{"error": err.Error()})
		return
	}
	defer resp.Body.Close()

	w := newSSEWriter(c)
	respID := "resp_" + uuid.NewString()
	reasoningItemID := "rs_" + uuid.NewString()
	messageItemID := "msg_" + uuid.NewString()

	w.event("response.created", createdEvent(respID, req.Model))
	w.event("response.output_item.added", outputItemAddedEvent(0, map[string]any{"id": reasoningItemID, "type": "reasoning", "status": "in_progress"}))
	w.event("response.output_item.added", outputItemAddedEvent(1, map[string]any{"id": messageItemID, "type": "message", "status": "in_progress", "role": "assistant"}))

	var fullText, fullReasoning string
	res := glmweb.ConsumeStream(resp.Body, func(delta glmweb.Delta) {
		if delta.Reasoning != "" {
			fullReasoning += delta.Reasoning
			w.event("response.reasoning_text.delta", map[string]any{
				"type": "response.reasoning_text.delta", "item_id": reasoningItemID,
				"output_index": 0, "content_index": 0, "delta": delta.Reasoning,
			})
		}
		if delta.Text != "" {
			fullText += delta.Text
			w.event("response.output_text.delta", map[string]any{
				"type": "response.output_text.delta", "item_id": messageItemID,
				"output_index": 1, "content_index": 0, "delta": delta.Text,
			})
		}
	})
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
func (d *Glm) chatResponsesNonStream(c *gin.Context, req *official.ResponsesAPIRequest, client *glmweb.Client, streamReq glmweb.CompletionRequest) {
	resp, err := d.completeWithAuth(client, streamReq)
	if err != nil {
		c.JSON(502, gin.H{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	var fullText, fullReasoning string
	res := glmweb.ConsumeStream(resp.Body, func(delta glmweb.Delta) {
		fullText += delta.Text
		fullReasoning += delta.Reasoning
	})
	if res.Err != "" && fullText == "" && fullReasoning == "" {
		c.JSON(502, gin.H{"error": res.Err})
		return
	}
	outResp := official.NewResponsesResponse(fullText, fullReasoning, countInputChars(req), util.CountToken(fullText), util.CountToken(fullReasoning), 0, 0, req.Model)
	c.JSON(200, outResp)
}

// chatCompletions 处理智谱 chat 变体(/v1/chat/completions)。
func (d *Glm) chatCompletions(c *gin.Context, m *glmModel, req *official.APIRequest) {
	client := d.webClient()
	if err := d.ensureToken(client); err != nil {
		c.JSON(502, gin.H{"error": err.Error()})
		return
	}
	messages := glmMessagesFromAPI(req)
	streamReq := glmweb.CompletionRequest{
		Messages:     messages,
		ChatMode:     m.Mode, // "speed"(快速,默认)|"thinking"(思考),由模型 id 挡位决定
		IsNetworking: true,
	}
	if req.Stream {
		d.chatCompletionsStream(c, req, client, streamReq)
		return
	}
	d.chatCompletionsNonStream(c, req, client, streamReq)
}

// glmMessagesFromAPI 把 chat.completions messages 转成智谱 messages。
func glmMessagesFromAPI(req *official.APIRequest) []glmweb.Message {
	var msgs []glmweb.Message
	for _, msg := range req.Messages {
		role := msg.Role
		if role == "system" {
			role = "user" // 智谱无 system role
		}
		if t := msg.Text(); t != "" {
			msgs = append(msgs, glmweb.Message{Role: role, Content: []glmweb.Content{{Type: "text", Text: t}}})
		}
	}
	if len(msgs) == 0 {
		msgs = append(msgs, glmweb.Message{Role: "user", Content: []glmweb.Content{{Type: "text", Text: ""}}})
	}
	return msgs
}

// chatCompletionsStream 流式 chat.completion.chunk。
func (d *Glm) chatCompletionsStream(c *gin.Context, req *official.APIRequest, client *glmweb.Client, streamReq glmweb.CompletionRequest) {
	resp, err := d.completeWithAuth(client, streamReq)
	if err != nil {
		c.JSON(502, gin.H{"error": err.Error()})
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
	_ = glmweb.ConsumeStream(resp.Body, func(delta glmweb.Delta) {
		if delta.Reasoning != "" {
			writeChunk(official.NewReasoningChunk(delta.Reasoning, model))
		}
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
func (d *Glm) chatCompletionsNonStream(c *gin.Context, req *official.APIRequest, client *glmweb.Client, streamReq glmweb.CompletionRequest) {
	resp, err := d.completeWithAuth(client, streamReq)
	if err != nil {
		c.JSON(502, gin.H{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	var fullText, fullReasoning string
	res := glmweb.ConsumeStream(resp.Body, func(delta glmweb.Delta) {
		fullText += delta.Text
		fullReasoning += delta.Reasoning
	})
	if res.Err != "" && fullText == "" && fullReasoning == "" {
		c.JSON(502, gin.H{"error": res.Err})
		return
	}
	outResp := official.NewChatCompletionWithMetadataAndReasoning(fullText, fullReasoning, countMessagesChars(req.Messages), util.CountToken(fullText), req.Model, "", nil)
	c.JSON(200, outResp)
}

var _ = strings.TrimSpace
