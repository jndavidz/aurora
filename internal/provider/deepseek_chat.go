package provider

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"aurora/internal/apierrors"
	"aurora/internal/deepseekweb"
	"aurora/internal/provider/session"
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

// searchEnabled 返回请求是否带联网搜索。
// 2026-09-27 起 exposed id "deepseek" 的定义即「智能搜索 + 非深度思考」,
// SearchAlways=true 恒开搜索,不受 DEEPSEEK_WEB_SEARCH 影响(其余模型按开关)。
// 有图时恒为 false(带图时上游忽略搜索开关)。
func (d *DeepSeek) searchEnabled(m *deepseekModel, hasImages bool) bool {
	if hasImages {
		return false // 识图与搜索互斥
	}
	if m.SearchAlways {
		return true
	}
	return m.Mode == modeQuick && d.cfg.DeepSeekWebSearch
}

func (d *DeepSeek) chatResponses(c *gin.Context, m *deepseekModel, req *official.ResponsesAPIRequest) {
	client := d.webClient()
	if client == nil {
		apierrors.JSONError(c, 502, "api_error", "deepseek web client unavailable: missing DEEPSEEK_WEB_TOKENS?", nil, "upstream_error")
		return
	}
	token := client.NextToken()
	if token == "" {
		apierrors.JSONError(c, 502, "api_error", "deepseek web token pool is empty", nil, "upstream_error")
		return
	}

	// 识图:提取 input 里的图片,上传并 fork 成 vision 版。
	// 带图 → model_type=vision,不进会话池(spec 拍板:与搜索互斥、
	// model_type 特殊,维持独立会话路径)。
	refFileIDs, _ := uploadImages(client, token, req)
	vision := len(refFileIDs) > 0

	// 会话策略 ticket 02:clientKey 解析(头 → user 字段),空 = 不进池。
	clientKey, isNew := session.ResolveClientKey(c.GetHeader("X-Session-Key"), c.GetHeader("X-Session-Action"), req.User)
	var pool *session.Pool
	if d.sessionPool != nil {
		pool = d.sessionPool
	} else {
		pool = session.NewPool(deepseekUpstream{client}, session.PoolConfig{CleanupInterval: 10 * time.Minute})
		d.sessionPool = pool
	}

	var turns []deepseekTurn
	for _, it := range responsesInputItems(req.Input) {
		if it.Text != "" {
			turns = append(turns, deepseekTurn{Text: it.Text})
		}
	}

	modelType := modelTypeFor(m)
	if vision {
		modelType = "vision"
	}

	flow := &deepseekChatFlow{
		pool:      pool,
		sender:    &deepseekWebSender{client: client, searchEnabled: d.searchEnabled(m, vision), thinking: thinkingEnabled(m, req)},
		clientKey: clientKey,
		isNew:     isNew,
		vision:    vision,
		modelType: modelType,
		token:     token,
		messages:  turns,
	}
	// 新会话引导 instructions 前置(仅拍平路径生效,续轮忽略)。
	flow.instructions = rawResponsesText(req.Instructions)

	// 流式逐帧 flush 需要帧序化回调:不走 flow.run(整流消费),改由
	// streamChatTurn 直接穿过 seam(acquire → send → 逐帧 flush → release)。
	if req.Stream {
		d.chatStreamTurn(c, m, req, flow)
		return
	}
	out, err := flow.run()
	if err != nil {
		apierrors.JSONError(c, 502, "api_error", err.Error(), nil, "upstream_error")
		return
	}
	d.chatNonStreamReplay(c, m, req, out)
}

