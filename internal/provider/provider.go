// Package provider 定义多上游(Provider)抽象与模型目录。
//
// aurora 的对外表面统一为 Responses API(/v1/responses),每个 Provider
// 负责把 Responses 请求翻译成某个上游(网页逆向)的协议并回吐 Responses 事件。
// ChatGPT 作为默认兜底保留在 handler 内(Resolve 未命中时走原路径);
// 新增上游(DeepSeek、后续 Qwen 等)实现本接口。
package provider

import (
	"aurora/typings/official"

	"github.com/gin-gonic/gin"
)

// Capability 描述一个模型具备的能力,用于 /v1/models 与文档如实标注。
type Capability string

const (
	// CapWebSearch 联网搜索(网页智能搜索 / web_search 工具)。
	CapWebSearch Capability = "web_search"
	// CapReasoning 深度思考 / 思维链。
	CapReasoning Capability = "reasoning"
	// CapVision 识图 / 多模态图片理解。
	CapVision Capability = "vision"
	// CapFunctionCall 工具调用(网页逆向下为文本协议模拟,非原生 function calling)。
	CapFunctionCall Capability = "function_call"
	// CapSandboxCode 云端沙箱代码执行(智谱 coding 变体:execute_sandbox_code,
	// 在智谱云端真实执行并回传结果;不是客户端工具调用)。
	CapSandboxCode Capability = "sandbox_code"
)

// Model 是 /v1/models 目录里的一个条目,附带能力标注。
type Model struct {
	ID      string
	OwnedBy string
	Caps    []Capability
}

// Provider 是一个可选对话上游。
//
// ChatGPT 不实现本接口(作为 handler 默认兜底);DeepSeek 等新上游实现它。
// 每个 Provider 在构造时持有自身所需的配置(API key/token 池/代理等)。
type Provider interface {
	// Name 返回上游标识,如 "deepseek"。
	Name() string
	// Models 返回该上游贡献给 /v1/models 的模型列表(含能力标注)。
	Models() []Model
	// Handles 报告该 Provider 是否接管给定的模型 id(精确匹配)。
	Handles(model string) bool
	// Responses 处理 /v1/responses 请求(流式 + 非流式),直接写 gin.Context。
	Responses(c *gin.Context, req *official.ResponsesAPIRequest)
	// ChatCompletions 处理 /v1/chat/completions 请求(流式 + 非流式)。
	// 输出 chat.completion / chat.completion.chunk 格式,与 Responses 并行。
	ChatCompletions(c *gin.Context, req *official.APIRequest)
}

// Registry 持有所有已注册的 Provider,按模型 id 路由。
type Registry struct {
	providers []Provider
	// E1/G2:provider 级熔断——连续失败达阈值则 Resolve 跳过,冷却到期半开恢复
	breaker *ProviderBreaker
}

// NewRegistry 构造一个空注册表。
func NewRegistry() *Registry {
	return &Registry{breaker: NewProviderBreaker()}
}

// Register 追加一个 Provider。先注册的优先级更高(Resolve 正序遍历,
// 先匹配先返回)—— router.go 的注册顺序即匹配优先级,勿随意调换。
func (r *Registry) Register(p Provider) {
	r.providers = append(r.providers, p)
}

// Resolve 返回接管给定模型的 Provider;无匹配返回 nil(走默认 ChatGPT)。
// 匹配分两层:先精确 Handles;失败后按规范化 id(小写、去空白)再匹配——
// 容错客户端把友好名当 id 传的情况(实测面板:GLM-5.3 Flash / Qwen3.8 MAX)。
// 命中规范化层的,会把 provider 的真实模型 id 回填到 req(handler 侧见
// chat_handler 的 normalizedModel 逻辑),保证 provider 内部 byID 路由可用。
func (r *Registry) Resolve(model string) Provider {
	for _, p := range r.providers {
		if p.Handles(model) {
			if r.breaker.Tripped(p.Name()) {
				continue // 熔断中的 provider 跳过(Resolve 找不到别的就走 ChatGPT 兜底)
			}
			return p
		}
	}
	// 第二层:规范化匹配(大小写/空白容错)
	norm := normalizeModelID(model)
	if norm != "" {
		for _, p := range r.providers {
			for _, m := range p.Models() {
				if normalizeModelID(m.ID) == norm {
					return p
				}
			}
		}
	}
	// 第三层:friendly_name 反查(面板把显示名当 id 传:GLM-5.3 Flash → glm-flash)
	if real := friendlyModelLookup(model); real != "" {
		for _, p := range r.providers {
			for _, m := range p.Models() {
				if m.ID == real {
					return p
				}
			}
		}
	}
	return nil
}

// friendlyModelLookup 由 handler 侧注入(friendlyModelNames 表反查),
// 避免 provider 包反向依赖 handler 包。默认无操作。
var friendlyModelLookup = func(string) string { return "" }

// SetFriendlyModelLookup 注册友好名反查函数(仅 handler 启动时调用一次)。
func SetFriendlyModelLookup(fn func(string) string) { friendlyModelLookup = fn }

// ResolveCanonical 返回 (provider, 真实模型 id)。规范化层命中时 id 与传入
// 原文不同(如 "GLM-5.3 Flash" → "glm-flash"),handler 应改用返回的 id。
func (r *Registry) ResolveCanonical(model string) (Provider, string) {
	if p := r.Resolve(model); p != nil {
		// 精确路径:原文即真实 id
		for _, m := range p.Models() {
			if m.ID == model {
				return p, model
			}
		}
		// 规范化路径:找该 provider 下规范化匹配的真实 id
		norm := normalizeModelID(model)
		for _, m := range p.Models() {
			if normalizeModelID(m.ID) == norm {
				return p, m.ID
			}
		}
		// friendly 反查路径:显示名 → 真实 id
		if real := friendlyModelLookup(model); real != "" {
			for _, m := range p.Models() {
				if m.ID == real {
					return p, m.ID
				}
			}
			return p, real
		}
		return p, model
	}
	return nil, model
}

// normalizeModelID 规范化模型 id:小写、去空白。保守策略:保留点号/连字符
// 差异(glm-5.2 ≠ glm52)。
func normalizeModelID(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' || c == '_' {
			continue
		}
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b = append(b, c)
	}
	return string(b)
}

// upstreamSlug 是"aurora 对外暴露 id → 上游真实 slug"的唯一映射点。
// 大多数 provider 上游不认 model 字段(走内部模板/assistant_id),或上游 slug
// 与暴露 id 完全一致(如 deepseek),无需映射。仅当 provider 把 model 直接透传
// 上游、且上游大小写/格式敏感时才需在此登记(目前仅千问)。
// 未知 id 原样返回(保底,正常不会走到)。
func upstreamSlug(exposedID string) string {
	switch exposedID {
	case "qwen-3.8-max":
		return "Qwen3.8-Max" // 千问上游大小写敏感,只认 Qwen3.8-Max
	default:
		return exposedID
	}
}

// Breaker 暴露熔断器(供 handler 记录成败与观测)。
func (r *Registry) Breaker() *ProviderBreaker { return r.breaker }

// Models 聚合所有 Provider 的模型列表。
func (r *Registry) Models() []Model {
	var out []Model
	for _, p := range r.providers {
		out = append(out, p.Models()...)
	}
	return out
}
