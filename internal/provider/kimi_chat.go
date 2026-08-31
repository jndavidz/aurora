package provider

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"aurora/internal/kimiweb"
	"aurora/typings/official"
	"aurora/util"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// kimiRefreshSkew 是 access_token 的提前换发余量(Kimi access 仅 ~15 分钟)。
// 距过期不足 3 分钟即视为需要换发。
const kimiRefreshSkew = 3 * time.Minute

// kimiSearchTool 是联网搜索的原生工具(chat 变体默认开启,同网页端;正文里的
// 🛠...🛠 引用标记由 kimiweb.ConsumeStream 剥离)。
var kimiSearchTool = []kimiweb.Tool{{Type: "TOOL_TYPE_SEARCH", Search: map[string]any{}}}

// chatResponses 处理 Kimi chat 变体(/v1/responses)。
//
// 与 GLM/DeepSeek 差异:Kimi Chat RPC 只收单条 message(singular),不认 messages
// 数组,上下文靠服务端 chat_id 会话。aurora 是无状态架构,因此把全量历史
// **拍平进单条用户消息文本**(实测有效,见 docs/KIMI.md §三)。chat 变体剥离 tools。
func (d *Kimi) chatResponses(c *gin.Context, m *kimiModel, req *official.ResponsesAPIRequest) {
	client := d.webClient()
	if err := d.ensureToken(client); err != nil {
		c.JSON(502, gin.H{"error": err.Error()})
		return
	}

	text := kimiFlattenResponses(req, true) // chat 变体剥离 tools
	streamReq := kimiweb.CompletionRequest{
		Text:     text,
		Thinking: true,
		Tools:    kimiSearchTool, // 开启联网搜索(快速模式 K2.6)
	}
	if req.Stream {
		d.chatResponsesStream(c, req, client, streamReq)
		return
	}
	d.chatResponsesNonStream(c, req, client, streamReq)
}

