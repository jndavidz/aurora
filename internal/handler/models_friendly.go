package handler

// friendlyModelName 返回模型的友好显示名(2026-09-04,用户拍板与测试页对齐:
// 下拉/面板显示不带 -chat 后缀的简洁名;请求仍用真实 id)。
// 未映射的模型返回 ""(调用方不输出 friendly_name 字段)。
var friendlyModelNames = map[string]string{
	"auto":                   "Auto",
	"gpt-5-6":                "GPT-5.6",
	"gpt-5-6-mini":           "GPT-5.6 Mini",
	"deepseek-v4-flash": "DeepSeek V4 Flash",
	"deepseek-v4-pro":   "DeepSeek V4 Pro",
	"glm-flash":           "GLM-5.2",
	"kimi":              "Kimi",
	"Qwen3.8-Max":            "Qwen3.8 Max",
	"doubao":            "豆包",
	"gemini-3-flash":    "Gemini 3 Flash",
	"claude-sonnet-5":   "Claude Sonnet 5",
	"minimax-m3":        "MiniMax M3",
	"mimo-v2.5-pro":     "MiMo V2.5 Pro",
	"mimo-v2.5-asr":          "MiMo V2.5 ASR",
	"grok-3":            "Grok 3",
}

func friendlyModelName(id string) string {
	return friendlyModelNames[id]
}
