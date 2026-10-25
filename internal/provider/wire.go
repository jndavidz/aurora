package provider

import (
	"net/http"

	"aurora/typings/official"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ─── wire emitters:两表面流式 wire format 的单点 ─────────────────────
//
// 此前同一套协议在 4 份平行词表上手搓:provider 侧 map 版事件构造(sse.go)、
// handler 侧 string 版(shared.go)、typed struct(chat_handler.go)与逐处
// 手拼四事件序列(tool_calling.go)——修一个格式 bug 要同步 20+ 处,且
// UTF-8 rune 安全切片只落在一处。两个 emitter 把「事件/chunk 序列骨架」收进
// 单一 implementation,各家只提供 delta 回调(spec .scratch/arch-hardening WP5)。
//
// 覆盖范围:chat 变体与 ChatGPT 兜底(活代码);coding 变体封存不动。

// sseHeaders 写 SSE 头四件套(newSSEWriter 与 ChunkStream 共用)。
func sseHeaders(w gin.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
}

// ChunkStream 发射 chat.completions 的 chunk 序列:
//
//	role 块 → (reasoning|text|tool_call delta)* → stop → [DONE]
//
// 两种头模式:立即(构造即写头+role,与各家旧序一致)与延迟(首个 delta 时才写,
// 供降级重试对客户端不可见、失败仍可走干净 JSON 502 的路径用,如 DeepSeek 会话复用)。
type ChunkStream struct {
	c        *gin.Context
	flusher  http.Flusher
	model    string
	started  bool
	deferred bool
}

// NewChunkStream 立即模式:构造即写 SSE 头与 role 块。
func NewChunkStream(c *gin.Context, model string) *ChunkStream {
	s := &ChunkStream{c: c, model: model, flusher: flusherOf(c)}
	s.ensureStarted()
	return s
}

// NewDeferredChunkStream 延迟模式:头与 role 块延迟到首个 chunk(降级重试
// 期间保持未开流状态,失败可回退 JSON 502)。
func NewDeferredChunkStream(c *gin.Context, model string) *ChunkStream {
	return &ChunkStream{c: c, model: model, deferred: true, flusher: flusherOf(c)}
}

func flusherOf(c *gin.Context) http.Flusher {
	f, _ := c.Writer.(http.Flusher)
	return f
}

func (s *ChunkStream) ensureStarted() {
	if s.started {
		return
	}
	s.started = true
	sseHeaders(s.c.Writer)
	// role 块(客户端先收到 assistant 角色,与旧路径一致)。
	role := official.NewChatCompletionChunk("", s.model)
	role.Choices[0].Delta.Role = "assistant"
	s.write(role)
}

func (s *ChunkStream) write(chunk official.ChatCompletionChunk) {
	s.c.Writer.WriteString("data: " + chunk.String() + "\n\n")
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// Started 报告头是否已写出(失败路径据此选择 error 帧或干净 JSON 502)。
func (s *ChunkStream) Started() bool { return s.started }

// TextDelta 发一个正文增量(延迟模式下自动触发头+role)。
func (s *ChunkStream) TextDelta(t string) {
	if t == "" {
		return
	}
	s.ensureStarted()
	s.write(official.NewChatCompletionChunk(t, s.model))
}

// ReasoningDelta 发一个思维链增量。
func (s *ChunkStream) ReasoningDelta(r string) {
	if r == "" {
		return
	}
	s.ensureStarted()
	s.write(official.NewReasoningChunk(r, s.model))
}

// ToolCallDeltas 发工具调用增量块。
func (s *ChunkStream) ToolCallDeltas(deltas ...official.ToolCallDelta) {
	s.ensureStarted()
	s.write(official.NewToolCallChunk(s.model, deltas...))
}

// StopToolCall 发工具调用收尾块(tools 路径的 stop 形态)。
func (s *ChunkStream) StopToolCall() {
	s.ensureStarted()
	s.write(official.NewToolCallStopChunk(s.model, ""))
}

// Stop 发 stop 块 + [DONE] + flush。
func (s *ChunkStream) Stop(reason string) {
	s.ensureStarted()
	s.write(official.StopChunk(reason, s.model))
	s.Done()
}

// Done 只发 [DONE] 收尾(error 帧后不追加 stop 块的路径用)。
func (s *ChunkStream) Done() {
	s.ensureStarted()
	s.c.Writer.WriteString("data: [DONE]\n\n")
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// ResponsesEmitter 发射 /v1/responses 的事件骨架:
//
//	created → added(reasoning?) → added(message) → delta* → done(reasoning?) → done(message) → completed
//
// withReasoning 决定是否存在 reasoning item(存在时 message 的 output_index 为 1)。
// 失败走 Fail(单事件收尾)。FunctionCall 发四事件序列(add/delta/done/item done)。
type ResponsesEmitter struct {
	w             *sseWriter
	model         string
	respID        string
	withReasoning bool
	reasoningID   string
	messageID     string
	msgIndex      int
}

// NewResponsesEmitter 构造并发出 created 与 output_item.added 序列。
func NewResponsesEmitter(c *gin.Context, model string, withReasoning bool) *ResponsesEmitter {
	e := &ResponsesEmitter{
		w:             newSSEWriter(c),
		model:         model,
		respID:        "resp_" + uuid.NewString(),
		withReasoning: withReasoning,
		reasoningID:   "rs_" + uuid.NewString(),
		messageID:     "msg_" + uuid.NewString(),
	}
	e.w.event("response.created", createdEvent(e.respID, model))
	if withReasoning {
		e.msgIndex = 1
		e.w.event("response.output_item.added", outputItemAddedEvent(0, map[string]any{
			"id": e.reasoningID, "type": "reasoning", "status": "in_progress",
		}))
	}
	e.w.event("response.output_item.added", outputItemAddedEvent(e.msgIndex, map[string]any{
		"id": e.messageID, "type": "message", "status": "in_progress", "role": "assistant",
	}))
	return e
}

// ReasoningDelta 发思维链增量(仅 withReasoning 构造时有效)。
func (e *ResponsesEmitter) ReasoningDelta(r string) {
	if r == "" || !e.withReasoning {
		return
	}
	e.w.event("response.reasoning_text.delta", map[string]any{
		"type": "response.reasoning_text.delta", "item_id": e.reasoningID,
		"output_index": 0, "content_index": 0, "delta": r,
	})
}

// TextDelta 发正文增量。
func (e *ResponsesEmitter) TextDelta(t string) {
	if t == "" {
		return
	}
	e.w.event("response.output_text.delta", map[string]any{
		"type": "response.output_text.delta", "item_id": e.messageID,
		"output_index": e.msgIndex, "content_index": 0, "delta": t,
	})
}

// FunctionCall 发一个 function_call item 的四事件序列
// (add in_progress → arguments delta → arguments done → item done)。
func (e *ResponsesEmitter) FunctionCall(outputIndex int, fcID, callID, name, arguments string) {
	e.w.event("response.output_item.added", outputItemAddedEvent(outputIndex, functionCallItem(fcID, callID, name, "", "in_progress")))
	e.w.event("response.function_call_arguments.delta", map[string]any{
		"type": "response.function_call_arguments.delta", "item_id": fcID,
		"output_index": outputIndex, "delta": arguments,
	})
	e.w.event("response.function_call_arguments.done", map[string]any{
		"type": "response.function_call_arguments.done", "item_id": fcID,
		"output_index": outputIndex, "arguments": arguments,
	})
	e.w.event("response.output_item.done", outputItemDoneEvent(outputIndex, functionCallItem(fcID, callID, name, arguments, "completed")))
}

// Complete 发 done(reasoning?) + done(message) + completed 并收尾。
// outResp 由调用方构造(usage/字符数各家口径不同);fullText/fullReasoning 进 done 事件的 item 全文。
func (e *ResponsesEmitter) Complete(outResp official.ResponsesResponse, fullText, fullReasoning string) {
	if e.withReasoning {
		e.w.event("response.output_item.done", outputItemDoneEvent(0, reasoningItem(e.reasoningID, fullReasoning, "completed")))
	}
	e.MessageDone(fullText)
	e.w.event("response.completed", completedEvent(outResp))
}

// MessageDone 只发 message item 的 done(带 function_call items 的路径:
// message 先收尾,后续 items 依次追加,最后 Completed 收尾)。
func (e *ResponsesEmitter) MessageDone(fullText string) {
	e.w.event("response.output_item.done", outputItemDoneEvent(e.msgIndex, messageItem(e.messageID, fullText, "completed")))
}

// Completed 只发 response.completed(FunctionCall 序列后的收尾;outResp 由调用方构造)。
func (e *ResponsesEmitter) Completed(outResp official.ResponsesResponse) {
	e.w.event("response.completed", completedEvent(outResp))
}

// Done 发 data: [DONE] 收尾行(ChatGPT 兜底两表面的历史形态;官方 Responses
// 协议无此行,provider 侧不发)。
func (e *ResponsesEmitter) Done() {
	e.w.c.Writer.WriteString("data: [DONE]\n\n")
	if e.w.flusher != nil {
		e.w.flusher.Flush()
	}
}

// Fail 发 response.failed 收尾(已发事件无法撤回,失败即终态)。
func (e *ResponsesEmitter) Fail(msg string) {
	e.w.event("response.failed", failedEvent(msg))
}
