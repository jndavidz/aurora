package handler

// provider_tools.go —— 方案 A:provider 工具模拟(handler 侧全部逻辑)。
//
// 网页逆向上游没有原生 tool_calls:把工具协议注入 system,让模型以
// <tool_call>{...}</tool_call> 文本输出,网关解析后包装成标准 tool_calls。
// 本文件只提供入口与实现,不接管路由(接线在 chat_handler 分派点,ticket 05)。
//
// 与 ChatGPT 通道(tool_calling.go)的区别:
//   - 收集走 provider.TextCollector(非流式全文),不碰 ChatGPT 账号池/会话状态
//   - 重试的 nudge 是固定文本(不套 FinalNudge 的 coding 语义),按轮追加为 user 消息
//   - 不做无标签 JSON 恢复(RecoverFromText):宁可降级为文本,也不猜工具调用
//   - 重试耗尽降级为普通文本;仅当连非空文本都没有时才 502(上游错误同样 502)

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"aurora/internal/apierrors"
	"aurora/internal/config"
	"aurora/internal/provider"
	"aurora/internal/toolcall"
	officialtypes "aurora/typings/official"
	"aurora/util"

	"github.com/gin-gonic/gin"
)

// providerToolDefaultRetries 是负值/缺省配置的回退档(与 config 默认一致)。
const providerToolDefaultRetries = 3

// providerToolEmulationEnabled 报告本次请求是否走 provider 工具模拟。
//
// 总开关 ProviderToolEmulation 必须为真;ProviderToolModels 为空(或全是空白项)
// 表示不限模型,否则要求 model 精确命中白名单(空白项忽略)。
func providerToolEmulationEnabled(cfg *config.Config, model string) bool {
	if cfg == nil || !cfg.ProviderToolEmulation {
		return false
	}
	hasEntry := false
	for _, m := range cfg.ProviderToolModels {
		name := strings.TrimSpace(m)
		if name == "" {
			continue // 空白项忽略
		}
		hasEntry = true
		if name == model {
			return true
		}
	}
	// 白名单没有有效项 = 不限模型。
	return !hasEntry
}

// providerToolEmulationRequestEligible 是请求侧的额外门禁(配置门禁之外):
//   - tool_choice:"none" —— 客户端显式禁止工具,注入协议 + nudge 会强迫模型
//     违反契约,直接交回 provider 原路径;
//   - -coding 变体 —— 走原生 coding 流程(CollectText 对非 chat 变体返回 error,
//     若被模拟接管会 502),判定沿用 handler 既有的 isCodingModelName。
func providerToolEmulationRequestEligible(req *officialtypes.APIRequest) bool {
	if req == nil {
		return false
	}
	if req.ToolChoice.IsForcedNone() {
		return false
	}
	return !isCodingModelName(req.Model)
}

// rejectNativeModelWithoutTools:方案 B 槽位里的模型只接管「带 tools 的 chat
// 请求」,无 tools 时明确 400 invalid_request_error(两表面同文案)。否则请求
// 会落到 ChatGPT 兜底:消耗账号、按未知模型返回莫名 4xx(而槽位 id 又不在
// /v1/models 里,客户端无从得知原因)。返回 true 表示已写响应。
func (h *ChatHandler) rejectNativeModelWithoutTools(c *gin.Context, model string, hasTools bool) bool {
	if h.providers == nil || hasTools {
		return false
	}
	native := h.providers.NativeTools()
	if native == nil || !native.Handles(model) {
		return false
	}
	apierrors.JSONError(c, 400, "invalid_request_error",
		"模型 "+model+" 仅接受带 tools 的 chat 请求(原生工具透传槽位,不支持无工具对话);请改用其他模型或携带 tools",
		apierrors.Param("tools"), "missing_tools")
	return true
}

