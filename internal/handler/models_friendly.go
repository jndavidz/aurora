package handler

import (
	"strings"

	"aurora/internal/provider"
)

// friendlyModelName 返回模型的友好显示名(2026-09-04,用户拍板与测试页对齐:
// 下拉/面板显示不带 -chat 后缀的简洁名;请求仍用真实 id)。
// 未映射的模型返回 ""(调用方不输出 friendly_name 字段)。
var friendlyModelNames = map[string]string{
	"auto":              "Auto",
	"gpt-5.6":           "GPT-5.6",
	"gpt-5.6-mini":      "GPT-5.6 Mini",
	"deepseek-v4-flash": "DeepSeek V4 Flash",
	"deepseek-v4-pro":   "DeepSeek V4 Pro",
	"glm-flash":         "GLM-5.3 Flash",
	"kimi":              "Kimi",
	"qwen-3.8-max":      "Qwen3.8 Max",
	"doubao":            "豆包",
	"gemini-3-flash":    "Gemini 3 Flash",
	"claude-sonnet-5":   "Claude Sonnet 5",
	"minimax-m3":        "MiniMax M3",
	"mimo-v2.5-pro":     "MiMo V2.5 Pro",
	"mimo-v2.5-asr":     "MiMo V2.5 ASR",
	"grok-3":            "Grok 3",
}

func friendlyModelName(id string) string {
	return friendlyModelNames[id]
}

// initFriendlyModelLookup 把友好名反查表挂到 provider.Registry
// (面板把显示名当 id 传时,Resolve 走第三层反查)。
func initFriendlyModelLookup() {
	// 反查表: friendly 名 → 真实 id;键规范化为小写(容错 MAX/max/Max 大小写差异)
	rev := make(map[string]string, len(friendlyModelNames)*2)
	for id, name := range friendlyModelNames {
		rev[name] = id
		rev[strings.ToLower(name)] = id
	}
	provider.SetFriendlyModelLookup(func(name string) string {
		// 直接遍历 id 表做宽松比对: 小写+去连字符+去空格 双向归一
		flat := func(x string) string {
			x = strings.ToLower(x)
			x = strings.ReplaceAll(x, "-", "")
			x = strings.ReplaceAll(x, " ", "")
			x = strings.ReplaceAll(x, "_", "")
			return x
		}
		target := flat(name)
		if target == "" {
			return ""
		}
		for id, name2 := range friendlyModelNames {
			if flat(id) == target || flat(name2) == target {
				return id
			}
		}
		return ""
	})
}
