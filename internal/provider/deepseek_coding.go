package provider

import (
	"fmt"
	"strings"

	"aurora/internal/deepseekweb"
	"aurora/internal/toolcall"
	"aurora/typings/official"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// codingResponses 处理 DeepSeek coding 变体(/v1/responses)。
//
// 与 ChatGPT coding 相同的「文本协议工具调用」:把客户端 tools 注入提示词,
// 引导模型输出 <|tool_calls_begin|>...</|tool_calls_end|> 文本块,解析成
// function_call item 回吐。复用 internal/toolcall 的解析/恢复/重试机制,
// 仅标签不同(DeepSeek 用 <|tool▁calls▁begin|> 系)。
func (d *DeepSeek) codingResponses(c *gin.Context, m *deepseekModel, req *official.ResponsesAPIRequest) {
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

	// 拍平 input(含 function_call/function_call_output 重放)并注入工具提示词。
	prompt := buildDeepSeekCodingPrompt(req)

	sessionID, err := client.CreateSession(token)
	if err != nil {
		c.JSON(502, gin.H{"error": fmt.Sprintf("deepseek create session: %v", err)})
		return
	}
	defer client.DeleteSession(token, sessionID)

	streamReq := deepseekweb.CompletionRequest{
		SessionID:       sessionID,
		Prompt:          prompt,
		ModelType:       "default",
		ThinkingEnabled: true,
		SearchEnabled:   false,
	}

	if req.Stream {
		d.codingStream(c, m, req, client, token, streamReq)
		return
	}
	d.codingNonStream(c, m, req, client, token, streamReq)
}

// buildDeepSeekCodingPrompt 拍平 input + 工具指令。
// 工具标签使用 DeepSeek 网页的 <|tool_calls_begin|>/<|tool_calls_end|> 系
// (见 deepseek网页协议整理.md §7.2);提示词细节 [P0] 实测后按网页行为对齐。
func buildDeepSeekCodingPrompt(req *official.ResponsesAPIRequest) string {
	var sb strings.Builder
	if instr := rawResponsesText(req.Instructions); instr != "" {
		sb.WriteString(instr)
		sb.WriteString("\n\n")
	}
	if len(req.Tools) > 0 {
		sb.WriteString(toolcall.BuildInstructionsWithTags(deepseekTagSet(), req.Tools, req.ToolChoice))
		sb.WriteString("\n\n")
	}
	for _, item := range responsesInputItems(req.Input) {
		switch item.Type {
		case "function_call":
			// 回放历史工具调用:用 DeepSeek 标签包裹 arguments。
			sb.WriteString("<|tool_call_begin|>" + item.Text + "<|tool_call_end|>")
		case "function_call_output":
			// 工具结果:简短标签标注(网页用角色锚点 token,我们退化为文本标签)。
			sb.WriteString("Tool result: " + item.Text)
		default:
			text := item.Text
			if text == "" {
				continue
			}
			sb.WriteString(text)
		}
		sb.WriteString("\n\n")
	}
	// 末尾强提醒:只输出工具块,不做散文(仿网页 reminder 注入)。
	if len(req.Tools) > 0 {
		sb.WriteString(deepSeekCodingNudge(deepseekTagSet()))
	}
	return strings.TrimSpace(sb.String())
}

// deepseekTagSet 返回 DeepSeek 网页的工具标签(文本协议)。
// 网页实测(2026-08-13):模型遵循 <|tool▁calls▁begin|>(▁=U+2581)形式;
// NormalizeTagged 已覆盖 <tool_calls_begin|>、ASCII 下划线等变体。
func deepseekTagSet() toolcall.TagSet {
	return toolcall.TagSet{
		StartTag: "<|tool\u2581calls\u2581begin|>",
		EndTag:   "<|tool\u2581calls\u2581end|>",
	}
}

// deepSeekCodingNudge 是 coding 变体末尾的强提醒(仿网页 reminder 注入):
// 强制模型只输出标签块,不做散文描述。
func deepSeekCodingNudge(tags toolcall.TagSet) string {
	return "\n\n[SYSTEM INSTRUCTION: To call a tool, output ONLY the " + tags.StartTag + " block with valid JSON inside, and nothing else — no prose, no explanation, no preamble. Then stop and wait for the tool result. If the task is not finished, in your next reply output only the next tool call block.]"
}

// codingStream 流式:文本协议工具调用 → function_call item / output_text delta。
func (d *DeepSeek) codingStream(c *gin.Context, m *deepseekModel, req *official.ResponsesAPIRequest, client *deepseekweb.Client, token string, streamReq deepseekweb.CompletionRequest) {
	resp, err := client.Complete(token, streamReq)
	if err != nil {
		c.JSON(502, gin.H{"error": err.Error()})
		return
	}
	defer resp.Body.Close()

	w := newSSEWriter(c)
	respID := "resp_" + uuid.NewString()
	messageItemID := "msg_" + uuid.NewString()
	callID := "call_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24]

	w.event("response.created", createdEvent(respID, req.Model))
	w.event("response.output_item.added", outputItemAddedEvent(0, map[string]any{"id": messageItemID, "type": "message", "status": "in_progress", "role": "assistant"}))

	// 用 toolcall.Parser 流式解析文本块。
	parser := toolcall.NewParserWithTags(deepseekTagSet())
	var textBuf strings.Builder

	emitCall := func(fcID string, tc official.ToolCall) {
		w.event("response.output_item.added", outputItemAddedEvent(0, functionCallItem(fcID, callID, tc.Function.Name, "", "in_progress")))
		w.event("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": fcID,
			"output_index": 0, "delta": tc.Function.Arguments,
		})
		w.event("response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "item_id": fcID,
			"output_index": 0, "arguments": tc.Function.Arguments,
		})
		w.event("response.output_item.done", outputItemDoneEvent(0, functionCallItem(fcID, callID, tc.Function.Name, tc.Function.Arguments, "completed")))
	}

	deepseekweb.ConsumeStream(resp.Body, func(delta deepseekweb.Delta) {
		if delta.Text == "" {
			return
		}
		textDelta, calls := parser.Feed(delta.Text)
		if textDelta != "" {
			textBuf.WriteString(textDelta)
			w.event("response.output_text.delta", map[string]any{
				"type": "response.output_text.delta", "item_id": messageItemID,
				"output_index": 0, "content_index": 0, "delta": textDelta,
			})
		}
		for _, tc := range calls {
			emitCall("fc_"+uuid.NewString(), tc)
		}
	})

	// 流结束:Flush 残余(未闭合标签)
	textDelta, calls := parser.Flush()
	if textDelta != "" {
		textBuf.WriteString(textDelta)
		w.event("response.output_text.delta", map[string]any{
			"type": "response.output_text.delta", "item_id": messageItemID,
			"output_index": 0, "content_index": 0, "delta": textDelta,
		})
	}
	for _, tc := range calls {
		emitCall("fc_"+uuid.NewString(), tc)
	}

	// 汇总事件
	w.event("response.output_item.done", outputItemDoneEvent(0, messageItem(messageItemID, textBuf.String(), "completed")))
	outResp := official.NewResponsesResponse(textBuf.String(), "", countInputChars(req), len(textBuf.String()), 0, 0, 0, req.Model)
	w.event("response.completed", completedEvent(outResp))
}

