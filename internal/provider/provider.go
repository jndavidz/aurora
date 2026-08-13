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
}

// NewRegistry 构造一个空注册表。
func NewRegistry() *Registry {
	return &Registry{}
}

// Register 追加一个 Provider。后注册的优先级更高(先匹配先返回)。
func (r *Registry) Register(p Provider) {
	r.providers = append(r.providers, p)
}

// Resolve 返回接管给定模型的 Provider;无匹配返回 nil(走默认 ChatGPT)。
func (r *Registry) Resolve(model string) Provider {
	for _, p := range r.providers {
		if p.Handles(model) {
			return p
		}
	}
	return nil
}

// Models 聚合所有 Provider 的模型列表。
func (r *Registry) Models() []Model {
	var out []Model
	for _, p := range r.providers {
		out = append(out, p.Models()...)
	}
	return out
}
