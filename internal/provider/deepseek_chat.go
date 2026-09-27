package provider

import (
	"encoding/json"
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
	pool := d.webPool(client)

	modelType := modelTypeFor(m)
	if vision {
		modelType = "vision"
	}

	flow := &deepseekChatFlow{
		pool:       pool,
		sender:     &deepseekWebSender{client: client, searchEnabled: d.searchEnabled(m, vision), thinking: thinkingEnabled(m, req)},
		clientKey:  clientKey,
		isNew:      isNew,
		vision:     vision,
		noResume:   d.resumeDisabled(m),
		modelType:  modelType,
		token:      token,
		messages:   deepseekTurnsFromItems(responsesInputItems(req.Input)),
		refFileIDs: refFileIDs,
	}
	// 新会话引导 instructions 前置(仅拍平路径生效,续轮忽略)。
	flow.instructions = rawResponsesText(req.Instructions)

	// 流式逐帧 flush 需要帧序化回调:不走 flow.run(整流消费),改由
	// chatStreamTurn 直接穿过 seam(acquire → send → 逐帧 flush → release)。
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

// webPool 惰性构造并复用服务生命周期共享的会话池。
func (d *DeepSeek) webPool(client *deepseekweb.Client) *session.Pool {
	if d.sessionPool == nil {
		d.sessionPool = session.NewPool(deepseekUpstream{client}, session.PoolConfig{CleanupInterval: 10 * time.Minute})
	}
	return d.sessionPool
}

// deepseekTurnsFromItems 把统一 item 列表收敛为本轮拍平输入(仅取文本;
// 图片走独立 vision 路径)。Responses / chat.completions 两表面共用。
//
// 硬规则:chat 变体绝不向上游注入任何工具信息——function_call /
// function_call_output 与 flattenChatItems 同口径跳过(防御性:客户端带了
// tools 也不得泄漏到 prompt)。
func deepseekTurnsFromItems(items []responsesInputItem) []deepseekTurn {
	var turns []deepseekTurn
	for _, it := range items {
		switch it.Type {
		case "function_call", "function_call_output":
			continue
		}
		if it.Text != "" {
			turns = append(turns, deepseekTurn{Text: it.Text})
		}
	}
	return turns
}

// chatStreamTurn 流式一轮:穿过与 flow.run 相同的 seam,但增量在 delta
// 回调里实时 flush(SSE 首字延迟来自真流式,不能等整流结束)。
// 池交互(acquire/降级/release)与 flow.run 同型。
func (d *DeepSeek) chatStreamTurn(c *gin.Context, m *deepseekModel, req *official.ResponsesAPIRequest, flow *deepseekChatFlow) {
	respID := "resp_" + uuid.NewString()
	reasoningItemID := "rs_" + uuid.NewString()
	messageItemID := "msg_" + uuid.NewString()

	// 建会话失败在 SSE 头设置前返回:保持干净的 JSON 502(建会话失败非续轮失败)。
	poolable := flow.poolable()
	var l *session.Lease
	if poolable {
		var err error
		if flow.isNew {
			l, err = flow.pool.AcquireNew(flow.clientKey, flow.modelID(), flow.token)
		} else {
			l, err = flow.pool.Acquire(flow.clientKey, flow.modelID(), flow.token)
		}
		if err != nil {
			apierrors.JSONError(c, 502, "api_error", err.Error(), nil, "upstream_error")
			return
		}
	}

	w := newSSEWriter(c)

	begin := func() {
		w.event("response.created", createdEvent(respID, req.Model))
		w.event("response.output_item.added", outputItemAddedEvent(0, map[string]any{"id": reasoningItemID, "type": "reasoning", "status": "in_progress"}))
		w.event("response.output_item.added", outputItemAddedEvent(1, map[string]any{"id": messageItemID, "type": "message", "status": "in_progress", "role": "assistant"}))
	}

	runOnce := func(l *session.Lease) (*deepseekStreamResult, error) {
		stream, err := flow.sender.Send(flow.token, flow.buildRequest(l))
		if err != nil {
			return nil, err
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
		if res != nil && res.Err != "" && fullText == "" && fullReasoning == "" {
			return res, flowError(res.Err)
		}
		return res, nil
	}

	begin()
	res, runErr := runOnce(l)
	if runErr != nil && poolable {
		// 失败降级(spec §失败降级):丢弃失败租约 → 新开重试一次。
		// 上报失败时不会 flush 任何 delta(正文与思维链皆空),故重试对客户端透明。
		flow.pool.Discard(l)
		l3, cerr := flow.pool.AcquireNew(flow.clientKey, flow.modelID(), flow.token)
		if cerr != nil {
			w.event("response.failed", failedEvent(cerr.Error()))
			return
		}
		res, runErr = runOnce(l3)
		l = l3
	}
	if runErr != nil {
		w.event("response.failed", failedEvent(runErr.Error()))
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

// resumeDisabled 报告本档是否禁用会话复用(ticket 05 回退开关)。
// expert 档由 DEEPSEEK_EXPERT_RESUME 控制(默认复用);quick 档恒复用。
func (d *DeepSeek) resumeDisabled(m *deepseekModel) bool {
	return m.Mode == modeExpert && !d.cfg.DeepSeekExpertResume
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

	// 识图:从 messages 里收集图片上传并 fork 成 vision 版;带图不进池。
	refFileIDs, _ := uploadImagesFromMessages(client, token, req.Messages)
	vision := len(refFileIDs) > 0

	// 会话策略 ticket 04:clientKey 解析与 Responses 表面同一规则
	// (X-Session-Key 头 → user 字段剥 #new# → 空不进池),信令两表面统一。
	clientKey, isNew := session.ResolveClientKey(c.GetHeader("X-Session-Key"), c.GetHeader("X-Session-Action"), req.User)

	modelType := modelTypeFor(m)
	if vision {
		modelType = "vision"
	}

	flow := &deepseekChatFlow{
		pool: d.webPool(client),
		// chat.completions 的思考开关走 reasoning_effort(thinkingEnabledAPI)。
		sender:     &deepseekWebSender{client: client, searchEnabled: d.searchEnabled(m, vision), thinking: thinkingEnabledAPI(m, req)},
		clientKey:  clientKey,
		isNew:      isNew,
		vision:     vision,
		noResume:   d.resumeDisabled(m),
		modelType:  modelType,
		token:      token,
		messages:   deepseekTurnsFromItems(apiMessagesToItems(req.Messages)),
		refFileIDs: refFileIDs,
	}

	if req.Stream {
		d.chatStreamCompletion(c, m, req, flow)
		return
	}
	out, err := flow.run()
	if err != nil {
		apierrors.JSONError(c, 502, "api_error", err.Error(), nil, "upstream_error")
		return
	}
	d.chatReplayCompletion(c, req, out)
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

// chatStreamCompletion 流式输出 chat.completion.chunk。
// 与 Responses 表面的 chatStreamTurn 同型:穿过同一 seam(acquire → send →
// 逐帧 flush → release),增量在 delta 回调里实时 flush。
// 失败降级对客户端透明:重试成功则只可见一条完整正常流。
func (d *DeepSeek) chatStreamCompletion(c *gin.Context, m *deepseekModel, req *official.APIRequest, flow *deepseekChatFlow) {
	poolable := flow.poolable()
	var l *session.Lease
	if poolable {
		var err error
		if flow.isNew {
			l, err = flow.pool.AcquireNew(flow.clientKey, flow.modelID(), flow.token)
		} else {
			l, err = flow.pool.Acquire(flow.clientKey, flow.modelID(), flow.token)
		}
		if err != nil {
			// 建会话失败在 SSE 头设置前:保持干净的 JSON 502。
			apierrors.JSONError(c, 502, "api_error", err.Error(), nil, "upstream_error")
			return
		}
	}

	model := req.Model
	if model == "" {
		model = "auto"
	}
	flusher, _ := c.Writer.(http.Flusher)
	// 头部与 role 块均延迟到首个增量时才写出:降级重试对客户端不可见的同时,
	// 保留旧路径的失败形态——尚未写出任何内容时的上游失败仍走干净的 JSON 502。
	started := false
	writeChunk := func(chunk official.ChatCompletionChunk) {
		c.Writer.WriteString("data: " + chunk.String() + "\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
	begin := func() {
		if started {
			return
		}
		started = true
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Writer.Header().Set("Connection", "keep-alive")
		c.Writer.Header().Set("X-Accel-Buffering", "no")
		// role 块(与旧路径一致:客户端先收到 assistant 角色)。
		roleChunk := official.NewChatCompletionChunk("", model)
		roleChunk.Choices[0].Delta.Role = "assistant"
		writeChunk(roleChunk)
	}

	// runOnce 发一轮并把增量实时吐成 chunk。仅在"一个增量都没吐"时才算失败
	// (与 flow.run 同判据),因此重试对客户端不可见。
	runOnce := func(l *session.Lease) (*deepseekStreamResult, error) {
		var emitted bool
		stream, err := flow.sender.Send(flow.token, flow.buildRequest(l))
		if err != nil {
			return nil, err
		}
		res := seamConsume(stream, func(dd deepseekDelta) {
			if dd.Reasoning != "" {
				emitted = true
				begin()
				writeChunk(official.NewReasoningChunk(dd.Reasoning, model))
			}
			if dd.Text != "" {
				emitted = true
				begin()
				writeChunk(official.NewChatCompletionChunk(dd.Text, model))
			}
		})
		if res != nil && res.Err != "" && !emitted {
			return res, flowError(res.Err)
		}
		return res, nil
	}

	res, runErr := runOnce(l)
	if runErr != nil && poolable {
		// 失败降级(spec §失败降级):丢弃失败租约 → 新开重试一次。
		flow.pool.Discard(l)
		l2, cerr := flow.pool.AcquireNew(flow.clientKey, flow.modelID(), flow.token)
		if cerr != nil {
			writeCompletionError(c, started, cerr)
			return
		}
		res, runErr = runOnce(l2)
		l = l2
	}
	if runErr != nil {
		if poolable {
			flow.pool.Discard(l)
		}
		writeCompletionError(c, started, runErr)
		return
	}
	if poolable {
		flow.pool.Release(l, res.ResponseMsgID)
	}

	// 空响应(上游未吐任何增量但不报错):补建流头与 role 块,与旧路径形态一致。
	begin()
	writeChunk(official.StopChunk("stop", model))
	c.Writer.WriteString("data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// writeCompletionError 统一流式失败出口:
//   - 尚未开出 SSE(started=false):干净的 JSON 502(保留旧路径对无 key/建会话
//     失败的行为,客户端拿到的是结构化错误而非半截流)
//   - 已开流:发一个 OpenAI 形态的 data: {"error": ...} 帧再以 [DONE] 收尾
func writeCompletionError(c *gin.Context, started bool, err error) {
	if !started {
		apierrors.JSONError(c, 502, "api_error", err.Error(), nil, "upstream_error")
		return
	}
	b, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": err.Error(), "type": "upstream_error"},
	})
	c.Writer.WriteString("data: " + string(b) + "\n\n")
	c.Writer.WriteString("data: [DONE]\n\n")
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
}

// chatReplayCompletion 非流式回放一轮已完成的 flow 结果(chat.completion 格式)。
func (d *DeepSeek) chatReplayCompletion(c *gin.Context, req *official.APIRequest, out deepseekTurnOutput) {
	inputTokens := countMessagesChars(req.Messages)
	outResp := official.NewChatCompletionWithMetadataAndReasoning(out.text, out.reasoning, inputTokens, util.CountToken(out.text), req.Model, "", nil)
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
