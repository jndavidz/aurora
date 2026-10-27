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
//   - 重试耗尽降级为普通文本,绝不 5xx(上游错误除外)

import (
	"encoding/json"
	"strings"

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

// handleProviderToolCalling 是方案 A 的主流程:
// 注入 → 收集(带重试)→ 解析(标签/兜底恢复)→ 封装(tool_calls 或文本 / SSE)。
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

	var (
		text  string
		calls []officialtypes.ToolCall
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
		calls = parseProviderToolCalls(text, req.Tools)
		if len(calls) > 0 || attempt >= retries {
			break
		}
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

// parseProviderToolCalls 从全文解析工具调用:标签解析(Feed) → 未闭合兜底
// (Flush) → 无标签 JSON 兜底(RecoverFromText),与 ChatGPT 通道同序。
// 命中后按出现顺序重排 Index。
func parseProviderToolCalls(text string, tools []officialtypes.Tool) []officialtypes.ToolCall {
	parser := toolcall.NewParserWithTagsAndTools(toolcall.DefaultTags, tools)
	_, calls := parser.Feed(text)
	if len(calls) == 0 {
		_, extra := parser.Flush()
		calls = append(calls, extra...)
	}
	if len(calls) == 0 {
		calls = toolcall.RecoverFromText(text, tools)
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