// ensureToken 确保有有效 access_token(必要时换发;换发失败轮询下一个池 token)。
func (d *Kimi) ensureToken(client *kimiweb.Client) error {
	d.tokMu.Lock()
	defer d.tokMu.Unlock()
	if client == nil || !client.HasToken() {
		return fmt.Errorf("kimi web client unavailable: missing KIMI_WEB_TOKENS?")
	}
	if client.HasAccessToken() && !client.AccessTokenNearExpiry(kimiRefreshSkew) {
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
	return fmt.Errorf("kimi: all refresh tokens failed")
}

// completeWithAuth 调 client.Complete;请求失败即清票(401/403 或传输错误),
// 下一次请求经 ensureToken 重换发 —— 同 glm 侧,修复过期票短路。
// Kimi 换发走 c.mu 互斥,并发安全。
func (d *Kimi) completeWithAuth(client *kimiweb.Client, req kimiweb.CompletionRequest) (*http.Response, error) {
	resp, err := client.Complete(req)
	if err != nil {
		client.ClearAccessToken()
	}
	return resp, err
}

// kimiFlattenResponses 把 Responses input 拍平成单条用户消息文本。
// stripTools=true(chat)时跳过 function_call/function_call_output。
// 格式:指令在前,随后是角色前缀(用户/助手)的对话文本。
func kimiFlattenResponses(req *official.ResponsesAPIRequest, stripTools bool) string {
	var sb strings.Builder
	if instr := rawResponsesText(req.Instructions); instr != "" {
		sb.WriteString(instr)
		sb.WriteString("\n")
	}
	kimiFlattenItems(&sb, responsesInputItems(req.Input), stripTools)
	return strings.TrimRight(sb.String(), "\n")
}

// kimiFlattenItems 把拍平后的 item 列表按角色前缀写入 sb。
func kimiFlattenItems(sb *strings.Builder, items []responsesInputItem, stripTools bool) {
	for _, it := range items {
		if stripTools && (it.Type == "function_call" || it.Type == "function_call_output") {
			continue
		}
		if it.Text == "" {
			continue
		}
		role := it.Role
		if role == "" || role == "system" {
			role = "user"
		}
		sb.WriteString(kimiRoleLabel(role))
		sb.WriteString(": ")
		sb.WriteString(it.Text)
		sb.WriteString("\n")
	}
}

// kimiRoleLabel 角色中文标签(模型中文训练,实测该格式可用)。
func kimiRoleLabel(role string) string {
	switch role {
	case "assistant":
		return "助手"
	case "tool", "function":
		return "工具结果"
	default:
		return "用户"
	}
}

// chatResponsesStream 流式输出 Responses 事件。
func (d *Kimi) chatResponsesStream(c *gin.Context, req *official.ResponsesAPIRequest, client *kimiweb.Client, streamReq kimiweb.CompletionRequest) {
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
	res := kimiweb.ConsumeStream(resp.Body, func(delta kimiweb.Delta) {
		// chat 变体:原生工具调用(ipython 等)不外露,模型已把结果折进正文。
		if delta.ToolCall != nil {
			return
		}
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
func (d *Kimi) chatResponsesNonStream(c *gin.Context, req *official.ResponsesAPIRequest, client *kimiweb.Client, streamReq kimiweb.CompletionRequest) {
	resp, err := d.completeWithAuth(client, streamReq)
	if err != nil {
		c.JSON(502, gin.H{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	var fullText, fullReasoning string
	res := kimiweb.ConsumeStream(resp.Body, func(delta kimiweb.Delta) {
		if delta.ToolCall != nil {
			return
		}
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

// chatCompletions 处理 Kimi chat 变体(/v1/chat/completions)。
func (d *Kimi) chatCompletions(c *gin.Context, m *kimiModel, req *official.APIRequest) {
	client := d.webClient()
	if err := d.ensureToken(client); err != nil {
		c.JSON(502, gin.H{"error": err.Error()})
		return
	}
	text := kimiFlattenMessages(req, true) // chat 变体剥离 tools
	streamReq := kimiweb.CompletionRequest{
		Text:     text,
		Thinking: true,
		Tools:    kimiSearchTool, // 开启联网搜索(快速模式 K2.6)
	}
	if req.Stream {
		d.chatCompletionsStream(c, req, client, streamReq)
		return
	}
	d.chatCompletionsNonStream(c, req, client, streamReq)
}

// kimiFlattenMessages 把 chat.completions messages 拍平成单条用户消息文本。
func kimiFlattenMessages(req *official.APIRequest, stripTools bool) string {
	var sb strings.Builder
	kimiFlattenAPIMessages(&sb, req.Messages, stripTools)
	return strings.TrimRight(sb.String(), "\n")
}

// kimiFlattenAPIMessages 把 messages 按角色前缀写入 sb。
func kimiFlattenAPIMessages(sb *strings.Builder, messages []official.APIMessage, stripTools bool) {
	for _, msg := range messages {
		role := msg.Role
		if role == "system" {
			role = "user"
		}
		// assistant tool_calls / tool role:stripTools=true 时跳过
		if stripTools && (len(msg.ToolCalls) > 0 || role == "tool" || role == "function") {
			continue
		}
		if len(msg.ToolCalls) > 0 {
			for _, tc := range msg.ToolCalls {
				sb.WriteString("助手: ")
				sb.WriteString(tc.Function.Arguments)
				sb.WriteString("\n")
			}
			continue
		}
		if role == "tool" || role == "function" {
			sb.WriteString("工具结果: ")
			sb.WriteString(msg.Text())
			sb.WriteString("\n")
			continue
		}
		if t := msg.Text(); t != "" {
			sb.WriteString(kimiRoleLabel(role))
			sb.WriteString(": ")
			sb.WriteString(t)
			sb.WriteString("\n")
		}
	}
}

// chatCompletionsStream 流式 chat.completion.chunk。
func (d *Kimi) chatCompletionsStream(c *gin.Context, req *official.APIRequest, client *kimiweb.Client, streamReq kimiweb.CompletionRequest) {
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
	_ = kimiweb.ConsumeStream(resp.Body, func(delta kimiweb.Delta) {
		if delta.ToolCall != nil {
			return
		}
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
func (d *Kimi) chatCompletionsNonStream(c *gin.Context, req *official.APIRequest, client *kimiweb.Client, streamReq kimiweb.CompletionRequest) {
	resp, err := d.completeWithAuth(client, streamReq)
	if err != nil {
		c.JSON(502, gin.H{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	var fullText, fullReasoning string
	res := kimiweb.ConsumeStream(resp.Body, func(delta kimiweb.Delta) {
		if delta.ToolCall != nil {
			return
		}
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
