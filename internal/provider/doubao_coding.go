package provider

import (
	"net/http"
	"strings"

	"aurora/internal/doubaoweb"
	"aurora/internal/toolcall"
	"aurora/typings/official"
	"aurora/util"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// codingResponses 处理豆包 coding 变体(/v1/responses)。
// 工具调用走文本协议(同 DeepSeek),豆包模型工具能力未实测,尽力而为。
func (d *Doubao) codingResponses(c *gin.Context, m *doubaoModel, req *official.ResponsesAPIRequest) {
	client := d.webClient()
	if client == nil || !client.HasAccount() {
		c.JSON(502, gin.H{"error": "doubao web client unavailable: missing DOUBAO_ACCOUNTS?"})
		return
	}
	prompt := doubaoCodingPromptFromResponses(req)
	streamReq := doubaoweb.CompletionRequest{Prompt: prompt}
	if req.Stream {
		d.codingResponsesStream(c, req, client, streamReq)
		return
	}
	d.codingResponsesNonStream(c, req, client, streamReq)
}

// doubaoCodingPromptFromResponses 组装 coding prompt。
func doubaoCodingPromptFromResponses(req *official.ResponsesAPIRequest) string {
	var sb strings.Builder
	if len(req.Tools) > 0 {
		sb.WriteString(toolcall.BuildInstructions(req.Tools, req.ToolChoice))
		sb.WriteString("\n\n")
	}
	if instr := rawResponsesText(req.Instructions); instr != "" {
		sb.WriteString(instr)
		sb.WriteString("\n\n")
	}
	items := responsesInputItems(req.Input)
	for _, it := range items {
		text := it.Text
		switch it.Type {
		case "function_call":
			text = "<tool_call>" + text + "</tool_call>"
		case "function_call_output":
			text = "Tool result: " + text
		}
		if text == "" {
			continue
		}
		sb.WriteString(text)
		sb.WriteString("\n\n")
	}
	return strings.TrimSpace(sb.String())
}

// codingResponsesStream 流式 Responses。
func (d *Doubao) codingResponsesStream(c *gin.Context, req *official.ResponsesAPIRequest, client *doubaoweb.Client, streamReq doubaoweb.CompletionRequest) {
	w := newSSEWriter(c)
	respID := "resp_" + uuid.NewString()
	messageItemID := "msg_" + uuid.NewString()
	w.event("response.created", createdEvent(respID, req.Model))
	w.event("response.output_item.added", outputItemAddedEvent(0, map[string]any{"id": messageItemID, "type": "message", "status": "in_progress", "role": "assistant"}))

	parser := toolcall.NewParserWithTagsAndTools(toolcall.DefaultTags, req.Tools)
	var textBuf strings.Builder
	var calls []official.ToolCall
	res := client.Complete(streamReq, func(delta doubaoweb.Delta) {
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

	finalText := textBuf.String()
	if res.Err != "" && finalText == "" && len(calls) == 0 {
		w.event("response.failed", failedEvent(res.Err))
		return
	}
	w.event("response.output_item.done", outputItemDoneEvent(0, messageItem(messageItemID, finalText, "completed")))
	outResp := official.NewResponsesResponse(finalText, "", countInputChars(req), len(finalText), 0, 0, 0, req.Model)
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

// codingResponsesNonStream 非流式。
func (d *Doubao) codingResponsesNonStream(c *gin.Context, req *official.ResponsesAPIRequest, client *doubaoweb.Client, streamReq doubaoweb.CompletionRequest) {
	var fullText string
	res := client.Complete(streamReq, func(delta doubaoweb.Delta) { fullText += delta.Text })
	if res.Err != "" && fullText == "" {
		c.JSON(502, gin.H{"error": res.Err})
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

// codingCompletions 处理豆包 coding 变体(/v1/chat/completions)。
func (d *Doubao) codingCompletions(c *gin.Context, m *doubaoModel, req *official.APIRequest) {
	client := d.webClient()
	if client == nil || !client.HasAccount() {
		c.JSON(502, gin.H{"error": "doubao web client unavailable: missing DOUBAO_ACCOUNTS?"})
		return
	}
	prompt := doubaoCodingPromptFromAPI(req)
	streamReq := doubaoweb.CompletionRequest{Prompt: prompt}
	if req.Stream {
		d.codingCompletionsStream(c, req, client, streamReq)
		return
	}
	d.codingCompletionsNonStream(c, req, client, streamReq)
}

// doubaoCodingPromptFromAPI 组装 chat.completions coding prompt。
func doubaoCodingPromptFromAPI(req *official.APIRequest) string {
	var sb strings.Builder
	if len(req.Tools) > 0 {
		sb.WriteString(toolcall.BuildInstructions(req.Tools, req.ToolChoice))
		sb.WriteString("\n\n")
	}
	for _, msg := range req.Messages {
		if len(msg.ToolCalls) > 0 {
			for _, tc := range msg.ToolCalls {
				sb.WriteString("<tool_call>" + tc.Function.Arguments + "</tool_call>\n\n")
			}
			continue
		}
		if msg.Role == "tool" {
			sb.WriteString("Tool result: " + msg.Text() + "\n\n")
			continue
		}
		if t := msg.Text(); t != "" {
			sb.WriteString(t + "\n\n")
		}
	}
	return strings.TrimSpace(sb.String())
}

// codingCompletionsStream 流式 chat.completion.chunk。
func (d *Doubao) codingCompletionsStream(c *gin.Context, req *official.APIRequest, client *doubaoweb.Client, streamReq doubaoweb.CompletionRequest) {
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
	_ = client.Complete(streamReq, func(delta doubaoweb.Delta) {
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

// codingCompletionsNonStream 非流式。
func (d *Doubao) codingCompletionsNonStream(c *gin.Context, req *official.APIRequest, client *doubaoweb.Client, streamReq doubaoweb.CompletionRequest) {
	var fullText string
	res := client.Complete(streamReq, func(delta doubaoweb.Delta) { fullText += delta.Text })
	if res.Err != "" && fullText == "" {
		c.JSON(502, gin.H{"error": res.Err})
		return
	}
	toolCalls := toolcall.RecoverFromText(fullText, req.Tools)
	cleanText := toolcall.StripTags(fullText, toolcall.DefaultTags)
	outResp := official.NewChatCompletionWithToolCalls(cleanText, "", toolCalls, countMessagesChars(req.Messages), util.CountToken(cleanText), req.Model, "", nil)
	c.JSON(200, outResp)
}
