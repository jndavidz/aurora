package provider

import (
	"time"

	"aurora/internal/config"
)

// ClaudeCDP 通过家庭/办公室 PC 上的 CDP 桥(scripts/cdp/bridge.mjs 的 claude 适配器)
// 执行 claude.ai 对话。协议(2026-08-14 抓包实测):
//   - 认证纯 cookie,无 Authorization/无会话令牌折腾(比 Gemini 简单得多)
//   - 发消息 = POST /api/organizations/{org}/chat_conversations/{convId}/completion,
//     每轮新 convId,多轮上下文靠全量拍平 prompt;响应为标准 Anthropic SSE
//   - 桥侧缓存 completion 请求体模板(26 个前端内置工具原样保留),每轮替换
//     prompt + turn_message_uuids
//
// 复用 GeminiCDP 的全部转发/桥池熔断/自动唤醒/限频逻辑(仅模型目录与名字不同)。
type ClaudeCDP struct {
	*GeminiCDP
}

// defaultClaudeCDPModels 默认目录(网页实测模型 claude-sonnet-5)。
var defaultClaudeCDPModels = []string{"claude-sonnet-5", "claude-sonnet-5-coding"}

// NewClaudeCDP 构造 Claude CDP 桥 provider。桥地址默认复用 GEMINI_CDP_URL
// (同一桥服务多 provider),也可用 CLAUDE_CDP_URL 单独指定。
func NewClaudeCDP(cfg *config.Config) *ClaudeCDP {
	urlList := cfg.ClaudeCDPURL
	if urlList == "" {
		urlList = cfg.GeminiCDPURL
	}
	base := newCdpBase(cfg, urlList, defaultClaudeCDPModels, "claude-", "anthropic",
		NewCodingLimiter(2*time.Second, time.Second)) // Claude 风控中等
	return &ClaudeCDP{base}
}

// Name 覆盖嵌入的 GeminiCDP.Name()。
func (d *ClaudeCDP) Name() string { return "anthropic" }
