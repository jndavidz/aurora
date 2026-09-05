package provider

import (
	"net/http"
	"strings"

	"aurora/internal/apierrors"
	"aurora/internal/toolcall"
	"aurora/internal/yuanbaoweb"
	"aurora/typings/official"
	"aurora/util"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// yuanbaoCodingResponses 处理元宝 coding 变体(/v1/responses)。
//
// 与 DeepSeek/ChatGPT coding 相同的「文本协议工具调用」:把客户端 tools 注入提示词,
// 引导模型输出 <tool_call>{...}</tool_call> 标签块,流式解析成 function_call item 回吐。
// 元宝网页 API 无原生 function calling,此变体是注入式模拟(能力如实标注为 CapFunctionCall)。
// 标签先用默认 <tool_call> 系;实测模型不跟随再换围栏/其它标签。
func (d *Yuanbao) yuanbaoCodingResponses(c *gin.Context, m *yuanbaoModel, req *official.ResponsesAPIRequest) {
	streamReq := yuanbaoweb.ChatRequest{
		ChatModelID: m.ChatModelID,
		Prompt:      yuanbaoCodingPrompt(req),
		WebSearch:   false, // 工具调用场景关联网搜索,保持干净上下文
	}
	if req.Stream {
		d.yuanbaoCodingResponsesStream(c, m, req, streamReq)
		return
	}
	d.yuanbaoCodingResponsesNonStream(c, m, req, streamReq)
}

// yuanbaoCodingCompletions 处理元宝 coding 变体(/v1/chat/completions)。
func (d *Yuanbao) yuanbaoCodingCompletions(c *gin.Context, m *yuanbaoModel, req *official.APIRequest) {
	streamReq := yuanbaoweb.ChatRequest{
		ChatModelID: m.ChatModelID,
		Prompt:      yuanbaoCodingPromptAPI(req),
		WebSearch:   false,
	}
	if req.Stream {
		d.yuanbaoCodingCompletionsStream(c, m, req, streamReq)
		return
	}
	d.yuanbaoCodingCompletionsNonStream(c, m, req, streamReq)
}

// yuanbaoCodingPrompt 拍平 Responses input + 工具指令(DefaultTags 版)。
func yuanbaoCodingPrompt(req *official.ResponsesAPIRequest) string {
	return yuanbaoCodingPromptItems(responsesInputItems(req.Input), rawResponsesText(req.Instructions), req.Tools, req.ToolChoice)
}

// yuanbaoCodingPromptAPI 从 chat.completions messages 构建 coding prompt。
func yuanbaoCodingPromptAPI(req *official.APIRequest) string {
	return yuanbaoCodingPromptItems(apiMessagesToItems(req.Messages), "", req.Tools, req.ToolChoice)
}

// yuanbaoCodingPromptItems 双接口共享的 coding prompt 构建(与 buildCodingPromptItems
// 同构,仅标签用 toolcall.DefaultTags;nudge 复用通用的工具链提醒)。
func yuanbaoCodingPromptItems(items []responsesInputItem, instructions string, tools []official.Tool, toolChoice *official.ToolChoice) string {
	tags := toolcall.DefaultTags
	var sb strings.Builder
	if instructions != "" {
		sb.WriteString(instructions)
		sb.WriteString("\n\n")
	}
	if len(tools) > 0 {
		sb.WriteString(toolcall.BuildInstructionsWithTags(tags, tools, toolChoice))
		sb.WriteString("\n\n")
	}
	for _, item := range items {
		switch item.Type {
		case "function_call":
			// 回放历史工具调用:标签包裹 arguments。
			sb.WriteString(tags.StartTag + item.Text + tags.EndTag)
		case "function_call_output":
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
	if len(tools) > 0 {
		if lastItemIsToolResult(items) {
			sb.WriteString(deepSeekToolResultNudge(tags))
		} else {
			sb.WriteString(deepSeekCodingNudge(tags))
		}
	}
	return strings.TrimSpace(sb.String())
}

// yuanbaoCodingResponsesStream 流式:文本协议工具调用 → function_call item / output_text delta。
func (d *Yuanbao) yuanbaoCodingResponsesStream(c *gin.Context, m *yuanbaoModel, req *official.ResponsesAPIRequest, streamReq yuanbaoweb.ChatRequest) {
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

	parser := toolcall.NewParserWithTagsAndTools(toolcall.DefaultTags, req.Tools)
	var textBuf strings.Builder
	var calls []official.ToolCall

	_ = yuanbaoweb.ConsumeStream(resp.Body, func(delta yuanbaoweb.Delta) {
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
	textDelta, parsed := parser.Flush()
	if textDelta != "" {
		textBuf.WriteString(textDelta)
		w.event("response.output_text.delta", map[string]any{
			"type": "response.output_text.delta", "item_id": messageItemID,
			"output_index": 0, "content_index": 0, "delta": textDelta,
		})
	}
	calls = append(calls, parsed...)
	calls = mergeRecoveredCalls(calls, textBuf.String(), req.Tools)

	w.event("response.output_item.done", outputItemDoneEvent(0, messageItem(messageItemID, textBuf.String(), "completed")))
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

// yuanbaoCodingResponsesNonStream 非流式。
func (d *Yuanbao) yuanbaoCodingResponsesNonStream(c *gin.Context, m *yuanbaoModel, req *official.ResponsesAPIRequest, streamReq yuanbaoweb.ChatRequest) {
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
	toolCalls := toolcall.RecoverFromText(fullText, req.Tools)
	cleanText := toolcall.StripTags(fullText, toolcall.DefaultTags)
	outResp := official.NewResponsesResponse(cleanText, "", countInputChars(req), util.CountToken(cleanText), 0, 0, 0, req.Model)
	for _, tc := range toolCalls {
		outResp.Output = append(outResp.Output, official.ResponsesOutputItem{
			ID: "fc_" + uuid.NewString(), Type: "function_call", Status: "completed",
			CallID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments,
		})
	}
	c.JSON(200, outResp)
}

// yuanbaoCodingCompletionsStream 流式 chat.completion.chunk + tool_calls delta。
func (d *Yuanbao) yuanbaoCodingCompletionsStream(c *gin.Context, m *yuanbaoModel, req *official.APIRequest, streamReq yuanbaoweb.ChatRequest) {
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

	parser := toolcall.NewParserWithTagsAndTools(toolcall.DefaultTags, req.Tools)
	var textBuf strings.Builder
	var emittedCall bool
	_ = yuanbaoweb.ConsumeStream(resp.Body, func(delta yuanbaoweb.Delta) {
		if delta.Text == "" {
			return
		}
		textDelta, calls := parser.Feed(delta.Text)
		if textDelta != "" {
			textBuf.WriteString(textDelta)
			writeChunk(official.NewChatCompletionChunk(textDelta, model))
		}
		for _, tc := range calls {
			emittedCall = true
			for _, dd := range toolcall.StreamToToolCallDeltas([]official.ToolCall{tc}) {
				writeChunk(official.NewToolCallChunk(model, dd...))
			}
		}
	})
	textDelta, calls := parser.Flush()
	if textDelta != "" {
		textBuf.WriteString(textDelta)
		writeChunk(official.NewChatCompletionChunk(textDelta, model))
	}
	for _, tc := range calls {
		emittedCall = true
		for _, dd := range toolcall.StreamToToolCallDeltas([]official.ToolCall{tc}) {
			writeChunk(official.NewToolCallChunk(model, dd...))
		}
	}
	for _, tc := range mergeRecoveredCalls(nil, textBuf.String(), req.Tools) {
		emittedCall = true
		for _, dd := range toolcall.StreamToToolCallDeltas([]official.ToolCall{tc}) {
			writeChunk(official.NewToolCallChunk(model, dd...))
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

// yuanbaoCodingCompletionsNonStream 非流式:解析全文,带 tool_calls 或纯文本返回。
func (d *Yuanbao) yuanbaoCodingCompletionsNonStream(c *gin.Context, m *yuanbaoModel, req *official.APIRequest, streamReq yuanbaoweb.ChatRequest) {
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
	toolCalls := toolcall.RecoverFromText(fullText, req.Tools)
	cleanText := toolcall.StripTags(fullText, toolcall.DefaultTags)
	outResp := official.NewChatCompletionWithToolCalls(cleanText, "", toolCalls, countMessagesChars(req.Messages), util.CountToken(cleanText), req.Model, "", nil)
	c.JSON(200, outResp)
}