// chatStreamTurn 流式一轮:穿过与 flow.run 相同的 seam,但增量在 delta
// 回调里实时 flush(SSE 首字延迟来自真流式,不能等整流结束)。
// 池交互(acquire/降级/release)与 flow.run 同型。
func (d *DeepSeek) chatStreamTurn(c *gin.Context, m *deepseekModel, req *official.ResponsesAPIRequest, flow *deepseekChatFlow) {
	w := newSSEWriter(c)
	respID := "resp_" + uuid.NewString()
	reasoningItemID := "rs_" + uuid.NewString()
	messageItemID := "msg_" + uuid.NewString()

	begin := func() {
		w.event("response.created", createdEvent(respID, req.Model))
		w.event("response.output_item.added", outputItemAddedEvent(0, map[string]any{"id": reasoningItemID, "type": "reasoning", "status": "in_progress"}))
		w.event("response.output_item.added", outputItemAddedEvent(1, map[string]any{"id": messageItemID, "type": "message", "status": "in_progress", "role": "assistant"}))
	}

	runOnce := func(l *session.Lease) (*deepseekStreamResult, bool) {
		stream, err := flow.sender.Send(flow.token, flow.buildRequest(l))
		if err != nil {
			return nil, false
		}
		var fullText, fullReasoning string
		// seamConsume 对 live 流逐帧回调(帧序化):delta 到达即 flush SSE,
		// 首字延迟来自真流式;测试预置流则一次性回放。
		res := seamConsume(stream, func(dd deepseekDelta) {
			if dd.Reasoning != "" {
				fullReasoning += dd.Reasoning
				w.event("response.reasoning_text.delta", map[string]any{
					"type": "response.reasoning_text.delta", "item_id": reasoningItemID,
					"output_index": 0, "content_index": 0, "delta": dd.Reasoning,
				})
			}
			if dd.Text != "" {
				fullText += dd.Text
				w.event("response.output_text.delta", map[string]any{
					"type": "response.output_text.delta", "item_id": messageItemID,
					"output_index": 1, "content_index": 0, "delta": dd.Text,
				})
			}
		})
		if res.Err != "" && fullText == "" && fullReasoning == "" {
			return res, false
		}
		return res, true
	}

	poolable := flow.clientKey != "" && !flow.vision
	var l *session.Lease
	if poolable {
		var err error
		if flow.isNew {
			l, err = flow.pool.AcquireNew(flow.clientKey, flow.modelID())
		} else {
			l, err = flow.pool.Acquire(flow.clientKey, flow.modelID())
		}
		if err != nil {
			apierrors.JSONError(c, 502, "api_error", err.Error(), nil, "upstream_error")
			return
		}
	}

	begin()
	res, ok := runOnce(l)
	if !ok && poolable {
		// 流式已可能吐出事件;失败降级重试一次,输出继续追加(SSE 消费端兼容)。
		flow.pool.Discard(l)
		l3, cerr := flow.pool.AcquireNew(flow.clientKey, flow.modelID())
		if cerr != nil {
			w.event("response.failed", failedEvent(cerr.Error()))
			return
		}
		res, ok = runOnce(l3)
		l = l3
	}
	if !ok {
		w.event("response.failed", failedEvent(res.Err))
		if poolable {
			flow.pool.Discard(l)
		}
		return
	}
	if poolable {
		flow.pool.Release(l, res.ResponseMsgID)
	}

	w.event("response.output_item.done", outputItemDoneEvent(0, reasoningItem(reasoningItemID, res.Reasoning, "completed")))
	w.event("response.output_item.done", outputItemDoneEvent(1, messageItem(messageItemID, res.Text, "completed")))

	outResp := official.NewResponsesResponse(res.Text, res.Reasoning, countInputChars(req), util.CountToken(res.Text), util.CountToken(res.Reasoning), 0, 0, req.Model)
	w.event("response.completed", completedEvent(outResp))
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

// deepseekWebSender 把 flow 的发送请求映射到真实网页协议(含 PoW);
// 消费由 seamConsume 统一承担(live 流逐帧回调)。
type deepseekWebSender struct {
	client        *deepseekweb.Client
	searchEnabled bool
	thinking      bool
}

func (s *deepseekWebSender) Send(token string, r deepseekSenderReq) (*preConsumedStream, error) {
	// 无池请求(无 X-Session-Key / vision):自建独立会话,一轮一建一删
	// (等价旧行为);池请求的 SessionID 由租约提供,不在此处建删。
	sessionID := r.SessionID
	var cleanup func()
	if sessionID == "" {
		sid, err := s.client.CreateSession(token)
		if err != nil {
			return nil, err
		}
		sessionID = sid
		cleanup = func() { _ = s.client.DeleteSession(token, sid) }
	}
	resp, err := s.client.Complete(token, deepseekweb.CompletionRequest{
		SessionID:       sessionID,
		ParentMessageID: r.ParentMessageID,
		Prompt:          r.Prompt,
		ModelType:       r.ModelType,
		ThinkingEnabled: s.thinking,
		SearchEnabled:   s.searchEnabled,
		RefFileIDs:      r.RefFileIDs,
	})
	if err != nil {
		if cleanup != nil {
			cleanup() // 自建会话未消费即败:顺手删,不悬挂
		}
		return nil, err
	}
	return &preConsumedStream{live: resp, cleanup: cleanup}, nil
}

// deepseekUpstream 适配 session.upstream(池淘汰/降级时删上游 session)。
type deepseekUpstream struct{ client *deepseekweb.Client }

func (u deepseekUpstream) CreateSession(token string) (string, error) {
	return u.client.CreateSession(token)
}

func (u deepseekUpstream) DeleteSession(token, sessionID string) error {
	return u.client.DeleteSession(token, sessionID)
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
//   - chat 变体:完全忽略 tools/tool_choice
//   - 不加 "User:"/"Assistant:" 前缀:网页真实请求的 prompt 是纯文本(角色锚点
//     由模型专用 token 承担),实测加前缀会被模型当成乱码/怪文本。
//   - 多轮 history 直接拼接(网页服务端按 session+parent_message_id 记忆,
//     aurora 每请求新会话,需全量提交)
func flattenChatInput(req *official.ResponsesAPIRequest, quickMode bool) string {
	return flattenChatItems(responsesInputItems(req.Input), rawResponsesText(req.Instructions))
}

// flattenChatInputAPI 从 chat.completions messages 拍平 prompt(同上,共享实现)。
func flattenChatInputAPI(req *official.APIRequest) string {
	return flattenChatItems(apiMessagesToItems(req.Messages), "")
}

// flattenChatItems 双接口共享的 chat prompt 构建(纯文本,跳过工具 item)。
func flattenChatItems(items []responsesInputItem, instructions string) string {
	var sb strings.Builder
	if instructions != "" {
		sb.WriteString(instructions)
		sb.WriteString("\n\n")
	}
	for _, item := range items {
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

// chatStreamReplay 流式回放一轮已完成的 flow 结果。
// 上游已整流消费完毕,无逐帧可推 —— 一次性下发全部 delta 后收尾(SSE
// 事件形态与旧路径一致,客户端解析无感;真实逐帧 flush 见流式分支)。
func (d *DeepSeek) chatStreamReplay(c *gin.Context, m *deepseekModel, req *official.ResponsesAPIRequest, out deepseekTurnOutput) {
	w := newSSEWriter(c)
	respID := "resp_" + uuid.NewString()
	reasoningItemID := "rs_" + uuid.NewString()
	messageItemID := "msg_" + uuid.NewString()

	w.event("response.created", createdEvent(respID, req.Model))
	w.event("response.output_item.added", outputItemAddedEvent(0, map[string]any{"id": reasoningItemID, "type": "reasoning", "status": "in_progress"}))
	w.event("response.output_item.added", outputItemAddedEvent(1, map[string]any{"id": messageItemID, "type": "message", "status": "in_progress", "role": "assistant"}))

	if out.reasoning != "" {
		w.event("response.reasoning_text.delta", map[string]any{
			"type": "response.reasoning_text.delta", "item_id": reasoningItemID,
			"output_index": 0, "content_index": 0, "delta": out.reasoning,
		})
	}
	if out.text != "" {
		w.event("response.output_text.delta", map[string]any{
			"type": "response.output_text.delta", "item_id": messageItemID,
			"output_index": 1, "content_index": 0, "delta": out.text,
		})
	}

	w.event("response.output_item.done", outputItemDoneEvent(0, reasoningItem(reasoningItemID, out.reasoning, "completed")))
	w.event("response.output_item.done", outputItemDoneEvent(1, messageItem(messageItemID, out.text, "completed")))

	outResp := official.NewResponsesResponse(out.text, out.reasoning, countInputChars(req), util.CountToken(out.text), util.CountToken(out.reasoning), 0, 0, req.Model)
	w.event("response.completed", completedEvent(outResp))
}

// chatNonStreamReplay 非流式回放一轮已完成的 flow 结果。
func (d *DeepSeek) chatNonStreamReplay(c *gin.Context, m *deepseekModel, req *official.ResponsesAPIRequest, out deepseekTurnOutput) {
	outResp := official.NewResponsesResponse(out.text, out.reasoning, countInputChars(req), util.CountToken(out.text), util.CountToken(out.reasoning), 0, 0, req.Model)
	c.JSON(200, outResp)
}

// ── /v1/chat/completions 支持(输出 chat.completion 格式)──

// chatCompletions 处理 DeepSeek chat 变体(/v1/chat/completions)。
// 与 chatResponses 同一套上游逻辑(session/PoW/SSE),仅输出格式不同。
func (d *DeepSeek) chatCompletions(c *gin.Context, m *deepseekModel, req *official.APIRequest) {
	client := d.webClient()
	if client == nil {
		apierrors.JSONError(c, 502, "api_error", "deepseek web client unavailable: missing DEEPSEEK_WEB_TOKENS?", nil, "upstream_error")
		return
	}
	token := client.NextToken()
	if token == "" {
		apierrors.JSONError(c, 502, "api_error", "deepseek web token pool is empty", nil, "upstream_error")
		return
	}

	prompt := flattenChatInputAPI(req)

	sessionID, err := client.CreateSession(token)
	if err != nil {
		apierrors.JSONError(c, 502, "api_error", fmt.Sprintf("deepseek create session: %v", err), nil, "upstream_error")
		return
	}
	defer client.DeleteSession(token, sessionID)

	refFileIDs, _ := uploadImagesFromMessages(client, token, req.Messages)
	modelType := modelTypeFor(m)
	if len(refFileIDs) > 0 {
		modelType = "vision"
	}
	streamReq := deepseekweb.CompletionRequest{
		SessionID:       sessionID,
		Prompt:          prompt,
		ModelType:       modelType,
		ThinkingEnabled: thinkingEnabledAPI(m, req),
		SearchEnabled:   d.searchEnabled(m, len(refFileIDs) > 0),
		RefFileIDs:      refFileIDs,
	}
	if req.Stream {
		d.chatCompletionsStream(c, m, req, client, token, streamReq)
		return
	}
	d.chatCompletionsNonStream(c, m, req, client, token, streamReq)
}

// thinkingEnabledAPI chat.completions 版的深度思考开关(reasoning_effort)。
func thinkingEnabledAPI(m *deepseekModel, req *official.APIRequest) bool {
	if m.Mode != modeExpert {
		return false
	}
	if req.ReasoningEffort != "" && req.ReasoningEffort == "none" {
		return false
	}
	return true
}

// chatCompletionsStream 流式输出 chat.completion.chunk SSE。
func (d *DeepSeek) chatCompletionsStream(c *gin.Context, m *deepseekModel, req *official.APIRequest, client *deepseekweb.Client, token string, streamReq deepseekweb.CompletionRequest) {
	resp, err := client.Complete(token, streamReq)
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

	// role 块
	roleChunk := official.NewChatCompletionChunk("", model)
	roleChunk.Choices[0].Delta.Role = "assistant"
	writeChunk(roleChunk)

	_ = deepseekweb.ConsumeStream(resp.Body, func(delta deepseekweb.Delta) {
		if delta.Reasoning != "" {
			writeChunk(official.NewReasoningChunk(delta.Reasoning, model))
		}
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

// chatCompletionsNonStream 非流式返回 ChatCompletion JSON。
func (d *DeepSeek) chatCompletionsNonStream(c *gin.Context, m *deepseekModel, req *official.APIRequest, client *deepseekweb.Client, token string, streamReq deepseekweb.CompletionRequest) {
	resp, err := client.Complete(token, streamReq)
	if err != nil {
		apierrors.JSONError(c, 502, "api_error", err.Error(), nil, "upstream_error")
		return
	}
	defer resp.Body.Close()

	var fullText, fullReasoning string
	res := deepseekweb.ConsumeStream(resp.Body, func(delta deepseekweb.Delta) {
		fullText += delta.Text
		fullReasoning += delta.Reasoning
	})
	if res.Err != "" && fullText == "" && fullReasoning == "" {
		apierrors.JSONError(c, 502, "api_error", res.Err, nil, "upstream_error")
		return
	}
	inputTokens := countMessagesChars(req.Messages)
	outResp := official.NewChatCompletionWithMetadataAndReasoning(fullText, fullReasoning, inputTokens, util.CountToken(fullText), req.Model, "", nil)
	c.JSON(200, outResp)
}

// countMessagesChars 粗略统计 messages 字符数(代替 token 统计)。
func countMessagesChars(messages []official.APIMessage) int {
	n := 0
	for _, msg := range messages {
		n += len(msg.Text())
	}
	return n
}
