package provider

import (
	"net/http"
	"strings"

	"aurora/internal/mimoweb"
	"aurora/internal/toolcall"
	"aurora/typings/official"
	"aurora/util"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// mimoModelCode 是网页端真实模型 id(对外 -chat/-coding 都映射到它)。
const mimoModelCode = "mimo-v2.5-pro"

// ─── chat 变体 ────────────────────────────────────────────────────

func (d *Mimo) chatResponses(c *gin.Context, m *mimoModel, req *official.ResponsesAPIRequest) {
	client := d.webClient()
	if client == nil || !client.HasToken() {
		c.JSON(502, gin.H{"error": "mimo web client unavailable: missing MIMO_WEB_TOKENS?"})
		return
	}
	token := client.NextToken()
	prompt := flattenChatInput(req, true)
	streamReq := mimoweb.CompletionRequest{Prompt: prompt, Model: mimoModelCode}
	if req.Stream {
		d.chatResponsesStream(c, req, client, token, streamReq)
		return
	}
	d.chatResponsesNonStream(c, req, client, token, streamReq)
}

func (d *Mimo) chatResponsesStream(c *gin.Context, req *official.ResponsesAPIRequest, client *mimoweb.Client, token string, streamReq mimoweb.CompletionRequest) {
	w := newSSEWriter(c)
	respID := "resp_" + uuid.NewString()
	messageItemID := "msg_" + uuid.NewString()
	w.event("response.created", createdEvent(respID, req.Model))
	w.event("response.output_item.added", outputItemAddedEvent(0, map[string]any{"id": messageItemID, "type": "message", "status": "in_progress", "role": "assistant"}))

	var fullText string
	res := client.Complete(token, streamReq, func(delta mimoweb.Delta) {
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

func (d *Mimo) chatResponsesNonStream(c *gin.Context, req *official.ResponsesAPIRequest, client *mimoweb.Client, token string, streamReq mimoweb.CompletionRequest) {
	var fullText string
	res := client.Complete(token, streamReq, func(delta mimoweb.Delta) { fullText += delta.Text })
	fullText = mimoweb.CleanCitations(fullText)
	if res.Err != "" && fullText == "" {
		c.JSON(502, gin.H{"error": res.Err})
		return
	}
	outResp := official.NewResponsesResponse(fullText, "", countInputChars(req), util.CountToken(fullText), 0, 0, 0, req.Model)
	c.JSON(200, outResp)
}

func (d *Mimo) chatCompletions(c *gin.Context, m *mimoModel, req *official.APIRequest) {
	client := d.webClient()
	if client == nil || !client.HasToken() {
		c.JSON(502, gin.H{"error": "mimo web client unavailable: missing MIMO_WEB_TOKENS?"})
		return
	}
	token := client.NextToken()
	prompt := flattenChatInputAPI(req)
	streamReq := mimoweb.CompletionRequest{Prompt: prompt, Model: mimoModelCode}
	if req.Stream {
		d.chatCompletionsStream(c, req, client, token, streamReq)
		return
	}
	d.chatCompletionsNonStream(c, req, client, token, streamReq)
}

func (d *Mimo) chatCompletionsStream(c *gin.Context, req *official.APIRequest, client *mimoweb.Client, token string, streamReq mimoweb.CompletionRequest) {
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
	_ = client.Complete(token, streamReq, func(delta mimoweb.Delta) {
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

func (d *Mimo) chatCompletionsNonStream(c *gin.Context, req *official.APIRequest, client *mimoweb.Client, token string, streamReq mimoweb.CompletionRequest) {
	var fullText string
	res := client.Complete(token, streamReq, func(delta mimoweb.Delta) { fullText += delta.Text })
	// 最终兜底:流式 cleaner 帧边界之外的 citation 残留整体剥一次
	fullText = mimoweb.CleanCitations(fullText)
	if res.Err != "" && fullText == "" {
		c.JSON(502, gin.H{"error": res.Err})
		return
	}
	outResp := official.NewChatCompletionWithMetadataAndReasoning(fullText, "", countMessagesChars(req.Messages), util.CountToken(fullText), req.Model, "", nil)
	c.JSON(200, outResp)
}

// ─── coding 变体(围栏 JSON + FenceParser) ──────────────────────────

// mimoCodingPrompt 组装 coding prompt(围栏 JSON 协议,与 GLM/Gemini/Claude 同款)。
func mimoCodingPromptFromResponses(req *official.ResponsesAPIRequest) string {
	var sb strings.Builder
	if len(req.Tools) > 0 {
		sb.WriteString(glmBuildInstructions(req.Tools, req.ToolChoice))
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
			text = "```json\n" + text + "\n```"
		case "function_call_output":
			text = "Tool result: " + text
		}
		if text == "" {
			continue
		}
		sb.WriteString(text)
		sb.WriteString("\n\n")
	}
	if len(req.Tools) > 0 && lastItemIsToolResult(items) {
		sb.WriteString(glmCodingNudge())
	}
	return strings.TrimSpace(sb.String())
}

func mimoCodingPromptFromAPI(req *official.APIRequest) string {
	var sb strings.Builder
	if len(req.Tools) > 0 {
		sb.WriteString(glmBuildInstructions(req.Tools, req.ToolChoice))
		sb.WriteString("\n\n")
	}
	for _, msg := range req.Messages {
		if len(msg.ToolCalls) > 0 {
			for _, tc := range msg.ToolCalls {
				sb.WriteString("```json\n" + tc.Function.Arguments + "\n```\n\n")
			}
			continue
		}
		if role := msg.Role; role == "tool" {
			sb.WriteString("Tool result: " + msg.Text() + "\n\n")
			continue
		}
		if t := msg.Text(); t != "" {
			sb.WriteString(t + "\n\n")
		}
	}
	return strings.TrimSpace(sb.String())
}

func (d *Mimo) codingResponses(c *gin.Context, m *mimoModel, req *official.ResponsesAPIRequest) {
	d.limiter.Wait() // 仅 coding 限频,chat 不限
	client := d.webClient()
	if client == nil || !client.HasToken() {
		c.JSON(502, gin.H{"error": "mimo web client unavailable: missing MIMO_WEB_TOKENS?"})
		return
	}
	token := client.NextToken()
	prompt := mimoCodingPromptFromResponses(req)
	streamReq := mimoweb.CompletionRequest{Prompt: prompt, Model: mimoModelCode}
	if req.Stream {
		d.codingResponsesStream(c, req, client, token, streamReq)
		return
	}
	d.codingResponsesNonStream(c, req, client, token, streamReq)
}

func (d *Mimo) codingResponsesStream(c *gin.Context, req *official.ResponsesAPIRequest, client *mimoweb.Client, token string, streamReq mimoweb.CompletionRequest) {
	w := newSSEWriter(c)
	respID := "resp_" + uuid.NewString()
	messageItemID := "msg_" + uuid.NewString()
	w.event("response.created", createdEvent(respID, req.Model))
	w.event("response.output_item.added", outputItemAddedEvent(0, map[string]any{"id": messageItemID, "type": "message", "status": "in_progress", "role": "assistant"}))

	parser := toolcall.NewFenceParser(req.Tools)
	var textBuf strings.Builder
	var calls []official.ToolCall
	res := client.Complete(token, streamReq, func(delta mimoweb.Delta) {
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

func (d *Mimo) codingResponsesNonStream(c *gin.Context, req *official.ResponsesAPIRequest, client *mimoweb.Client, token string, streamReq mimoweb.CompletionRequest) {
	var fullText string
	res := client.Complete(token, streamReq, func(delta mimoweb.Delta) { fullText += delta.Text })
	fullText = mimoweb.CleanCitations(fullText)
	if res.Err != "" && fullText == "" {
		c.JSON(502, gin.H{"error": res.Err})
		return
	}
	parser := toolcall.NewFenceParser(req.Tools)
	_, calls := parser.Feed(fullText)
	calls = append(calls, parser.FlushCalls()...)
	calls = mergeRecoveredCalls(calls, fullText, req.Tools)
	cleanText := toolcall.StripFencedBlocks(fullText)
	outResp := official.NewResponsesResponse(cleanText, "", countInputChars(req), util.CountToken(cleanText), 0, 0, 0, req.Model)
	for _, tc := range calls {
		outResp.Output = append(outResp.Output, official.ResponsesOutputItem{
			ID: "fc_" + uuid.NewString(), Type: "function_call", Status: "completed",
			CallID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments,
		})
	}
	c.JSON(200, outResp)
}

func (d *Mimo) codingCompletions(c *gin.Context, m *mimoModel, req *official.APIRequest) {
	d.limiter.Wait() // 仅 coding 限频,chat 不限
	client := d.webClient()
	if client == nil || !client.HasToken() {
		c.JSON(502, gin.H{"error": "mimo web client unavailable: missing MIMO_WEB_TOKENS?"})
		return
	}
	token := client.NextToken()
	prompt := mimoCodingPromptFromAPI(req)
	streamReq := mimoweb.CompletionRequest{Prompt: prompt, Model: mimoModelCode}
	if req.Stream {
		d.codingCompletionsStream(c, req, client, token, streamReq)
		return
	}
	d.codingCompletionsNonStream(c, req, client, token, streamReq)
}

func (d *Mimo) codingCompletionsStream(c *gin.Context, req *official.APIRequest, client *mimoweb.Client, token string, streamReq mimoweb.CompletionRequest) {
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

	parser := toolcall.NewFenceParser(req.Tools)
	var textBuf strings.Builder
	var emittedCall bool
	_ = client.Complete(token, streamReq, func(delta mimoweb.Delta) {
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

func (d *Mimo) codingCompletionsNonStream(c *gin.Context, req *official.APIRequest, client *mimoweb.Client, token string, streamReq mimoweb.CompletionRequest) {
	var fullText string
	res := client.Complete(token, streamReq, func(delta mimoweb.Delta) { fullText += delta.Text })
	fullText = mimoweb.CleanCitations(fullText)
	if res.Err != "" && fullText == "" {
		c.JSON(502, gin.H{"error": res.Err})
		return
	}
	parser := toolcall.NewFenceParser(req.Tools)
	_, calls := parser.Feed(fullText)
	calls = append(calls, parser.FlushCalls()...)
	calls = mergeRecoveredCalls(calls, fullText, req.Tools)
	cleanText := toolcall.StripFencedBlocks(fullText)
	outResp := official.NewChatCompletionWithToolCalls(cleanText, "", calls, countMessagesChars(req.Messages), util.CountToken(cleanText), req.Model, "", nil)
	c.JSON(200, outResp)
}
