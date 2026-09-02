package handler

import (
	"aurora/httpclient/bogdanfinn"
	"aurora/internal/accounts"
	"aurora/internal/chatgpt"
	"aurora/internal/config"
	"aurora/internal/provider"
	"aurora/middlewares"

	"github.com/gin-gonic/gin"
)

func optionsHandler(c *gin.Context) {
	c.Header("Access-Control-Allow-Origin", "*")
	c.Header("Access-Control-Allow-Methods", "POST")
	c.Header("Access-Control-Allow-Headers", "*")
	c.JSON(200, gin.H{
		"message": "pong",
	})
}

func RegisterRouter(accountPool *accounts.Pool, cfg *config.Config) *gin.Engine {
	// 构建 Provider 注册表:DeepSeek 等新上游在此注册。
	// 仅当配置了 token 池时才注册(避免 /v1/models 广告无可用 token 的模型)。
	// 注册顺序决定 /v1/models 的排列顺序(先注册的在前)。
	// 按用户 2026-08-14 要求的排列:GPT → DeepSeek → GLM → Kimi → Qianwen → Doubao → Grok
	// Gemini 直连已停用(数据中心 IP + 模拟指纹被 Google 风控拒绝,见 commit ff4af80);
	// 现走 CDP 桥通道(真浏览器执行,家庭 PC 上的 scripts/cdp/bridge.mjs):
	// NAS 只做 HTTP 转发,GEMINI_CDP_URL 配置后注册(见 docs/GEMINI.md §八)。
	registry := provider.NewRegistry()
	// if cfg.GeminiAccounts != "" {
	// 	registry.Register(provider.NewGemini(cfg))
	// }
	if cfg.GeminiCDPURL != "" {
		registry.Register(provider.NewGeminiCDP(cfg))
	}
	if cfg.ClaudeCDPURL != "" {
		registry.Register(provider.NewClaudeCDP(cfg))
	}
	if cfg.ChatgptCDPURL != "" {
		registry.Register(provider.NewChatgptCDP(cfg))
	}
	if cfg.HunyuanCDPURL != "" {
		registry.Register(provider.NewHunyuanCDP(cfg))
	}
	if cfg.MinimaxWebTokens != "" {
		registry.Register(provider.NewMinimax(cfg))
	}
	var mimoProvider *provider.Mimo
	if cfg.MimoWebTokens != "" {
		mimoProvider = provider.NewMimo(cfg)
		registry.Register(mimoProvider)
	}
	if cfg.DeepSeekWebTokens != "" {
		registry.Register(provider.NewDeepSeek(cfg))
	}
	if cfg.GlmWebTokens != "" {
		registry.Register(provider.NewGlm(cfg))
	}
	if cfg.KimiWebTokens != "" {
		registry.Register(provider.NewKimi(cfg))
	}
	if cfg.QianwenWebTokens != "" {
		registry.Register(provider.NewQianwen(cfg))
	}
	if cfg.DoubaoAccounts != "" {
		registry.Register(provider.NewDoubao(cfg))
	}
	if cfg.GrokCookies != "" {
		registry.Register(provider.NewGrok(cfg))
	}
	if cfg.YuanbaoWebTokens != "" {
		registry.Register(provider.NewYuanbao(cfg))
	}

	chatHandler := NewChatHandler(accountPool, cfg, registry)
	imageHandler := NewImageHandler(accountPool, cfg)
	audioHandler := NewAudioHandler(accountPool, cfg, mimoProvider)
	authHandler := NewAuthHandler(accountPool)
	modelsHandler := NewModelsHandler(registry, cfg.CodingEnabled)

	// 初始化基础前置参数（DPL、BasicCookies 等）
	proxyUrl := ""
	client := bogdanfinn.NewStdClient()
	chatgpt.GetDpl(client, proxyUrl)

	router := gin.Default()
	router.Use(middlewares.Cors)

	router.GET("/", func(c *gin.Context) { c.JSON(200, gin.H{"message": "Hello, world!"}) })
	router.GET("/ping", func(c *gin.Context) { c.JSON(200, gin.H{"message": "pong"}) })

	router.POST("/auth/session", authHandler.Session)
	router.POST("/auth/refresh", authHandler.Refresh)

	router.OPTIONS("/v1/chat/completions", optionsHandler)
	router.OPTIONS("/v1/models", optionsHandler)
	router.OPTIONS("/v1/models/responses", optionsHandler)
	router.OPTIONS("/v1/responses", optionsHandler)
	router.OPTIONS("/v1/images/generations", optionsHandler)
	router.OPTIONS("/v1/images/edits", optionsHandler)
	router.OPTIONS("/v1/images/variations", optionsHandler)
	router.OPTIONS("/v1/files", optionsHandler)
	router.OPTIONS("/v1/audio/speech", optionsHandler)
	router.OPTIONS("/v1/audio/transcriptions", optionsHandler)
	router.OPTIONS("/v1/audio/translations", optionsHandler)

	authGroup := router.Group("").Use(middlewares.Authorization)
	authGroup.POST("/v1/chat/completions", chatHandler.Nightmare)
	authGroup.POST("/v1/responses", chatHandler.Responses)
	// pi 的 responses 适配器实际请求路径是 /v1/models/responses(见 PI_AGENT_DEBUG.md §3)。
	authGroup.POST("/v1/models/responses", chatHandler.Responses)
	authGroup.POST("/v1/files", chatHandler.Files)
	authGroup.GET("/v1/models", modelsHandler.ListModels)
	// 凭证健康端点(可靠性 A3):汇总 ChatGPT 池与各 provider 凭证有效期,内网运维用。
	authGroup.GET("/v1/health/credentials", chatHandler.CredentialHealth)
	authGroup.POST("/backend-api/conversation", chatHandler.ChatGPTConversation)
	authGroup.POST("/v1/images/generations", imageHandler.Generations)
	authGroup.POST("/v1/images/edits", imageHandler.Edits)
	authGroup.POST("/v1/images/variations", imageHandler.Variations)
	authGroup.POST("/v1/audio/speech", audioHandler.TTS)
	authGroup.POST("/v1/audio/transcriptions", audioHandler.Transcriptions)
	authGroup.POST("/v1/audio/translations", audioHandler.Translations)

	return router
}