// codingNonStream 非流式:解析完整文本,按 tool_calls 或纯文本返回。
func (d *DeepSeek) codingNonStream(c *gin.Context, m *deepseekModel, req *official.ResponsesAPIRequest, client *deepseekweb.Client, token string, streamReq deepseekweb.CompletionRequest) {
	resp, err := client.Complete(token, streamReq)
	if err != nil {
		c.JSON(502, gin.H{"error": err.Error()})
		return
	}
	defer resp.Body.Close()

	var fullText string
	res := deepseekweb.ConsumeStream(resp.Body, func(delta deepseekweb.Delta) {
		fullText += delta.Text
	})
	if res.Err != "" && fullText == "" {
		c.JSON(502, gin.H{"error": res.Err})
		return
	}

	// 从全文解析 tool_calls(文本协议)
	toolCalls := toolcall.RecoverFromText(fullText, req.Tools)
	// 剥掉标签残留文本作为正文
	cleanText := toolcall.StripTags(fullText, deepseekTagSet())

	outResp := official.NewResponsesResponse(cleanText, "", countInputChars(req), len(cleanText), 0, 0, 0, req.Model)
	// 若解析出工具调用,以 function_call 形式返回(简化:文本返回,工具走流式路径)。
	if len(toolCalls) > 0 {
		outResp.Output = append(outResp.Output, official.ResponsesOutputItem{
			ID: "fc_" + uuid.NewString(), Type: "function_call", Status: "completed",
			CallID: toolCalls[0].ID, Name: toolCalls[0].Function.Name, Arguments: toolCalls[0].Function.Arguments,
		})
	}
	c.JSON(200, outResp)
}
