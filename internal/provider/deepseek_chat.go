package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"aurora/internal/apierrors"
	"aurora/internal/deepseekweb"
	"aurora/internal/provider/session"
	"aurora/internal/toolcall"
	"aurora/typings/official"
	"aurora/util"

	"github.com/gin-gonic/gin"
)

// chatResponses 处理 DeepSeek chat 变体(/v1/responses)。
//
// 硬规则:上游只发「真人对话」形态的请求 —— 剥离客户端 tools/tool_choice,
// 不注入任何工具调用信息;仅携带网页模式开关(快速/专家、智能搜索、深度思考、识图)。
// 识图(快速模式)与联网搜索互斥(DeepSeek 网页行为)。

// searchEnabled 返回请求是否带联网搜索。
// 2026-09-27 起 exposed id "deepseek" 的定义即「智能搜索 + 非深度思考」,
// SearchAlways=true 恒开搜索,不受 DEEPSEEK_WEB_SEARCH 影响(其余模型按开关)。
// 2026-10-26 起 SearchNever=true 恒关搜索(免搜索变体),同样不受开关影响。
// 有图时恒为 false(带图时上游忽略搜索开关)。
func (d *DeepSeek) searchEnabled(m *deepseekModel, hasImages bool) bool {
	if hasImages {
		return false // 识图与搜索互斥
	}
	if m.SearchAlways {
		return true
	}
	if m.SearchNever {
		return false
	}
	return m.Mode == modeQuick && d.cfg.DeepSeekWebSearch
}

