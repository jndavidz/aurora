package provider

import (
	"fmt"
	"net/http"
	"strings"

	"aurora/internal/deepseekweb"
	"aurora/internal/toolcall"
	"aurora/typings/official"
	"aurora/util"

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

// buildDeepSeekCodingPrompt 拍平 Responses input + 工具指令。
// 工具标签使用 DeepSeek 网页的 <|tool_calls_begin|>/<|tool_calls_end|> 系
// (见 deepseek网页协议整理.md §7.2);提示词细节 [P0] 实测后按网页行为对齐。
func buildDeepSeekCodingPrompt(req *official.ResponsesAPIRequest) string {
	return buildCodingPromptItems(responsesInputItems(req.Input), rawResponsesText(req.Instructions), req.Tools, req.ToolChoice)
}

// buildDeepSeekCodingPromptAPI 从 chat.completions messages 构建 coding prompt(共享实现)。
func buildDeepSeekCodingPromptAPI(req *official.APIRequest) string {
	return buildCodingPromptItems(apiMessagesToItems(req.Messages), "", req.Tools, req.ToolChoice)
}

// buildCodingPromptItems 双接口共享的 coding prompt 构建。
func buildCodingPromptItems(items []responsesInputItem, instructions string, tools []official.Tool, toolChoice *official.ToolChoice) string {
	var sb strings.Builder
	if instructions != "" {
		sb.WriteString(instructions)
		sb.WriteString("\n\n")
	}
	if len(tools) > 0 {
		sb.WriteString(toolcall.BuildInstructionsWithTags(deepseekTagSet(), tools, toolChoice))
		sb.WriteString("\n\n")
	}
	for _, item := range items {
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
	// 末尾强提醒:角色感知——最后是工具结果时用更强的"继续调工具"提醒,
	// 否则用通用"只输出工具块"提醒(仿网页 reminder 注入)。
	if len(tools) > 0 {
		if lastItemIsToolResult(items) {
			sb.WriteString(deepSeekToolResultNudge(deepseekTagSet()))
		} else {
			sb.WriteString(deepSeekCodingNudge(deepseekTagSet()))
		}
	}
	return strings.TrimSpace(sb.String())
}

// lastItemIsToolResult 报告最后一条有效消息是否是工具结果(决定 nudge 强度)。
func lastItemIsToolResult(items []responsesInputItem) bool {
	for i := len(items) - 1; i >= 0; i-- {
		it := items[i]
		if it.Type == "function_call_output" {
			return true
		}
		if it.Type == "message" && it.Text != "" {
			return false
		}
	}
	return false
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

// deepSeekToolResultNudge 是最后一条消息为工具结果时的强提醒:
// 防"工具链断裂"——模型拿到工具结果后不继续调工具,反而输出计划/进度报告/凭文件名猜测。
// 对齐 ChatGPT local-toolfix 的 FinalNudge(tool) 分支。
func deepSeekToolResultNudge(tags toolcall.TagSet) string {
	return "\n\n[SYSTEM INSTRUCTION — 工具结果已返回:\n" +
		"The tool output above is the REAL result produced by running your tool call on the user's actual machine. Treat it as ground truth and as the current state of the workspace.\n" +
		"You have DIRECT read access to every file through the tools — the tool output IS the real file content.\n" +
		"A file LISTING is NOT the file content. If the task requires reading files, you are NOT done until you have read each relevant file with the read tool. Summarizing from a file tree or file names is GUESSING and is WRONG.\n" +
		"NEVER ask the user to provide, paste, or upload file contents — you can read them yourself.\n" +
		"A progress report, a reading plan, or a promise like 'I will read next' is NOT a valid reply and NOT a final answer. If the task is not finished, emit the next " + tags.StartTag + " block in THIS reply now — there is no later turn unless you call a tool now.]"
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

	w.event("response.created", createdEvent(respID, req.Model))
	w.event("response.output_item.added", outputItemAddedEvent(0, map[string]any{"id": messageItemID, "type": "message", "status": "in_progress", "role": "assistant"}))

	// 用 toolcall.Parser 流式解析文本块。
	parser := toolcall.NewParserWithTags(deepseekTagSet())
	var textBuf strings.Builder
	var calls []official.ToolCall

	deepseekweb.ConsumeStream(resp.Body, func(delta deepseekweb.Delta) {
		if delta.Text == "" {
			return
		}
		textDelta, parsed := parser.Feed(delta.Text)
		if textDelta != "" {
			textBuf.WriteString(textDelta)
			w.event("response.output_text.delta", map[string]any{
				"type": "response.output_text.delta", "item_id": messageItemID,
				"output_index": 0, "content_index": 0, "delta": textDelta,
			})
		}
		calls = append(calls, parsed...)
	})

	// 流结束:Flush 残余(未闭合标签)
	textDelta, parsed := parser.Flush()
	if textDelta != "" {
		textBuf.WriteString(textDelta)
		w.event("response.output_text.delta", map[string]any{
			"type": "response.output_text.delta", "item_id": messageItemID,
			"output_index": 0, "content_index": 0, "delta": textDelta,
		})
	}
	calls = append(calls, parsed...)

	// 先完成 message item(index 0),再按序发 function_call items(index 1..n)。
	// 协议要求 output_index 每 item 唯一递增,且 item 按序完成(此前两者都挤在 0,
	// 且 message.done 后发,严格客户端无法注册工具调用)。
	w.event("response.output_item.done", outputItemDoneEvent(0, messageItem(messageItemID, textBuf.String(), "completed")))

	// 组装最终响应:message + function_call items。
	outResp := official.NewResponsesResponse(textBuf.String(), "", countInputChars(req), len(textBuf.String()), 0, 0, 0, req.Model)
	for i, tc := range calls {
		idx := i + 1
		fcID := "fc_" + uuid.NewString()
		callID := tc.ID
		if callID == "" {
			callID = "call_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24]
		}
		w.event("response.output_item.added", outputItemAddedEvent(idx, functionCallItem(fcID, callID, tc.Function.Name, "", "in_progress")))
		w.event("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": fcID,
			"output_index": idx, "delta": tc.Function.Arguments,
		})
		w.event("response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "item_id": fcID,
			"output_index": idx, "arguments": tc.Function.Arguments,
		})
		w.event("response.output_item.done", outputItemDoneEvent(idx, functionCallItem(fcID, callID, tc.Function.Name, tc.Function.Arguments, "completed")))
		outResp.Output = append(outResp.Output, official.ResponsesOutputItem{
			ID: fcID, Type: "function_call", Status: "completed",
			CallID: callID, Name: tc.Function.Name, Arguments: tc.Function.Arguments,
		})
	}
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

// ── /v1/chat/completions 支持(coding 变体)──

// codingCompletions 处理 DeepSeek coding 变体(/v1/chat/completions)。
func (d *DeepSeek) codingCompletions(c *gin.Context, m *deepseekModel, req *official.APIRequest) {
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

	prompt := buildDeepSeekCodingPromptAPI(req)

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
		d.codingCompletionsStream(c, m, req, client, token, streamReq)
		return
	}
	d.codingCompletionsNonStream(c, m, req, client, token, streamReq)
}

// codingCompletionsStream 流式:文本协议工具调用 → tool_calls delta / content chunk。
func (d *DeepSeek) codingCompletionsStream(c *gin.Context, m *deepseekModel, req *official.APIRequest, client *deepseekweb.Client, token string, streamReq deepseekweb.CompletionRequest) {
	resp, err := client.Complete(token, streamReq)
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

	parser := toolcall.NewParserWithTags(deepseekTagSet())
	var emittedCall bool
	_ = deepseekweb.ConsumeStream(resp.Body, func(delta deepseekweb.Delta) {
		if delta.Text == "" {
			return
		}
		textDelta, calls := parser.Feed(delta.Text)
		if textDelta != "" {
			writeChunk(official.NewChatCompletionChunk(textDelta, model))
		}
		for _, tc := range calls {
			emittedCall = true
			for _, d := range toolcall.StreamToToolCallDeltas([]official.ToolCall{tc}) {
				writeChunk(official.NewToolCallChunk(model, d...))
			}
		}
	})
	textDelta, calls := parser.Flush()
	if textDelta != "" {
		writeChunk(official.NewChatCompletionChunk(textDelta, model))
	}
	for _, tc := range calls {
		emittedCall = true
		for _, d := range toolcall.StreamToToolCallDeltas([]official.ToolCall{tc}) {
			writeChunk(official.NewToolCallChunk(model, d...))
		}
	}

	if emittedCall {
		writeChunk(official.NewToolCallStopChunk(model, ""))
	} else {
		writeChunk(official.StopChunk("stop", model))
	}
	c.Writer.WriteString("data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// codingCompletionsNonStream 非流式:解析全文,带 tool_calls 或纯文本返回。
func (d *DeepSeek) codingCompletionsNonStream(c *gin.Context, m *deepseekModel, req *official.APIRequest, client *deepseekweb.Client, token string, streamReq deepseekweb.CompletionRequest) {
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

	toolCalls := toolcall.RecoverFromText(fullText, req.Tools)
	cleanText := toolcall.StripTags(fullText, deepseekTagSet())
	inputTokens := countMessagesChars(req.Messages)
	outResp := official.NewChatCompletionWithToolCalls(cleanText, "", toolCalls, inputTokens, util.CountToken(cleanText), req.Model, "", nil)
	c.JSON(200, outResp)
}
