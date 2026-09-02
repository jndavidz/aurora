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

// codingResponses / codingCompletions 的 coding 分支在子类入口**接管**(不委托
// 基类 coding 方法):基类方法内部调 d.codingEnvPrompt()/d.codingCompletions 时
// receiver 是裸 *GeminiCDP,Go 无虚方法 —— 任何"子类覆写等基类内部来调"的方案
// 都不可达(两次实测踩坑:prompt 长度恒定暴露覆写从未生效)。
// 因此 ChatCompletions/Responses 在入口识别 coding variant 后,直接在子类层
// 构造 prompt 并调基类 Stream/NonStream(同包私有方法,receiver 正确)。
func (d *ChatgptCDP) codingResponses(c *gin.Context, req *official.ResponsesAPIRequest) {
	d.limiter.Wait()
	prompt := geminiCodingPromptFromResponses(req, d.codingEnvPrompt(req.Tools))
	if req.Stream {
		d.codingResponsesStream(c, req, prompt)
		return
	}
	d.codingResponsesNonStream(c, req, prompt)
}

func (d *ChatgptCDP) codingCompletions(c *gin.Context, req *official.APIRequest) {
	d.limiter.Wait()
	prompt := geminiCodingPromptFromAPI(req, d.codingEnvPrompt(req.Tools))
	if req.Stream {
		d.codingCompletionsStream(c, req, prompt)
		return
	}
	d.codingCompletionsNonStream(c, req, prompt)
}

// codingEnvPrompt 返回空 —— 实测定案(2026-09-02 实验C/D):强协议指令(长篇
// CRITICAL RULES + ENVIRONMENT REALITY)反而触发模型的诚实拒绝("I can't access
// the user's machine" / "按要求不能编造结果"),因为它与网页环境事实矛盾("你
// 没有沙箱"被模型证伪)+ 高压措辞激活诚实 RLHF;而 glm 温和版在完全相同的任务上
// 成功输出围栏 JSON(实验轮1)。钩子与子类覆写入口保留(方法分派修复仍有价值:
// 未来需要时可正确注入),但 ChatGPT 不再覆盖 glm 指令。
// 详见 docs/CHATGPT_TOOL_BRIDGE.md §十。
func (d *ChatgptCDP) codingEnvPrompt(tools []official.Tool) string { return "" }

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
	// coding variant 在子类层接管(同 ChatCompletions 注释)
	if v, ok := d.byID[req.Model]; ok && v == "coding" {
		d.codingResponses(c, req)
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
	// coding variant 在子类层接管(见 codingCompletions 注释:基类内部调用不可达子类覆写)
	if v, ok := d.byID[req.Model]; ok && v == "coding" {
		d.codingCompletions(c, req)
		return
	}
	d.GeminiCDP.ChatCompletions(c, req)
}
