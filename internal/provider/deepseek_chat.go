package provider

import (
	"fmt"
	"strings"

	"aurora/internal/deepseekweb"
	"aurora/typings/official"
	"aurora/util"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// chatResponses 处理 DeepSeek chat 变体(/v1/responses)。
//
// 硬规则:上游只发「真人对话」形态的请求 —— 剥离客户端 tools/tool_choice,
// 不注入任何工具调用信息;仅携带网页模式开关(快速/专家、智能搜索、深度思考、识图)。
// 识图(快速模式)与联网搜索互斥(DeepSeek 网页行为)。
func (d *DeepSeek) chatResponses(c *gin.Context, m *deepseekModel, req *official.ResponsesAPIRequest) {
	client := d.webClient()
	if client == nil {
		c.JSON(502, gin.H{"error": "deepseek web client unavailable: missing DEEPSEEK_WEB_TOKENS?"})
		return
	}
	token := client.NextToken()
	if token == "" {
		c.JSON(502, gin.H{"error": "deepseek web token pool is empty"})
		return
	}

	// 拍平多轮 input 为真人对话 prompt(无任何工具信息)。
	prompt := flattenChatInput(req, m.Mode == modeQuick)

	sessionID, err := client.CreateSession(token)
	if err != nil {
		c.JSON(502, gin.H{"error": fmt.Sprintf("deepseek create session: %v", err)})
		return
	}
	defer client.DeleteSession(token, sessionID)

	// 识图:提取 input 里的图片,上传并 fork 成 vision 版。
	refFileIDs, _ := uploadImages(client, token, req)

	// 带图时 completion 用 model_type=vision + fork 后的 ref_file_ids
	// (实测 P0:网页点"发送至识图模式"后发的正是这个形态,parent 可空)。
	modelType := modelTypeFor(m)
	if len(refFileIDs) > 0 {
		modelType = "vision"
	}

	streamReq := deepseekweb.CompletionRequest{
		SessionID:       sessionID,
		Prompt:          prompt,
		ModelType:       modelType,
		ThinkingEnabled: thinkingEnabled(m, req),
		SearchEnabled:   m.Mode == modeQuick,
		RefFileIDs:      refFileIDs,
	}
	// 续轮:需 parent_message_id —— 简化首版不续轮,每次新会话(可接受)。
	if req.Stream {
		d.chatStream(c, m, req, client, token, sessionID, streamReq)
		return
	}
	d.chatNonStream(c, m, req, client, token, sessionID, streamReq)
}

// modelTypeFor chat 变体的网页 model_type 映射。
// [P0] 需官网实测确认枚举(default/expert/vision)。
func modelTypeFor(m *deepseekModel) string {
	switch m.Mode {
	case modeQuick:
		return "default"
	default:
		return "expert"
	}
}

// thinkingEnabled 根据模式与 reasoning.effort 决定是否开深度思考。
func thinkingEnabled(m *deepseekModel, req *official.ResponsesAPIRequest) bool {
	if m.Mode != modeExpert {
		return false
	}
	if req.Reasoning != nil && req.Reasoning.Effort != "" && req.Reasoning.Effort == "none" {
		return false
	}
	return true
}

// flattenChatInput 把 Responses input 拍平成网页 prompt 的真人对话文本。
// - chat 变体:完全忽略 tools/tool_choice
// - 不加 "User:"/"Assistant:" 前缀:网页真实请求的 prompt 是纯文本(角色锚点
//   由模型专用 token 承担),实测加前缀会被模型当成乱码/怪文本。
// - 多轮 history 直接拼接(网页服务端按 session+parent_message_id 记忆,
//   aurora 每请求新会话,需全量提交)
func flattenChatInput(req *official.ResponsesAPIRequest, quickMode bool) string {
	var sb strings.Builder
	// instructions → system 前缀(仅真人对话语境,无工具说明)
	if instr := rawResponsesText(req.Instructions); instr != "" {
		sb.WriteString(instr)
		sb.WriteString("\n\n")
	}
	for _, item := range responsesInputItems(req.Input) {
		switch item.Type {
		case "function_call", "function_call_output":
			// chat 变体不应出现工具 item;防御性跳过(不注入上游)。
			continue
		default:
			text := item.Text
			if text == "" {
				continue
			}
			sb.WriteString(text)
			sb.WriteString("\n\n")
		}
	}
	return strings.TrimSpace(sb.String())
}

// chatStream 流式输出 Responses 事件。
func (d *DeepSeek) chatStream(c *gin.Context, m *deepseekModel, req *official.ResponsesAPIRequest, client *deepseekweb.Client, token, sessionID string, streamReq deepseekweb.CompletionRequest) {
	resp, err := client.Complete(token, streamReq)
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
	res := deepseekweb.ConsumeStream(resp.Body, func(delta deepseekweb.Delta) {
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

// chatNonStream 非流式:走网页流式消费后一次性返回完整 ResponsesResponse。
func (d *DeepSeek) chatNonStream(c *gin.Context, m *deepseekModel, req *official.ResponsesAPIRequest, client *deepseekweb.Client, token, sessionID string, streamReq deepseekweb.CompletionRequest) {
	resp, err := client.Complete(token, streamReq)
	if err != nil {
		c.JSON(502, gin.H{"error": err.Error()})
		return
	}
	defer resp.Body.Close()

	var fullText, fullReasoning string
	res := deepseekweb.ConsumeStream(resp.Body, func(delta deepseekweb.Delta) {
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
