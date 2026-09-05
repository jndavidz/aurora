package provider

import (
	"net/http"
	"strings"

	"aurora/internal/apierrors"
	"aurora/internal/grokweb"
	"aurora/internal/toolcall"
	"aurora/typings/official"
	"aurora/util"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// codingResponses 处理 Grok coding 变体(/v1/responses)。
//
// 定位:云端沙盒代码执行助手(与智谱 glm-*-coding 同款,见 docs/GLM.md §四)。
// Grok 模型有远程沙盒(/home/workdir/artifacts),能执行 bash/python,
// 但不能访问用户本地文件,也不输出可解析的客户端工具调用(实测无 function_call 事件)。
// 客户端自定义工具作为"尽力而为"通道:提示词引导模型输出围栏 JSON,
// FenceParser 捕获;模型不输出时正常返回文本(沙盒执行结果)。
func (d *Grok) codingResponses(c *gin.Context, m *grokModel, req *official.ResponsesAPIRequest) {
	d.limiter.Wait() // 仅 coding 限频,chat 不限
	client := d.webClient()
	if client == nil || !client.HasAccount() {
		apierrors.JSONError(c, 502, "api_error", "grok web client unavailable: missing GROK_COOKIES?", nil, "upstream_error")
		return
	}
	prompt := grokCodingPromptFromResponses(req)
	streamReq := grokweb.CompletionRequest{Prompt: prompt}
	if req.Stream {
		d.codingResponsesStream(c, req, client, streamReq)
		return
	}
	d.codingResponsesNonStream(c, req, client, streamReq)
}

// grokCodingPromptFromResponses 组装 coding prompt:工具指令 + 对话历史。
func grokCodingPromptFromResponses(req *official.ResponsesAPIRequest) string {
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

// codingResponsesStream 流式 Responses。
func (d *Grok) codingResponsesStream(c *gin.Context, req *official.ResponsesAPIRequest, client *grokweb.Client, streamReq grokweb.CompletionRequest) {
	w := newSSEWriter(c)
	respID := "resp_" + uuid.NewString()
	messageItemID := "msg_" + uuid.NewString()
	w.event("response.created", createdEvent(respID, req.Model))
	w.event("response.output_item.added", outputItemAddedEvent(0, map[string]any{"id": messageItemID, "type": "message", "status": "in_progress", "role": "assistant"}))

	parser := toolcall.NewFenceParser(req.Tools)
	var textBuf strings.Builder
	var calls []official.ToolCall
	res := client.Complete(streamReq, func(delta grokweb.Delta) {
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
func (d *Grok) codingResponsesNonStream(c *gin.Context, req *official.ResponsesAPIRequest, client *grokweb.Client, streamReq grokweb.CompletionRequest) {
	var fullText string
	res := client.Complete(streamReq, func(delta grokweb.Delta) { fullText += delta.Text })
	clean, reasoning := splitGrokThinking(fullText)
	if res.Err != "" && clean == "" {
		apierrors.JSONError(c, 502, "api_error", res.Err, nil, "upstream_error")
		return
	}
	parser := toolcall.NewFenceParser(req.Tools)
	_, calls := parser.Feed(clean)
	calls = append(calls, parser.FlushCalls()...)
	calls = mergeRecoveredCalls(calls, clean, req.Tools)
	cleanText := toolcall.StripFencedBlocks(clean)
	outResp := official.NewResponsesResponse(cleanText, reasoning, countInputChars(req), util.CountToken(cleanText), util.CountToken(reasoning), 0, 0, req.Model)
	for _, tc := range calls {
		outResp.Output = append(outResp.Output, official.ResponsesOutputItem{
			ID: "fc_" + uuid.NewString(), Type: "function_call", Status: "completed",
			CallID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments,
		})
	}
	c.JSON(200, outResp)
}

// codingCompletions 处理 Grok coding 变体(/v1/chat/completions)。
func (d *Grok) codingCompletions(c *gin.Context, m *grokModel, req *official.APIRequest) {
	d.limiter.Wait() // 仅 coding 限频,chat 不限
	client := d.webClient()
	if client == nil || !client.HasAccount() {
		apierrors.JSONError(c, 502, "api_error", "grok web client unavailable: missing GROK_COOKIES?", nil, "upstream_error")
		return
	}
	prompt := grokCodingPromptFromAPI(req)
	streamReq := grokweb.CompletionRequest{Prompt: prompt}
	if req.Stream {
		d.codingCompletionsStream(c, req, client, streamReq)
		return
	}
	d.codingCompletionsNonStream(c, req, client, streamReq)
}

// grokCodingPromptFromAPI 组装 chat.completions coding prompt。
func grokCodingPromptFromAPI(req *official.APIRequest) string {
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

// codingCompletionsStream 流式 chat.completion.chunk。
func (d *Grok) codingCompletionsStream(c *gin.Context, req *official.APIRequest, client *grokweb.Client, streamReq grokweb.CompletionRequest) {
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
	res := client.Complete(streamReq, func(delta grokweb.Delta) {
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
	_ = res
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
func (d *Grok) codingCompletionsNonStream(c *gin.Context, req *official.APIRequest, client *grokweb.Client, streamReq grokweb.CompletionRequest) {
	var fullText string
	res := client.Complete(streamReq, func(delta grokweb.Delta) { fullText += delta.Text })
	clean, reasoning := splitGrokThinking(fullText)
	if res.Err != "" && clean == "" {
		apierrors.JSONError(c, 502, "api_error", res.Err, nil, "upstream_error")
		return
	}
	parser := toolcall.NewFenceParser(req.Tools)
	_, calls := parser.Feed(clean)
	calls = append(calls, parser.FlushCalls()...)
	calls = mergeRecoveredCalls(calls, clean, req.Tools)
	cleanText := toolcall.StripFencedBlocks(clean)
	outResp := official.NewChatCompletionWithToolCalls(cleanText, reasoning, calls, countMessagesChars(req.Messages), util.CountToken(cleanText), req.Model, "", nil)
	c.JSON(200, outResp)
}
