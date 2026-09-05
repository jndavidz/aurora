package provider

import (
	"net/http"
	"strings"

	"aurora/internal/apierrors"
	"aurora/internal/kimiweb"
	"aurora/internal/toolcall"
	"aurora/typings/official"
	"aurora/util"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// codingResponses 处理 Kimi coding 变体(/v1/responses)。
//
// Kimi K2.6 快速模式的硬约束(见 docs/KIMI.md §六):
//   - Chat RPC 的 ToolType 枚举没有 FUNCTION,客户端自定义工具服务器不注册,
//     模型也拒绝假装调用(工具诚实度极高),因此 GLM 式文本协议在此无效;
//   - 模型自带原生工具(ipython 代码执行 / web_search 等),服务端执行,
//     结果经 block.tool 流回;
//   - 因此 coding 变体 = 工具上下文注入(尽力而为)+ 原生工具透传(仅转发
//     客户端声明过的同名工具,其余静默——模型已把结果折进正文)。
func (d *Kimi) codingResponses(c *gin.Context, m *kimiModel, req *official.ResponsesAPIRequest) {
	d.limiter.Wait() // 仅 coding 限频,chat 不限
	client := d.webClient()
	if err := d.ensureToken(client); err != nil {
		apierrors.JSONError(c, 502, "api_error", err.Error(), nil, "upstream_error")
		return
	}
	text := kimiCodingResponsesText(req)
	streamReq := kimiweb.CompletionRequest{
		Text:     text,
		Thinking: true,
	}
	if req.Stream {
		d.codingResponsesStream(c, req, client, streamReq)
		return
	}
	d.codingResponsesNonStream(c, req, client, streamReq)
}

// kimiCodingResponsesText 组装 coding 单条消息文本:工具指令 + 对话历史。
func kimiCodingResponsesText(req *official.ResponsesAPIRequest) string {
	var sb strings.Builder
	if len(req.Tools) > 0 {
		sb.WriteString(kimiBuildInstructions(req.Tools))
		sb.WriteString("\n")
	}
	if instr := rawResponsesText(req.Instructions); instr != "" {
		sb.WriteString(instr)
		sb.WriteString("\n")
	}
	kimiFlattenItems(&sb, responsesInputItems(req.Input), false) // coding 保留工具 item
	return strings.TrimRight(sb.String(), "\n")
}

// kimiBuildInstructions 生成 Kimi coding 变体的工具上下文指令。
//
// 定位:模型无法真正调用客户端工具,但能看到上下文;指令明确说明工具由
// agent 框架代执行,模型应把工具意图写进正文,或使用自带能力(ipython 等)。
func kimiBuildInstructions(tools []official.Tool) string {
	var sb strings.Builder
	sb.WriteString("You are a coding assistant working with an agent framework.\n")
	sb.WriteString("\nThe user's environment exposes these tools (an external agent framework executes them for you):\n")
	sb.WriteString(toolcall.CompactToolsPrompt(tools))
	sb.WriteString("\n\nRules:\n")
	sb.WriteString("- If a task requires one of these tools, reason in text about which tool and arguments you would use; the agent will act on it.\n")
	sb.WriteString("- If you can compute or reason yourself (including your built-in code execution), just do it and answer directly.\n")
	return sb.String()
}

// codingResponsesStream 流式 Responses:转发文本 + 客户端声明过的原生工具调用。
func (d *Kimi) codingResponsesStream(c *gin.Context, req *official.ResponsesAPIRequest, client *kimiweb.Client, streamReq kimiweb.CompletionRequest) {
	resp, err := client.Complete(streamReq)
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

	var textBuf strings.Builder
	var calls []official.ToolCall
	_ = kimiweb.ConsumeStream(resp.Body, func(delta kimiweb.Delta) {
		// 原生工具调用:只转发客户端声明过的工具(过滤 ipython 等内置能力)。
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
		textBuf.WriteString(delta.Text)
		w.event("response.output_text.delta", map[string]any{
			"type": "response.output_text.delta", "item_id": messageItemID,
			"output_index": 0, "content_index": 0, "delta": delta.Text,
		})
	})

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
func (d *Kimi) codingResponsesNonStream(c *gin.Context, req *official.ResponsesAPIRequest, client *kimiweb.Client, streamReq kimiweb.CompletionRequest) {
	resp, err := client.Complete(streamReq)
	if err != nil {
		apierrors.JSONError(c, 502, "api_error", err.Error(), nil, "upstream_error")
		return
	}
	defer resp.Body.Close()
	var fullText string
	var calls []official.ToolCall
	res := kimiweb.ConsumeStream(resp.Body, func(delta kimiweb.Delta) {
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
		fullText += delta.Text
	})
	if res.Err != "" && fullText == "" {
		apierrors.JSONError(c, 502, "api_error", res.Err, nil, "upstream_error")
		return
	}
	outResp := official.NewResponsesResponse(fullText, "", countInputChars(req), util.CountToken(fullText), 0, 0, 0, req.Model)
	for _, tc := range calls {
		outResp.Output = append(outResp.Output, official.ResponsesOutputItem{
			ID: "fc_" + uuid.NewString(), Type: "function_call", Status: "completed",
			CallID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments,
		})
	}
	c.JSON(200, outResp)
}

// codingCompletions 处理 Kimi coding 变体(/v1/chat/completions)。
func (d *Kimi) codingCompletions(c *gin.Context, m *kimiModel, req *official.APIRequest) {
	d.limiter.Wait() // 仅 coding 限频,chat 不限
	client := d.webClient()
	if err := d.ensureToken(client); err != nil {
		apierrors.JSONError(c, 502, "api_error", err.Error(), nil, "upstream_error")
		return
	}
	text := kimiCodingMessagesText(req)
	streamReq := kimiweb.CompletionRequest{
		Text:     text,
		Thinking: true,
	}
	if req.Stream {
		d.codingCompletionsStream(c, req, client, streamReq)
		return
	}
	d.codingCompletionsNonStream(c, req, client, streamReq)
}

// kimiCodingMessagesText 组装 chat.completions coding 单条消息文本。
func kimiCodingMessagesText(req *official.APIRequest) string {
	var sb strings.Builder
	if len(req.Tools) > 0 {
		sb.WriteString(kimiBuildInstructions(req.Tools))
		sb.WriteString("\n")
	}
	kimiFlattenAPIMessages(&sb, req.Messages, false) // coding 保留工具消息
	return strings.TrimRight(sb.String(), "\n")
}

// codingCompletionsStream 流式 chat.completion.chunk + tool_calls delta。
func (d *Kimi) codingCompletionsStream(c *gin.Context, req *official.APIRequest, client *kimiweb.Client, streamReq kimiweb.CompletionRequest) {
	resp, err := client.Complete(streamReq)
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

	var emittedCall bool
	_ = kimiweb.ConsumeStream(resp.Body, func(delta kimiweb.Delta) {
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
		writeChunk(official.NewChatCompletionChunk(delta.Text, model))
	})
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
func (d *Kimi) codingCompletionsNonStream(c *gin.Context, req *official.APIRequest, client *kimiweb.Client, streamReq kimiweb.CompletionRequest) {
	resp, err := client.Complete(streamReq)
	if err != nil {
		apierrors.JSONError(c, 502, "api_error", err.Error(), nil, "upstream_error")
		return
	}
	defer resp.Body.Close()
	var fullText string
	var calls []official.ToolCall
	res := kimiweb.ConsumeStream(resp.Body, func(delta kimiweb.Delta) {
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
		fullText += delta.Text
	})
	if res.Err != "" && fullText == "" {
		apierrors.JSONError(c, 502, "api_error", res.Err, nil, "upstream_error")
		return
	}
	outResp := official.NewChatCompletionWithToolCalls(fullText, "", calls, countMessagesChars(req.Messages), util.CountToken(fullText), req.Model, "", nil)
	c.JSON(200, outResp)
}