// handleProviderToolCalling 是方案 A 的主流程:
// 注入 → 收集(带退避重试)→ 解析(仅标签,不做无标签 JSON 猜测)→ 封装(tool_calls 或文本 / SSE)。
//
// tc 由调用方按模型选择(如 *provider.DeepSeek);本函数不写任何会话状态,
// 每次尝试都是一次独立收集。req 及其消息切片不被修改。
func (h *ChatHandler) handleProviderToolCalling(c *gin.Context, req *officialtypes.APIRequest, tc provider.TextCollector) {
	if req == nil || tc == nil {
		apierrors.JSONError(c, 502, "api_error", "provider tool emulation: missing request or collector", nil, "upstream_error")
		return
	}

	model := req.Model
	streamRequested := req.Stream
	// 延迟模式:头与 role 块在首个 delta 时才写,上游失败仍可回退干净 JSON 502。
	var stream *provider.ChunkStream
	if streamRequested {
		stream = provider.NewDeferredChunkStream(c, model)
	}

	injected := injectProviderToolProtocol(req)
	nudge := providerToolNudge(req.Tools)
	retries := providerToolRetryBudget(h.cfg)

	// 末条消息是工具结果(tool/function)且模型给出非空文本时,说明模型已经
	// 基于工具输出完成任务 —— 再 nudge 会逼它重复发起工具调用(重复点歌 /
	// 重复发红外),所以这一轮必须直接接受(与 tool_calling.go 的守卫同口径)。
	lastRole := ""
	if n := len(req.Messages); n > 0 {
		lastRole = req.Messages[n-1].Role
	}
	toolResultTurn := lastRole == "tool" || lastRole == "function"

	var (
		text         string
		lastNonEmpty string
		calls        []officialtypes.ToolCall
	)
	for attempt := 0; ; attempt++ {
		// 每轮从注入副本出发,重试轮只多一条 nudge user 消息(不累积、不污染)。
		attemptReq := injected
		if attempt > 0 {
			attemptReq = appendProviderToolNudge(injected, nudge)
		}
		out, err := tc.CollectText(c, attemptReq)
		if err != nil {
			writeProviderToolError(c, stream, err)
			return
		}
		text = out
		if strings.TrimSpace(out) != "" {
			lastNonEmpty = out // 重试耗尽时回退用(末轮可能为空)
		}
		calls = parseProviderToolCalls(out, req.Tools)
		// 先判「是否该接受」,再判「是否该重试」:
		//   1) 命中工具调用 → 接受
		//   2) 工具结果轮 + 非空文本 → 接受(不再催工具,见上)
		//   3) 重试预算耗尽 → 接受(降级为文本,空文本由下方统一转 502)
		if len(calls) > 0 || (toolResultTurn && strings.TrimSpace(out) != "") || attempt >= retries {
			break
		}
		// 重试前 context-aware 退避(防风控:连续重试会新开会话/重传图片,
		// 无间隔地打上游是最典型的机器化流量特征)。
		if !providerToolWait(c.Request.Context(), providerToolRetryDelay(attempt)) {
			return // 客户端已断开,不再白跑上游
		}
	}

	// 重试耗尽后保留「最后一个非空文本」:末轮空串时用它,全部尝试都空则
	// 502 —— 200 + content:"" 会被 bridge 判成「没收到回复」。
	if strings.TrimSpace(text) == "" {
		text = lastNonEmpty
	}
	if len(calls) == 0 && strings.TrimSpace(text) == "" {
		// 流是延迟模式,此时头尚未写出,两表面都走同一份干净 JSON 502。
		apierrors.JSONError(c, 502, "api_error", "provider tool emulation: empty reply", nil, "no_valid_reply")
		return
	}

	inputTokens := countMessagesTokens(req.Messages)
	outputTokens := util.CountToken(text)

	if streamRequested {
		if len(calls) > 0 {
			// 每个 delta 一帧(OpenAI 惯例:一帧一条 tool_call 增量)。
			for _, deltas := range toolcall.StreamToToolCallDeltas(calls) {
				stream.ToolCallDeltas(deltas...)
			}
			stream.StopToolCall() // finish_reason=tool_calls
			stream.Done()         // [DONE]
			return
		}
		stream.TextDelta(text)
		stream.Stop("stop") // stop 块 + [DONE]
		return
	}

	if len(calls) > 0 {
		// 构造器自动 finish_reason=tool_calls 并丢弃正文。
		c.JSON(200, officialtypes.NewChatCompletionWithToolCalls("", "", calls, inputTokens, outputTokens, model, "", nil))
		return
	}
	c.JSON(200, officialtypes.NewChatCompletionWithMetadata(text, inputTokens, outputTokens, model, "", nil))
}

// injectProviderToolProtocol 返回请求副本:工具协议按 ChatGPT 通道的写法包进
// <tool_calling_protocol>…</tool_calling_protocol>,并入单条 system 消息
// (有 system 就追加到其末尾,没有就插到最前)。调用方 req 不被修改。
func injectProviderToolProtocol(req *officialtypes.APIRequest) *officialtypes.APIRequest {
	out := *req
	out.Messages = append([]officialtypes.APIMessage(nil), req.Messages...)

	block := toolcall.BuildInstructions(req.Tools, req.ToolChoice)
	if strings.TrimSpace(block) == "" {
		return &out
	}
	protocol := "<tool_calling_protocol>\n" + block + "\n</tool_calling_protocol>"

	for i := range out.Messages {
		if out.Messages[i].Role == "system" {
			out.Messages[i] = appendMessageText(out.Messages[i], protocol)
			return &out
		}
	}
	out.Messages = append([]officialtypes.APIMessage{officialtypes.NewTextMessage("system", protocol)}, out.Messages...)
	return &out
}

