package provider

import (
	"time"

	"aurora/internal/config"
)

// HunyuanCDP 通过 CDP 桥(scripts/cdp/bridge.mjs 的 hunyuan 适配器)执行腾讯元宝
// (混元)对话。协议(2026-08-22):
//   - 直连逆向(bogdanfinn TLS 指纹模拟)已风控 2 个账号 —— 本 Provider 走
//     真实浏览器**页内 fetch** 重放:每次请求 = create 会话 → chat 重放,
//     认证头(X-Uskey/X-HY93/X-device-id 等)会话级复用(用户手动请求捕获一次)。
//   - 响应 SSE:{"type":"text","msg":...} 增量。
//
// 复用 GeminiCDP 的转发/桥池熔断/自动唤醒逻辑;限频最保守(风控敏感)。
type HunyuanCDP struct {
	*GeminiCDP
}

// defaultHunyuanCDPModels 默认目录(网页实测混元模型 hunyuan_gpt_175B_0404)。
var defaultHunyuanCDPModels = []string{"hunyuan-hy3-chat"}

// NewHunyuanCDP 构造混元 CDP 桥 provider。桥地址默认复用 GEMINI_CDP_URL,
// 也可用 HUNYUAN_CDP_URL 单独指定。
func NewHunyuanCDP(cfg *config.Config) *HunyuanCDP {
	urlList := cfg.HunyuanCDPURL
	if urlList == "" {
		urlList = cfg.GeminiCDPURL
	}
	// 风控敏感(已风控 2 个账号):chat 也限频,间隔拉长
	base := newCdpBase(cfg, urlList, defaultHunyuanCDPModels, "hunyuan-", "tencent",
		NewCodingLimiter(5*time.Second, 5*time.Second))
	return &HunyuanCDP{base}
}

// Name 覆盖嵌入的 GeminiCDP.Name()。
func (d *HunyuanCDP) Name() string { return "tencent" }
