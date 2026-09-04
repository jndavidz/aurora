package provider

import (
	"time"

	"aurora/internal/config"
)

// DoubaoCDP 通过 CDP 桥(scripts/cdp/bridge.mjs 的 doubao 适配器)执行豆包对话。
// 协议(2026-09-04 调研 5 个 doubao2api 参考仓库后定案):
//   - 网页版 cookie 直连已被上游风控封死(回声 bug/静默空响应/滑块),页面 UI
//     正常而 API 直连被识别拦截——两条路风控规则分离
//   - 本 Provider 走真实浏览器**页内 fetch**(同 hunyuan/claude):桥页面内发
//     /chat/completion,字节前端 JS hook 自动注入 a_bogus/msToken(实时生成
//     永不过期);会话状态(conversation_id/last_message_index)由 capture 钩子
//     从页面真实请求持续同步——用户 VNC 聊天推进会话时桥状态自动跟进
//   - 会话索引与上游一致是关键:错位即被判重放(返回旧应答或空响应)
//
// 复用 GeminiCDP 的转发/桥池熔断/自动唤醒逻辑;限频保守(风控敏感)。
type DoubaoCDP struct {
	*GeminiCDP
}

// defaultDoubaoCDPModels 默认目录(豆包网页主模型)。
var defaultDoubaoCDPModels = []string{"doubao-chat"}

// NewDoubaoCDP 构造豆包 CDP 桥 provider。桥地址默认复用 GEMINI_CDP_URL,
// 也可用 DOUBAO_CDP_URL 单独指定。
func NewDoubaoCDP(cfg *config.Config) *DoubaoCDP {
	urlList := cfg.DoubaoCDPURL
	if urlList == "" {
		urlList = cfg.GeminiCDPURL
	}
	// 风控敏感:chat 限频(3s 间隔),避免高频触发滑块
	base := newCdpBase(cfg, urlList, defaultDoubaoCDPModels, "doubao-", "bytedance",
		NewCodingLimiter(3*time.Second, 3*time.Second))
	return &DoubaoCDP{base}
}

// Name 覆盖嵌入的 GeminiCDP.Name()。
func (d *DoubaoCDP) Name() string { return "bytedance" }