// appendMessageText 在消息正文末尾追加一段文本(保留多模态 parts)。
func appendMessageText(m officialtypes.APIMessage, extra string) officialtypes.APIMessage {
	if len(m.Content.Parts) > 0 {
		parts := append([]officialtypes.MessageContentPart(nil), m.Content.Parts...)
		parts = append(parts, officialtypes.MessageContentPart{Type: "text", Text: extra})
		m.Content = officialtypes.MessageContent{Parts: parts}
		return m
	}
	text := m.Content.Text()
	if text != "" {
		text += "\n\n"
	}
	m.Content = officialtypes.MessageContent{TextValue: text + extra}
	return m
}

// appendProviderToolNudge 返回在副本末尾追加一条 nudge user 消息的请求。
// 工具结果之后追加 user 消息是合法的(客户端视角就是用户在催"继续")。
func appendProviderToolNudge(req *officialtypes.APIRequest, nudge string) *officialtypes.APIRequest {
	out := *req
	out.Messages = append(
		append([]officialtypes.APIMessage(nil), req.Messages...),
		officialtypes.NewTextMessage("user", nudge),
	)
	return &out
}

// providerToolNudge 是重试轮的固定提醒文本(非 coding 语义,不按 role 分流):
// 指出上一轮未按协议输出,列出本次可用工具名,并给"确实不需要工具"留退路。
func providerToolNudge(tools []officialtypes.Tool) string {
	var names []string
	for _, t := range tools {
		if t.Type == "function" && strings.TrimSpace(t.Function.Name) != "" {
			names = append(names, t.Function.Name)
		}
	}
	namesPart := ""
	if len(names) > 0 {
		namesPart = " The tools available in this session are: " + strings.Join(names, ", ") + "."
	}
	return "[SYSTEM INSTRUCTION: Your previous reply did not contain any <tool_call> block." + namesPart +
		" If the task requires reading data, running a command or otherwise using a tool, you MUST reply now with ONLY" +
		" the <tool_call>{\"name\": \"...\", \"arguments\": {...}}</tool_call> block(s) and nothing else — no prose," +
		" no explanation, no preamble. If you are confident that no tool is needed, just answer the user normally in plain text.]"
}

// providerToolRetryBudget 返回额外重试次数(总尝试 = 1 + retries)。
// 0 合法(只跑一次);负值回退默认档(配置层已归一,这里兜底防御)。
func providerToolRetryBudget(cfg *config.Config) int {
	if cfg == nil || cfg.ProviderToolRetries < 0 {
		return providerToolDefaultRetries
	}
	return cfg.ProviderToolRetries
}

// providerToolRetryDelay 返回第 attempt 轮失败后的重试退避:
// 400ms × (attempt+1),上限 2s。退避只在失败路径发生(成功轮不睡)。
func providerToolRetryDelay(attempt int) time.Duration {
	d := 400 * time.Millisecond * time.Duration(attempt+1)
	if d > 2*time.Second {
		d = 2 * time.Second
	}
	return d
}

// providerToolWait 是退避的实际执行点(包级变量,测试可替换以不真睡)。
// 返回 false 表示 context 已取消(客户端断开),调用方应立即退出。
var providerToolWait = func(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// parseProviderToolCalls 从全文解析工具调用:标签解析(Feed)→ 未闭合兜底(Flush)。
//
// 不做「无标签 JSON」恢复(RecoverFromText 的 InferToolFromParams):正文里任何
// 「key 恰好是某工具参数子集」的 JSON 都会被伪造成调用——例如模型解释
// 「song_request 需要 {"query":"歌曲名"}」,网关就会真的去点「歌曲名」。
// 宁可降级为文本,也不猜工具调用(与 ChatGPT 通道不同,这里刻意不共享该兜底)。
// 命中后按出现顺序重排 Index。
func parseProviderToolCalls(text string, tools []officialtypes.Tool) []officialtypes.ToolCall {
	parser := toolcall.NewParserWithTagsAndTools(toolcall.DefaultTags, tools)
	_, calls := parser.Feed(text)
	if len(calls) == 0 {
		_, extra := parser.Flush()
		calls = append(calls, extra...)
	}
	for i := range calls {
		calls[i].Index = i
	}
	return calls
}

// writeProviderToolError 是收集阶段的唯一错误出口:
//   - 流未开始(延迟流尚未写头):干净的 JSON 502
//   - 流已开始:OpenAI 形态的 error 帧 + [DONE] 收尾(与 provider 侧
//     writeCompletionError 同型;该 helper 在 provider 包内不可复用,本地等价实现)
func writeProviderToolError(c *gin.Context, stream *provider.ChunkStream, err error) {
	if stream == nil || !stream.Started() {
		apierrors.JSONError(c, 502, "api_error", err.Error(), nil, "upstream_error")
		return
	}
	b, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": err.Error(), "type": "upstream_error"},
	})
	c.Writer.WriteString("data: " + string(b) + "\n\n")
	stream.Done()
}
