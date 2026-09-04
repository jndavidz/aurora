package config

import (
	"os"
	"strconv"
	"strings"
)

type Config struct {
	ServerHost          string
	ServerPort          string
	TLSCert             string
	TLSKey              string
	Authorization       string
	BaseURL             string
	APIReverseProxy     string
	FilesReverseProxy   string
	StreamMode          bool
	MaxContinueCount    int
	EnableHistory       bool
	EnableExternalToken bool // 是否接受外部传入的 accessToken
	ToolCallingEnabled  bool
	RefusalRetries      int
	DebugToolLog        string
	FreeAccounts        bool
	FreeAccountsNum     int
	ProxyURL            string
	HTTPProxy           string
	DebugSentinel       bool

	// DeepSeek 网页逆向(chat.deepseek.com)通道配置。
	DeepSeekWebBase   string   // 网页端 base,默认 https://chat.deepseek.com
	DeepSeekWebTokens string   // 网页 token 注入池文件路径(每行一个 user_token)
	DeepSeekModels    []string // 暴露的模型目录(exposed id 列表)
	DeepSeekProxy     string   // 网页通道出口代理(非美区,绕 WAF)
	// DeepSeekWebSearch 控制 quick 档(-chat)是否带联网搜索。
	// 默认关闭:网页搜索使首字延迟 +1~2s,而 API 客户端通常由自己侧调 search 工具;
	// 需要网页代查的场景设 DEEPSEEK_WEB_SEARCH=1。
	DeepSeekWebSearch bool // 默认 false

	// 智谱清言(chatglm.cn)网页逆向通道配置。
	GlmWebBase   string   // 网页端 base,默认 https://chatglm.cn
	GlmWebTokens string   // 网页 token 注入池文件路径(每行一个 chatglm_refresh_token)
	GlmModels    []string // 暴露的模型目录
	GlmProxy     string   // 网页通道出口代理

	// Kimi(www.kimi.com)网页逆向通道配置。
	KimiWebBase   string   // 网页端 base,默认 https://www.kimi.com
	KimiWebTokens string   // 网页 token 注入池文件路径(每行一个 refresh_token)
	KimiModels    []string // 暴露的模型目录
	KimiProxy     string   // 网页通道出口代理

	// 豆包(www.doubao.com)网页逆向通道配置。
	DoubaoAccounts string   // 网页账号池 JSON 文件路径(见 docs/DOUBAO.md)
	DoubaoModels   []string // 暴露的模型目录

	// 千问(www.qianwen.com)网页逆向通道配置。
	QianwenWebBase   string   // 网页端 base,默认 https://chat2.qianwen.com
	QianwenWebTokens string   // 网页 token 注入池文件路径(每行一个 tongyi_sso_ticket)
	QianwenModels    []string // 暴露的模型目录
	QianwenProxy     string   // 网页通道出口代理

	// Grok(grok.com)网页逆向通道配置。
	GrokCookies string   // 网页 cookie 池文件路径(每行 uid|cookie 串)
	GrokModels  []string // 暴露的模型目录

	// Gemini(gemini.google.com)网页逆向通道配置。
	GeminiAccounts string   // 网页账号池 JSON 文件路径(见 docs/GEMINI.md;直连已停用)
	GeminiModels   []string // 暴露的模型目录
	// Gemini CDP 桥通道(真浏览器执行,推荐):NAS 只做 HTTP 转发,
	// 真浏览器/指纹由家庭 PC 侧的 scripts/cdp/bridge.mjs 负责。
	GeminiCDPURL string // 桥地址,如 http://10.10.10.6:8799;空=不注册 CDP provider
	GeminiCDPKey string // 可选:桥的 BRIDGE_AUTH 对应 token
	// GeminiCDPWakePort 是 PC 上唤醒守护(scripts/cdp/keeper.mjs)的端口。
	// 桥池全挂时 aurora 自动 POST /wake 拉起 Chrome+桥后重试(全自动唤醒)。
	GeminiCDPWakePort string

	// Claude(claude.ai)CDP 桥通道。桥地址默认复用 GEMINI_CDP_URL(同一桥服务
	// 多 provider);CLAUDE_CDP_URL 可单独指定。仅当 URL 非空时注册。
	ClaudeCDPURL string
	ClaudeCDPKey string
	ClaudeModels []string

	// 混元(腾讯元宝)CDP 桥通道。走真实浏览器页内 fetch 重放(直连逆向已风控
	// 2 个账号,见 docs/YUANBAO.md)。桥地址默认复用 GEMINI_CDP_URL。
	HunyuanCDPURL string
	HunyuanCDPKey string
	HunyuanModels []string

	// ChatGPT(chatgpt.com)CDP 桥通道。背景:ChatGPT 已改为浏览器会话绑定鉴权,
	// 服务端无法复用 token,aurora 直连 backend-api 的 token 文件会被 403。
	// 故 ChatGPT 对话改走真实浏览器页内 fetch(bridge.mjs 的 chatgpt adapter),
	// coding 封存总开关(2026-09-02):false 时全部 -coding 变体不注册/不暴露/
	// 请求返回 400。背景:ChatGPT 网页通道的 agent 循环每轮 130-180s 且存在
	// 概率性拒绝/空响应,体验远差于 API,aurora 收敛为纯对话网关(冻结不删除,
	// 恢复置 true 即可,见 docs/CHATGPT_TOOL_BRIDGE.md)。
	CodingEnabled bool
	// 与 Gemini/Claude 同机制。桥地址默认复用 GEMINI_CDP_URL;CHATGPT_CDP_URL
	// 可单独指定。仅当 URL 非空时注册(用 ChatgptCDP)。
	ChatgptCDPURL string
	ChatgptCDPKey string

	// MiniMax(agent.minimaxi.com)网页逆向通道配置(直连,协议见 docs/MINIMAX.md)。
	MinimaxWebTokens string // token 池文件路径(每行一个 JWT,localStorage._token)
	MinimaxModels    []string
	MinimaxAgentID   string // 普通模式 agent id(默认 430731272630966;换账号需更新)
	MinimaxDeviceID  string // URL 参数 device_id(数字;抓包提取)
	MinimaxUserID    string // URL 参数 user_id(数字;抓包提取)

	// Mimo(aistudio.xiaomimimo.com)网页逆向通道配置(直连,协议见 docs/MIMO.md)。
	MimoWebTokens string // token 池文件路径(每行一个 cookie xiaomichatbot_ph 值)
	MimoModels    []string

	// 腾讯元宝(yuanbao.tencent.com)网页逆向通道配置。
	YuanbaoWebBase   string   // 网页端 base,默认 https://yuanbao.tencent.com
	YuanbaoWebTokens string   // 网页 token 注入池文件路径(每行一条 "<x-uskey>\t<cookie header>")
	YuanbaoModels    []string // 暴露的模型目录(hy3-* / yb-deepseek-*)
	YuanbaoAgentID   string   // 元宝主 agent id,默认 naQivTmsDa(页面 /chat/<agentId>)
	YuanbaoProxy     string   // 网页通道出口代理
}

