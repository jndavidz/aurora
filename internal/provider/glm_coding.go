package provider

import (
	"net/http"
	"strings"

	"aurora/internal/glmweb"
	"aurora/internal/toolcall"
	"aurora/typings/official"
	"aurora/util"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// codingResponses 处理智谱 coding 变体(/v1/responses)。
//
// 智谱模型(chatglm.cn)是"全部工具智能体",不认 ChatGPT <tool_call> 标签,
// 也不认 DeepSeek <|tool▁calls▁begin|>;实测输出 markdown 围栏 JSON。
// 因此 coding 变体用 glmBuildInstructions(围栏格式指令)+ FenceParser(流式拦截围栏)。
func (d *Glm) codingResponses(c *gin.Context, m *glmModel, req *official.ResponsesAPIRequest) {
	d.limiter.Wait() // 仅 coding 限频,chat 不限
	client := d.webClient()
	if err := d.ensureToken(client); err != nil {
		c.JSON(502, gin.H{"error": err.Error()})
		return
	}
	messages := glmCodingMessagesFromResponses(req)
	streamReq := glmweb.CompletionRequest{
		Messages:     messages,
		ChatMode:     "thinking",
		IsNetworking: false,
	}
	if req.Stream {
		d.codingResponsesStream(c, req, client, streamReq)
		return
	}
	d.codingResponsesNonStream(c, req, client, streamReq)
}

// glmCodingMessagesFromResponses 组装 coding messages:工具指令 + 对话历史。
func glmCodingMessagesFromResponses(req *official.ResponsesAPIRequest) []glmweb.Message {
	var msgs []glmweb.Message
	if len(req.Tools) > 0 {
		inst := glmBuildInstructions(req.Tools, req.ToolChoice)
		msgs = append(msgs, glmweb.Message{Role: "user", Content: []glmweb.Content{{Type: "text", Text: inst}}})
	}
	items := responsesInputItems(req.Input)
	if instr := rawResponsesText(req.Instructions); instr != "" {
		msgs = append(msgs, glmweb.Message{Role: "user", Content: []glmweb.Content{{Type: "text", Text: instr}}})
	}
	for _, it := range items {
		role := it.Role
		if role == "" || role == "system" {
			role = "user"
		}
		text := it.Text
		switch it.Type {
		case "function_call":
			text = "```json\n" + text + "\n```"
			role = "assistant"
		case "function_call_output":
			text = "Tool result: " + text
			role = "user"
		}
		if text == "" {
			continue
		}
		msgs = append(msgs, glmweb.Message{Role: role, Content: []glmweb.Content{{Type: "text", Text: text}}})
	}
	if len(msgs) == 0 {
		msgs = append(msgs, glmweb.Message{Role: "user", Content: []glmweb.Content{{Type: "text", Text: ""}}})
	}
	// 末尾强提醒:防"工具链断裂"——拿到工具结果后不继续调工具,只输出计划。
	if len(req.Tools) > 0 && lastItemIsToolResult(items) {
		msgs = append(msgs, glmweb.Message{Role: "user", Content: []glmweb.Content{{Type: "text", Text: glmCodingNudge()}}})
	}
	return msgs
}

// codingResponsesStream 流式 Responses,用 FenceParser 拦截围栏 JSON。
func (d *Glm) codingResponsesStream(c *gin.Context, req *official.ResponsesAPIRequest, client *glmweb.Client, streamReq glmweb.CompletionRequest) {
	resp, err := client.Complete(streamReq)
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

	parser := toolcall.NewFenceParser(req.Tools)
	var textBuf strings.Builder
	var calls []official.ToolCall
	_ = glmweb.ConsumeStream(resp.Body, func(delta glmweb.Delta) {
		// 智谱原生 tool_calls(结构化 content):只转发客户端声明过的工具。
		// 模型是"全部工具智能体",会优先调用自己的内置沙箱(execute_sandbox_code 等),
		// 这些调用客户端执行不了,必须过滤掉。
		if delta.ToolCall != nil {
			if toolNameInList(delta.ToolCall.Name, req.Tools) {
				calls = append(calls, official.ToolCall{
					ID:   delta.ToolCall.ID,
					Type: "function",
					Function: official.ToolCallFunc{
						Name:      delta.ToolCall.Name,
						Arguments: delta.ToolCall.Arguments,
					},
				})
			}
			return
		}
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

	// 兜底:围栏解析漏掉的裸 JSON 工具调用(未加围栏时)。
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

// codingResponsesNonStream 非流式。
func (d *Glm) codingResponsesNonStream(c *gin.Context, req *official.ResponsesAPIRequest, client *glmweb.Client, streamReq glmweb.CompletionRequest) {
	resp, err := client.Complete(streamReq)
	if err != nil {
		c.JSON(502, gin.H{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	var fullText string
	var nativeCalls []official.ToolCall
	_ = glmweb.ConsumeStream(resp.Body, func(delta glmweb.Delta) {
		if delta.ToolCall != nil {
			nativeCalls = append(nativeCalls, official.ToolCall{
				ID:   delta.ToolCall.ID,
				Type: "function",
				Function: official.ToolCallFunc{
					Name:      delta.ToolCall.Name,
					Arguments: delta.ToolCall.Arguments,
				},
			})
			return
		}
		fullText += delta.Text
	})

	parser := toolcall.NewFenceParser(req.Tools)
	_, calls := parser.Feed(fullText)
	calls = append(calls, parser.FlushCalls()...)
	calls = mergeRecoveredCalls(calls, fullText, req.Tools)
	calls = mergeToolCalls(calls, nativeCalls, req.Tools)

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

// codingCompletions 处理智谱 coding 变体(/v1/chat/completions)。
func (d *Glm) codingCompletions(c *gin.Context, m *glmModel, req *official.APIRequest) {
	d.limiter.Wait() // 仅 coding 限频,chat 不限
	client := d.webClient()
	if err := d.ensureToken(client); err != nil {
		c.JSON(502, gin.H{"error": err.Error()})
		return
	}
	messages := glmCodingMessagesFromAPI(req)
	streamReq := glmweb.CompletionRequest{
		Messages:     messages,
		ChatMode:     "thinking",
		IsNetworking: false,
	}
	if req.Stream {
		d.codingCompletionsStream(c, req, client, streamReq)
		return
	}
	d.codingCompletionsNonStream(c, req, client, streamReq)
}

// glmCodingMessagesFromAPI 组装 chat.completions coding messages。
func glmCodingMessagesFromAPI(req *official.APIRequest) []glmweb.Message {
	var msgs []glmweb.Message
	if len(req.Tools) > 0 {
		inst := glmBuildInstructions(req.Tools, req.ToolChoice)
		msgs = append(msgs, glmweb.Message{Role: "user", Content: []glmweb.Content{{Type: "text", Text: inst}}})
	}
	for _, msg := range req.Messages {
		role := msg.Role
		if role == "system" {
			role = "user"
		}
		if len(msg.ToolCalls) > 0 {
			for _, tc := range msg.ToolCalls {
				msgs = append(msgs, glmweb.Message{Role: "assistant", Content: []glmweb.Content{{Type: "text", Text: "```json\n" + tc.Function.Arguments + "\n```"}}})
			}
			continue
		}
		if role == "tool" {
			msgs = append(msgs, glmweb.Message{Role: "user", Content: []glmweb.Content{{Type: "text", Text: "Tool result: " + msg.Text()}}})
			continue
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

// codingCompletionsStream 流式 chat.completion.chunk + tool_calls delta。
func (d *Glm) codingCompletionsStream(c *gin.Context, req *official.APIRequest, client *glmweb.Client, streamReq glmweb.CompletionRequest) {
	resp, err := client.Complete(streamReq)
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

	parser := toolcall.NewFenceParser(req.Tools)
	var textBuf strings.Builder
	var emittedCall bool
	_ = glmweb.ConsumeStream(resp.Body, func(delta glmweb.Delta) {
		// 智谱原生 tool_calls(结构化 content):只转发客户端声明过的工具
		if delta.ToolCall != nil {
			if toolNameInList(delta.ToolCall.Name, req.Tools) {
				emittedCall = true
				tc := official.ToolCall{
					ID:   delta.ToolCall.ID,
					Type: "function",
					Function: official.ToolCallFunc{
						Name:      delta.ToolCall.Name,
						Arguments: delta.ToolCall.Arguments,
					},
				}
				for _, dd := range toolcall.StreamToToolCallDeltas([]official.ToolCall{tc}) {
					writeChunk(official.NewToolCallChunk(model, dd...))
				}
			}
			return
		}
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
func (d *Glm) codingCompletionsNonStream(c *gin.Context, req *official.APIRequest, client *glmweb.Client, streamReq glmweb.CompletionRequest) {
	resp, err := client.Complete(streamReq)
	if err != nil {
		c.JSON(502, gin.H{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	var fullText string
	var nativeCalls []official.ToolCall
	_ = glmweb.ConsumeStream(resp.Body, func(delta glmweb.Delta) {
		if delta.ToolCall != nil {
			nativeCalls = append(nativeCalls, official.ToolCall{
				ID:   delta.ToolCall.ID,
				Type: "function",
				Function: official.ToolCallFunc{
					Name:      delta.ToolCall.Name,
					Arguments: delta.ToolCall.Arguments,
				},
			})
			return
		}
		fullText += delta.Text
	})

	parser := toolcall.NewFenceParser(req.Tools)
	_, calls := parser.Feed(fullText)
	calls = append(calls, parser.FlushCalls()...)
	calls = mergeRecoveredCalls(calls, fullText, req.Tools)
	calls = mergeToolCalls(calls, nativeCalls, req.Tools)

	cleanText := toolcall.StripFencedBlocks(fullText)
	outResp := official.NewChatCompletionWithToolCalls(cleanText, "", calls, countMessagesChars(req.Messages), util.CountToken(cleanText), req.Model, "", nil)
	c.JSON(200, outResp)
}

// mergeToolCalls 合并原生 tool_calls 与文本协议解析结果,按 name+arguments 去重。
// 原生调用只保留客户端声明过的工具(过滤智谱内置沙箱工具)。
func mergeToolCalls(parsed, native []official.ToolCall, tools []official.Tool) []official.ToolCall {
	out := parsed
	seen := make(map[string]bool, len(parsed))
	for _, c := range parsed {
		seen[c.Function.Name+"\x00"+c.Function.Arguments] = true
	}
	for _, c := range native {
		if !toolNameInList(c.Function.Name, tools) {
			continue
		}
		key := c.Function.Name + "\x00" + c.Function.Arguments
		if !seen[key] {
			seen[key] = true
			out = append(out, c)
		}
	}
	return out
}

// toolNameInList 报告工具名是否在客户端声明列表中。
func toolNameInList(name string, tools []official.Tool) bool {
	for _, t := range tools {
		if t.Type == "function" && t.Function.Name == name {
			return true
		}
	}
	return false
}