func (d *DeepSeek) chatResponses(c *gin.Context, m *deepseekModel, req *official.ResponsesAPIRequest) {
	client := d.webClient()
	if client == nil {
		apierrors.JSONError(c, 502, "api_error", "deepseek web client unavailable: missing DEEPSEEK_WEB_TOKENS?", nil, "upstream_error")
		return
	}
	// 会话策略 ticket 02:clientKey 解析(头 → user 字段),空 = 不进池。
	clientKey, isNew := session.ResolveClientKey(c.GetHeader("X-Session-Key"), c.GetHeader("X-Session-Action"), req.User)
	// token 按 clientKey 确定性选取:同一会话键落到同一账号(session 与
	// token 绑定,轮询会令池命中恒为 0);空 key 回落轮询,单发负载均衡。
	token := client.TokenFor(clientKey)
	if token == "" {
		apierrors.JSONError(c, 502, "api_error", "deepseek web token pool is empty", nil, "upstream_error")
		return
	}

	// 识图:提取 input 里的图片,上传并 fork 成 vision 版。
	// 带图 → model_type=vision,不进会话池(spec 拍板:与搜索互斥、
	// model_type 特殊,维持独立会话路径)。
	refFileIDs, _ := uploadImages(client, token, req)
	vision := len(refFileIDs) > 0

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
// 竞态守卫:DeepSeek 是单例,并发首访必须只建一次池——否则旧池与其后台
// 清理 goroutine 泄漏,且两批请求各用各的池(同 clientKey 无法复用)。
func (d *DeepSeek) webPool(client *deepseekweb.Client) *session.Pool {
	d.oncePool.Do(func() {
		d.sessionPool = session.NewPool(deepseekUpstream{client}, session.PoolConfig{})
	})
	return d.sessionPool
}

// deepseekTurnsFromItems 把统一 item 列表收敛为本轮拍平输入。
// 图片走独立 vision 路径,这里只取文本。Responses / chat.completions 两表面共用。
//
// 工具历史(T02)按 ChatGPT 通道已验证的文本协议翻译,而不是丢弃:
//   - function_call → `<tool_call>{"name":...,"arguments":...}</tool_call>`
//     (toolcall.SerializeForHistory,模型据此"记得"自己调过什么)
//   - function_call_output → `Tool (Resultado da ferramenta <name>): <content>`
//     (toolcall 系统提示以 "Tool (...)" 块识别真实工具输出,换措辞会削弱遵循率)
//
// 硬规则不变:不下发 req.Tools(上游只收真人对话形态的 prompt,工具参数
// 绝不出现);工具历史只是对话文本的一部分。
func deepseekTurnsFromItems(items []responsesInputItem) []deepseekTurn {
	var turns []deepseekTurn
	// callNames 记录本轮 function_call 的 call_id → 工具名:OpenAI 的
	// role=tool 消息只带 tool_call_id,靠它反查才能拼出 "Tool (...)" 前缀。
	callNames := make(map[string]string)
	for _, it := range items {
		switch it.Type {
		case "function_call":
			ref, ok := toolCallRefFromItem(it)
			if !ok {
				continue // 解不出 name/arguments 的历史调用跳过,不污染 prompt
			}
			callNames[ref.ID] = ref.Function.Name
			turns = append(turns, deepseekTurn{Text: strings.TrimSpace(toolcall.SerializeForHistory([]official.ToolCallRef{ref}))})
		case "function_call_output":
			turns = append(turns, deepseekTurn{Text: formatToolResult(toolResultName(it, callNames), it.Text)})
		default:
			if it.Text != "" {
				turns = append(turns, deepseekTurn{Text: it.Text})
			}
		}
	}
	return turns
}

// 工具结果回灌上限与截断后缀(与 ChatGPT 通道同口径:防长结果挤占上下文)。
const (
	toolResultMaxBytes      = 4096
	toolResultTruncatedTail = "…（已截断）"
)

// toolResultName 决定 "Tool (...)" 前缀里的工具名:item 自带 name 优先,
// 其次按 call_id 反查本轮的 function_call,都拿不到回落 "tool"。
func toolResultName(it responsesInputItem, callNames map[string]string) string {
	if it.Name != "" {
		return it.Name
	}
	if it.CallID != "" {
		if name, ok := callNames[it.CallID]; ok && name != "" {
			return name
		}
	}
	return "tool"
}

// formatToolResult 复刻 ChatGPT 通道的工具结果文本
// (typings/chatgpt 的 AddToolMessage),超限按字节截断并加后缀。
func formatToolResult(name, content string) string {
	if len(content) > toolResultMaxBytes {
		// ToValidUTF8 顺带去掉截断处的半个多字节字符(ASCII 结果仍是 4096 字节)。
		content = strings.ToValidUTF8(content[:toolResultMaxBytes], "") + toolResultTruncatedTail
	}
	return "Tool (Resultado da ferramenta " + name + "): " + content
}

// toolCallRefFromItem 从 function_call item 里还原 call_id / name / arguments。
// item.Text 是 apiMessagesToItems / parseInputItem 复刻的围栏形状:
//
//	{"type":"tool_calls","tool_calls":{"name":"read","arguments":"{\"path\":\"a\"}"}}
//
// arguments 既可能是 JSON 字符串(OpenAI 协议)也可能是对象(容错),
// 统一还原成原始 JSON 文本供 SerializeForHistory 拼装。解不出返回 false。
func toolCallRefFromItem(it responsesInputItem) (official.ToolCallRef, bool) {
	var payload struct {
		ToolCalls struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal([]byte(it.Text), &payload); err != nil {
		return official.ToolCallRef{}, false
	}
	name := payload.ToolCalls.Name
	if name == "" {
		name = it.Name // Responses 表面单带 name 时兜底
	}
	if name == "" {
		return official.ToolCallRef{}, false
	}
	ref := official.ToolCallRef{Index: 0, ID: it.CallID, Type: "function"}
	ref.Function.Name = name
	ref.Function.Arguments = toolCallArguments(payload.ToolCalls.Arguments)
	return ref, true
}

// toolCallArguments 把 item 里的 arguments 还原成原始 JSON 文本:
// JSON 字符串 → 解包后的内容;对象/数组 → 原样;缺省/空串 → "{}"
// (空串会拼出 `"arguments": }` 这种坏 JSON,模型无法照抄)。
func toolCallArguments(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return "{}"
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if strings.TrimSpace(s) == "" {
			return "{}"
		}
		return s
	}
	return string(raw)
}

// chatStreamTurn 流式一轮:穿过与 flow.run 相同的 seam,但增量在 delta
// 回调里实时 flush(SSE 首字延迟来自真流式,不能等整流结束)。
// 池交互(acquire/降级/release)与 flow.run 同型。
func (d *DeepSeek) chatStreamTurn(c *gin.Context, m *deepseekModel, req *official.ResponsesAPIRequest, flow *deepseekChatFlow) {
	// 建会话失败在 SSE 头设置前返回:保持干净的 JSON 502(建会话失败非续轮失败)。
	l, err := flow.acquireLease()
	if err != nil {
		apierrors.JSONError(c, 502, "api_error", err.Error(), nil, "upstream_error")
		return
	}

	e := NewResponsesEmitter(c, req.Model, true)
	out, _, rerr := flow.runStreaming(l, func(dd deepseekDelta) {
		e.ReasoningDelta(dd.Reasoning)
		e.TextDelta(dd.Text)
	})
	if rerr != nil {
		e.Fail(rerr.Error())
		return
	}
	outResp := official.NewResponsesResponse(out.text, out.reasoning, countInputChars(req), util.CountToken(out.text), util.CountToken(out.reasoning), 0, 0, req.Model)
	e.Complete(outResp, out.text, out.reasoning)
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
		apierrors.JSONError(c, 502, "api_error", errDeepSeekWebUnavailable.Error(), nil, "upstream_error")
		return
	}
	// 会话策略 ticket 04:clientKey 解析与 Responses 表面同一规则
	// (X-Session-Key 头 → user 字段剥 #new# → 空不进池),信令两表面统一。
	clientKey, isNew := session.ResolveClientKey(c.GetHeader("X-Session-Key"), c.GetHeader("X-Session-Action"), req.User)
	flow, err := d.newChatFlow(client, m, req, clientKey, isNew)
	if err != nil {
		apierrors.JSONError(c, 502, "api_error", err.Error(), nil, "upstream_error")
		return
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

// errDeepSeekWebUnavailable / errDeepSeekTokenPoolEmpty 是两个 chat 表面共用的
// 干净 502 错误(文案与历史一致,对客户端可见)。
var (
	errDeepSeekWebUnavailable = errors.New("deepseek web client unavailable: missing DEEPSEEK_WEB_TOKENS?")
	errDeepSeekTokenPoolEmpty = errors.New("deepseek web token pool is empty")
)

// 编译期断言:DeepSeek 支持工具模拟所需的「非流式取回全文」。
var _ TextCollector = (*DeepSeek)(nil)

// CollectText 实现 TextCollector:非流式跑一轮 chat 变体,返回正文
// (不含 reasoning),不写 gin.Context。
//
// 会话必须每轮新建:续轮路径(buildRequest)只发最后一条 message,注入的
// 工具指令/系统片段会被整段丢弃 → clientKey 恒置空(不进池、一轮一建一删)。
// 硬规则保留:req.Tools 不透传上游(发出去的只是真人对话形态的 prompt)。
// req.Stream 被忽略 —— 工具模拟要等全文到齐才能解析。
func (d *DeepSeek) CollectText(c *gin.Context, req *official.APIRequest) (string, error) {
	client := d.webClient()
	if client == nil {
		return "", errDeepSeekWebUnavailable
	}
	m, ok := d.byID[req.Model]
	if !ok || m.Variant != variantChat {
		return "", fmt.Errorf("deepseek: model %q is not a chat model", req.Model)
	}
	flow, err := d.newChatFlow(client, m, req, "", false)
	if err != nil {
		return "", err
	}
	out, err := flow.run()
	if err != nil {
		return "", err
	}
	return out.text, nil
}

// newChatFlow 构造 chat.completions 形态的一轮 flow(chatCompletions 与
// CollectText 共用):token 选取、识图上传、网页 model_type 判定与 flow 装配。
// clientKey 为空 = 不进池(一轮一建一删)。
func (d *DeepSeek) newChatFlow(client *deepseekweb.Client, m *deepseekModel, req *official.APIRequest, clientKey string, isNew bool) (*deepseekChatFlow, error) {
	token := client.TokenFor(clientKey)
	if token == "" {
		return nil, errDeepSeekTokenPoolEmpty
	}

	// 识图:从 messages 里收集图片上传并 fork 成 vision 版;带图不进池。
	refFileIDs, _ := uploadImagesFromMessages(client, token, req.Messages)
	vision := len(refFileIDs) > 0

	modelType := modelTypeFor(m)
	if vision {
		modelType = "vision"
	}

	return &deepseekChatFlow{
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
	}, nil
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
	// 建会话失败在 SSE 头设置前:保持干净的 JSON 502。
	l, err := flow.acquireLease()
	if err != nil {
		apierrors.JSONError(c, 502, "api_error", err.Error(), nil, "upstream_error")
		return
	}

	model := req.Model
	if model == "" {
		model = "auto"
	}
	// 头部与 role 块均延迟到首个增量时才写出(ChunkStream deferred 模式):
	// 降级重试对客户端不可见的同时,保留旧路径的失败形态——尚未写出任何
	// 内容时的上游失败仍走干净的 JSON 502。
	// 池交互/零增量降级/归还全部在 flow.runStreaming 单点(spec WP1)。
	s := NewDeferredChunkStream(c, model)
	_, _, rerr := flow.runStreaming(l, func(dd deepseekDelta) {
		s.ReasoningDelta(dd.Reasoning)
		s.TextDelta(dd.Text)
	})
	if rerr != nil {
		writeCompletionError(s, rerr)
		return
	}

	// 空响应(上游未吐任何增量但不报错):补建流头与 role 块,与旧路径形态一致。
	s.Stop("stop")
}

// writeCompletionError 统一流式失败出口:
//   - 尚未开出 SSE(ChunkStream 未 started):干净的 JSON 502(保留旧路径对无 key/
//     建会话失败的行为,客户端拿到的是结构化错误而非半截流)
//   - 已开流:发一个 OpenAI 形态的 data: {"error": ...} 帧再以 [DONE] 收尾
func writeCompletionError(s *ChunkStream, err error) {
	if !s.Started() {
		// 未开流的干净 JSON 502 由 handler 的 status 旁敲记熔断,无需重复上报。
		apierrors.JSONError(s.c, 502, "api_error", err.Error(), nil, "upstream_error")
		return
	}
	// 已开流的错误帧对 status 旁敲不可见,这里上报熔断器(WP6)。
	ReportStreamFailure(s.c, err.Error())
	b, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": err.Error(), "type": "upstream_error"},
	})
	s.c.Writer.WriteString("data: " + string(b) + "\n\n")
	s.Done()
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