func Load() Config {
	return Config{
		ServerHost:          getEnv("SERVER_HOST", "0.0.0.0"),
		ServerPort:          getEnvWithFallback("SERVER_PORT", "PORT", "8080"),
		TLSCert:             os.Getenv("TLS_CERT"),
		TLSKey:              os.Getenv("TLS_KEY"),
		Authorization:       os.Getenv("Authorization"),
		BaseURL:             getEnv("BASE_URL", "https://chatgpt.com/backend-api"),
		APIReverseProxy:     os.Getenv("API_REVERSE_PROXY"),
		FilesReverseProxy:   os.Getenv("FILES_REVERSE_PROXY"),
		StreamMode:          getBoolEnv("STREAM_MODE", true),
		MaxContinueCount:    getIntEnv("MAX_CONTINUE_COUNT", 3),
		EnableHistory:       getBoolEnv("ENABLE_HISTORY", false),
		EnableExternalToken: getBoolEnv("ENABLE_EXTERNAL_TOKEN", true),
		ToolCallingEnabled:  getBoolEnv("TOOL_CALLING_ENABLED", true),
		RefusalRetries:      getIntEnv("REFUSAL_RETRIES", 3),
		DebugToolLog:        os.Getenv("DEBUG_TOOL_LOG"),
		FreeAccounts:        getBoolEnv("FREE_ACCOUNTS", false),
		FreeAccountsNum:     getIntEnv("FREE_ACCOUNTS_NUM", 1024),
		ProxyURL:            os.Getenv("PROXY_URL"),
		HTTPProxy:           os.Getenv("http_proxy"),
		DebugSentinel:       getBoolEnv("DEBUG_SENTINEL", false),

		DeepSeekWebBase:   getEnv("DEEPSEEK_WEB_BASE", "https://chat.deepseek.com"),
		DeepSeekWebTokens: os.Getenv("DEEPSEEK_WEB_TOKENS"),
		DeepSeekModels:    splitCSV(os.Getenv("DEEPSEEK_MODELS")),
		DeepSeekProxy:     os.Getenv("DEEPSEEK_PROXY"),
		DeepSeekWebSearch: getBoolEnv("DEEPSEEK_WEB_SEARCH", false),

		GlmWebBase:   getEnv("GLM_WEB_BASE", "https://chatglm.cn"),
		GlmWebTokens: os.Getenv("GLM_WEB_TOKENS"),
		GlmModels:    splitCSV(os.Getenv("GLM_MODELS")),
		GlmProxy:     os.Getenv("GLM_PROXY"),

		KimiWebBase:   getEnv("KIMI_WEB_BASE", "https://www.kimi.com"),
		KimiWebTokens: os.Getenv("KIMI_WEB_TOKENS"),
		KimiModels:    splitCSV(os.Getenv("KIMI_MODELS")),
		KimiProxy:     os.Getenv("KIMI_PROXY"),

		DoubaoAccounts: os.Getenv("DOUBAO_ACCOUNTS"),
		DoubaoModels:   splitCSV(os.Getenv("DOUBAO_MODELS")),

		QianwenWebBase:   getEnv("QIANWEN_WEB_BASE", "https://chat2.qianwen.com"),
		QianwenWebTokens: os.Getenv("QIANWEN_WEB_TOKENS"),
		QianwenModels:    splitCSV(os.Getenv("QIANWEN_MODELS")),
		QianwenProxy:     os.Getenv("QIANWEN_PROXY"),

		GrokCookies: os.Getenv("GROK_COOKIES"),
		GrokModels:  splitCSV(os.Getenv("GROK_MODELS")),

		GeminiAccounts:    os.Getenv("GEMINI_ACCOUNTS"),
		GeminiModels:      splitCSV(os.Getenv("GEMINI_MODELS")),
		GeminiCDPURL:      os.Getenv("GEMINI_CDP_URL"),
		GeminiCDPKey:      os.Getenv("GEMINI_CDP_KEY"),
		GeminiCDPWakePort: getEnv("GEMINI_CDP_WAKE_PORT", "8798"),

		ClaudeCDPURL: os.Getenv("CLAUDE_CDP_URL"),
		ClaudeCDPKey: os.Getenv("CLAUDE_CDP_KEY"),
		ClaudeModels: splitCSV(os.Getenv("CLAUDE_MODELS")),

		HunyuanCDPURL: os.Getenv("HUNYUAN_CDP_URL"),
		HunyuanCDPKey: os.Getenv("HUNYUAN_CDP_KEY"),
		HunyuanModels: splitCSV(os.Getenv("HUNYUAN_MODELS")),

		CodingEnabled: os.Getenv("CODING_ENABLED") == "true" || os.Getenv("CODING_ENABLED") == "1",

		ChatgptCDPURL: os.Getenv("CHATGPT_CDP_URL"),
		ChatgptCDPKey: os.Getenv("CHATGPT_CDP_KEY"),

		MinimaxWebTokens: os.Getenv("MINIMAX_WEB_TOKENS"),
		MinimaxModels:    splitCSV(os.Getenv("MINIMAX_MODELS")),
		MinimaxAgentID:   os.Getenv("MINIMAX_AGENT_ID"),
		MinimaxDeviceID:  os.Getenv("MINIMAX_DEVICE_ID"),
		MinimaxUserID:    os.Getenv("MINIMAX_USER_ID"),

		MimoWebTokens: os.Getenv("MIMO_WEB_TOKENS"),
		MimoModels:    splitCSV(os.Getenv("MIMO_MODELS")),

		YuanbaoWebBase:   getEnv("YUANBAO_WEB_BASE", "https://yuanbao.tencent.com"),
		YuanbaoWebTokens: os.Getenv("YUANBAO_WEB_TOKENS"),
		YuanbaoModels:    splitCSV(os.Getenv("YUANBAO_MODELS")),
		YuanbaoAgentID:   getEnv("YUANBAO_AGENT_ID", "naQivTmsDa"),
		YuanbaoProxy:     os.Getenv("YUANBAO_PROXY"),
	}
}

// splitCSV 把逗号分隔的字符串拆成非空切片;空串返回 nil。
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func getEnvWithFallback(key, fallbackKey, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	if v := os.Getenv(fallbackKey); v != "" {
		return v
	}
	return defaultVal
}

func getBoolEnv(key string, defaultVal bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return defaultVal
	}
	return b
}

func getIntEnv(key string, defaultVal int) int {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return defaultVal
	}
	return n
}
