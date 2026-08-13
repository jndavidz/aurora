package config

import (
	"os"
	"strconv"
	"strings"
)

type Config struct {
	ServerHost         string
	ServerPort         string
	TLSCert            string
	TLSKey             string
	Authorization      string
	BaseURL            string
	APIReverseProxy    string
	FilesReverseProxy  string
	StreamMode         bool
	MaxContinueCount   int
	EnableHistory      bool
	EnableExternalToken bool  // 是否接受外部传入的 accessToken
	ToolCallingEnabled bool
	RefusalRetries     int
	DebugToolLog       string
	FreeAccounts       bool
	FreeAccountsNum    int
	ProxyURL           string
	HTTPProxy          string
	DebugSentinel      bool

	// DeepSeek 网页逆向(chat.deepseek.com)通道配置。
	DeepSeekWebBase   string   // 网页端 base,默认 https://chat.deepseek.com
	DeepSeekWebTokens string   // 网页 token 注入池文件路径(每行一个 user_token)
	DeepSeekModels    []string // 暴露的模型目录(exposed id 列表)
	DeepSeekProxy     string   // 网页通道出口代理(非美区,绕 WAF)

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

	// 千问(www.qianwen.com)网页逆向通道配置。
	QianwenWebBase   string   // 网页端 base,默认 https://chat2.qianwen.com
	QianwenWebTokens string   // 网页 token 注入池文件路径(每行一个 tongyi_sso_ticket)
	QianwenModels    []string // 暴露的模型目录
	QianwenProxy     string   // 网页通道出口代理

	// Grok(grok.com)网页逆向通道配置。
	GrokCookies string   // 网页 cookie 池文件路径(每行 uid|cookie 串)
	GrokModels  []string // 暴露的模型目录

	// Gemini(gemini.google.com)网页逆向通道配置。
	GeminiAccounts string   // 网页账号池 JSON 文件路径(见 docs/GEMINI.md)
	GeminiModels   []string // 暴露的模型目录

	// 腾讯元宝(yuanbao.tencent.com)网页逆向通道配置。
	YuanbaoWebBase   string   // 网页端 base,默认 https://yuanbao.tencent.com
	YuanbaoWebTokens string   // 网页 token 注入池文件路径(每行一条 "<x-uskey>\t<cookie header>")
	YuanbaoModels    []string // 暴露的模型目录(hy3-* / yb-deepseek-*)
	YuanbaoAgentID   string   // 元宝主 agent id,默认 naQivTmsDa(页面 /chat/<agentId>)
	YuanbaoProxy     string   // 网页通道出口代理
}

func Load() Config {
	return Config{
		ServerHost:         getEnv("SERVER_HOST", "0.0.0.0"),
		ServerPort:         getEnvWithFallback("SERVER_PORT", "PORT", "8080"),
		TLSCert:            os.Getenv("TLS_CERT"),
		TLSKey:             os.Getenv("TLS_KEY"),
		Authorization:      os.Getenv("Authorization"),
		BaseURL:            getEnv("BASE_URL", "https://chatgpt.com/backend-api"),
		APIReverseProxy:    os.Getenv("API_REVERSE_PROXY"),
		FilesReverseProxy:  os.Getenv("FILES_REVERSE_PROXY"),
		StreamMode:         getBoolEnv("STREAM_MODE", true),
		MaxContinueCount:   getIntEnv("MAX_CONTINUE_COUNT", 3),
		EnableHistory:      getBoolEnv("ENABLE_HISTORY", false),
		EnableExternalToken: getBoolEnv("ENABLE_EXTERNAL_TOKEN", true),
		ToolCallingEnabled: getBoolEnv("TOOL_CALLING_ENABLED", true),
		RefusalRetries:     getIntEnv("REFUSAL_RETRIES", 3),
		DebugToolLog:       os.Getenv("DEBUG_TOOL_LOG"),
		FreeAccounts:       getBoolEnv("FREE_ACCOUNTS", false),
		FreeAccountsNum:    getIntEnv("FREE_ACCOUNTS_NUM", 1024),
		ProxyURL:           os.Getenv("PROXY_URL"),
		HTTPProxy:          os.Getenv("http_proxy"),
		DebugSentinel:      getBoolEnv("DEBUG_SENTINEL", false),

		DeepSeekWebBase:   getEnv("DEEPSEEK_WEB_BASE", "https://chat.deepseek.com"),
		DeepSeekWebTokens: os.Getenv("DEEPSEEK_WEB_TOKENS"),
		DeepSeekModels:    splitCSV(os.Getenv("DEEPSEEK_MODELS")),
		DeepSeekProxy:     os.Getenv("DEEPSEEK_PROXY"),

		GlmWebBase:   getEnv("GLM_WEB_BASE", "https://chatglm.cn"),
		GlmWebTokens: os.Getenv("GLM_WEB_TOKENS"),
		GlmModels:    splitCSV(os.Getenv("GLM_MODELS")),
		GlmProxy:     os.Getenv("GLM_PROXY"),

		KimiWebBase:   getEnv("KIMI_WEB_BASE", "https://www.kimi.com"),
		KimiWebTokens: os.Getenv("KIMI_WEB_TOKENS"),
		KimiModels:    splitCSV(os.Getenv("KIMI_MODELS")),
		KimiProxy:     os.Getenv("KIMI_PROXY"),

		QianwenWebBase:   getEnv("QIANWEN_WEB_BASE", "https://chat2.qianwen.com"),
		QianwenWebTokens: os.Getenv("QIANWEN_WEB_TOKENS"),
		QianwenModels:    splitCSV(os.Getenv("QIANWEN_MODELS")),
		QianwenProxy:     os.Getenv("QIANWEN_PROXY"),

		GrokCookies: os.Getenv("GROK_COOKIES"),
		GrokModels:  splitCSV(os.Getenv("GROK_MODELS")),

		GeminiAccounts: os.Getenv("GEMINI_ACCOUNTS"),
		GeminiModels:   splitCSV(os.Getenv("GEMINI_MODELS")),

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
