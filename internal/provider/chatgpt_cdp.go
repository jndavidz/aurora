package provider

import (
	"strings"
	"time"

	"aurora/internal/config"
	"aurora/typings/official"

	"github.com/gin-gonic/gin"
)

// ChatgptCDP 通过家庭/办公室 PC 上的 CDP 桥(scripts/cdp/bridge.mjs 的 chatgpt 适配器)
// 执行 chatgpt.com 对话。背景:ChatGPT 已改为浏览器会话绑定鉴权(Cloudflare + oai-did
// 设备指纹 + 实时 sentinel 反自动化 header),不再提供服务端可复用的 session/access
// token —— aurora 直连 backend-api 用的 token 文件会被 403,页内手造 fetch 缺
// OpenAI-Sentinel-* 等实时 header 同样 403 "Unusual activity"(均经 2026-09-02 抓包实测)。
// 唯一稳的通道是 UI 驱动:在已登录真实浏览器页面里让页面自己发消息(页面 JS 实时生成
// 全部 header),桥只负责注入文本到 composer + 点击发送 + 轮询 DOM 读回复(与 gemini
// UI 模式同思路)。旧对话偶发静默失败时,桥会自愈:导航开新对话重发一次。
//
// 复用 GeminiCDP 的全部转发/桥池熔断/自动唤醒/限频逻辑(仅模型目录与名字不同)。
// 桥地址默认复用 GEMINI_CDP_URL(同一桥服务多 provider)。
//
// 模型:gpt-5-6 / gpt-5-6-mini(用户直接以这两个 id 请求;aurora 原样转给桥,
// 桥侧 chatgpt adapter 以 "gpt-" 前缀特判)。内部以 "-chat" 后缀注册以满足基类
// newCdpBase 的后缀约定,对外(/v1/models)与转发时剥除该后缀。
type ChatgptCDP struct {
	*GeminiCDP
}

// defaultChatgptCDPModels 内部注册名(带 -chat 后缀,桥侧只用模板的固定 model)。
// "gpt-coding" 不带 -chat 后缀:基类按 -coding 后缀注册为 coding variant
// (gemini_cdp.go newCdpBase),走 codingCompletions 整包 prompt → 桥 UI 发送 →
// FenceParser 解析 <tool_call> → 标准 OpenAI tool_calls。
// 可行性实测见 docs/CHATGPT_TOOL_BRIDGE.md(网页模型遵守协议、DOM 提取保真、
// 规则 6 压制网页原生 Python 工具)。
var defaultChatgptCDPModels = []string{"gpt-5-6-chat", "gpt-5-6-mini-chat", "gpt-coding"}

// NewChatgptCDP 构造 ChatGPT CDP 桥 provider。桥地址默认复用 GEMINI_CDP_URL
// (同一桥服务多 provider),也可用 CHATGPT_CDP_URL 单独指定。
func NewChatgptCDP(cfg *config.Config) *ChatgptCDP {
	urlList := cfg.ChatgptCDPURL
	if urlList == "" {
		urlList = cfg.GeminiCDPURL
	}
	base := newCdpBase(cfg, urlList, defaultChatgptCDPModels, "gpt-", "openai",
		NewCodingLimiter(2*time.Second, 2*time.Second)) // ChatGPT 免费账号周限额,基础 2s
	return &ChatgptCDP{base}
}

// Name 覆盖嵌入的 GeminiCDP.Name()。
func (d *ChatgptCDP) Name() string { return "openai" }

// Handles 同时接受 "gpt-5-6" 与内部 "gpt-5-6-chat" 形态。
func (d *ChatgptCDP) Handles(model string) bool {
	if _, ok := d.byID[model]; ok {
		return true
	}
	if _, ok := d.byID[model+"-chat"]; ok {
		return true
	}
	return false
}

// Models 对外暴露不带 -chat 后缀的真实模型 id。
func (d *ChatgptCDP) Models() []Model {
	out := make([]Model, 0, len(d.models))
	for _, m := range d.models {
		id := strings.TrimSuffix(m.ID, "-chat")
		out = append(out, Model{ID: id, OwnedBy: m.OwnedBy, Caps: m.Caps})
	}
	return out
}

// Responses 对 /v1/responses 的 gpt-coding 保持"必须携带 tools"约束(与 ChatCompletions
// 对称;其余透传基类 —— coding variant 走 codingResponses 整包协议,chat 变体走桥)。
func (d *ChatgptCDP) Responses(c *gin.Context, req *official.ResponsesAPIRequest) {
	if req != nil && req.Model == "gpt-coding" && len(req.Tools) == 0 {
		c.JSON(400, gin.H{"error": gin.H{
			"message": "coding 模型(gpt-coding)需要携带 tools 参数",
			"type":    "invalid_request_error",
			"param":   "tools",
			"code":    "missing_tools",
		}})
		return
	}
	d.GeminiCDP.Responses(c, req)
}

// ChatCompletions 在转发前把无后缀 model 补成内部 "-chat" 形态(基类按 byID 路由),
// 桥侧 chatgpt adapter 以 "gpt-" 前缀特判,UI 模式按页面当前选中模型实际发送。
// gpt-coding 走 coding variant(codingCompletions 整包 prompt + FenceParser),
// 前置校验 tools 非空 —— 原校验在 chat_handler 的官方 API 路径,走桥后不再经过。
func (d *ChatgptCDP) ChatCompletions(c *gin.Context, req *official.APIRequest) {
	if req != nil && req.Model == "gpt-coding" {
		if len(req.Tools) == 0 {
			c.JSON(400, gin.H{"error": gin.H{
				"message": "coding 模型(gpt-coding)需要携带 tools 参数",
				"type":    "invalid_request_error",
				"param":   "tools",
				"code":    "missing_tools",
			}})
			return
		}
	}
	if req != nil && !strings.HasSuffix(req.Model, "-chat") {
		if _, ok := d.byID[req.Model+"-chat"]; ok {
			req.Model = req.Model + "-chat"
		}
	}
	d.GeminiCDP.ChatCompletions(c, req)
}
