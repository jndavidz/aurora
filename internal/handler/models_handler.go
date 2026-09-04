package handler

import (
	"strings"
	"aurora/internal/provider"

	"github.com/gin-gonic/gin"
)

type ModelsHandler struct {
	registry     *provider.Registry
	codingEnabled bool
}

func NewModelsHandler(registry *provider.Registry, codingEnabled bool) *ModelsHandler {
	initFriendlyModelLookup() // 挂载友好名反查(面板显示名容错)
	return &ModelsHandler{registry: registry, codingEnabled: codingEnabled}
}

// isCodingModelName 判定 coding 模型名(各家 -coding 变体 + gpt-coding 别名)。
func isCodingModelName(model string) bool {
	return strings.HasSuffix(model, "-coding") || model == "gpt-coding"
}

// normalizeCodingModel 检测 ChatGPT 的 -coding 变体:
//   - gpt-coding → (gpt-5-6, true):改写为真实 slug 透传上游,强制工具调用
//   - 其他 → (原 id, false):不改写
func normalizeCodingModel(model string) (string, bool) {
	if model == "gpt-coding" {
		return "gpt-5-6", true
	}
	return model, false
}

// ListModels 返回模型列表(原 initialize/handlers.go:engines)。
// ChatGPT 网页逆向的硬编码列表(owned_by=openai) + provider 聚合的模型(owned_by=deepseek 等)。
// 兼容性:OpenAI 官方 /v1/models 无显示名字段,friendly 用非标准字段 `friendly_name`
// 附加返回;id 保持真实值(请求仍用 id),不识别该字段的客户端自动忽略。
func (h *ModelsHandler) ListModels(c *gin.Context) {
	type ResData struct {
		ID           string   `json:"id"`
		Object       string   `json:"object"`
		Created      int      `json:"created"`
		OwnedBy      string   `json:"owned_by"`
		Capabilities []string `json:"capabilities,omitempty"`
		FriendlyName string   `json:"friendly_name,omitempty"`
	}

	type JSONData struct {
		Object string    `json:"object"`
		Data   []ResData `json:"data"`
	}

	// ChatGPT 网页逆向实际可用模型。
	// 模型 id 在请求时原样透传上游,列表只保留真实存在的 slug。
	// 来源:2026-09-02 经 NUC Chrome(CDP 9222)已登录 chatgpt.com 标签页
	//       直接调用 /backend-api/models 实测,保留 5.6 家族 + Auto:
	//         slug=gpt-5-6       title=GPT-5.6 Luna        ← 当前默认最强模型
	//         slug=gpt-5-6-mini  title=GPT-5.6 Luna (Mini) ← 同 GPT-5.6 Luna 家族
	//         slug=auto          title=Auto
	// (GPT-5.5 / 5.3 Mini 等经用户确认不在映射内,故不暴露。)
	// gpt-coding 是 -coding 别名(非真实 slug):请求时改写为 gpt-5-6 并强制工具调用,
	//         标注 function_call 能力;其本身不出现在 ChatGPT 网页模型选择器里,故单列于后。
	models := []string{
		"auto",
		"gpt-5-6",
		"gpt-5-6-mini",
	}
	// -coding 变体别名(强制工具调用),单独追加,不计入网页选择器 slug。
	codingAlias := "gpt-coding"

	var resModelList []ResData
	for _, model := range models {
		entry := ResData{
			ID:      model,
			Object:  "model",
			Created: 1685474247,
			OwnedBy: "openai",
		}
		if _, coding := normalizeCodingModel(model); coding {
			entry.Capabilities = []string{string(provider.CapFunctionCall)}
		}
		resModelList = append(resModelList, entry)
	}

	// 追加 -coding 别名(gpt-coding → gpt-5-6 透传上游,强制工具调用)。
	// coding 封存(2026-09-02):开关关闭时不暴露(见 config.CodingEnabled)。
	if h.codingEnabled {
	if _, coding := normalizeCodingModel(codingAlias); coding {
		resModelList = append(resModelList, ResData{
			ID:           codingAlias,
			Object:       "model",
			Created:      1685474247,
			OwnedBy:      "openai",
			Capabilities: []string{string(provider.CapFunctionCall)},
		})
	}
	}

	// 聚合 provider 模型(DeepSeek 等),并附能力标注。
	if h.registry != nil {
		for _, m := range h.registry.Models() {
			if !h.codingEnabled && isCodingModelName(m.ID) {
				continue // coding 封存:不暴露 -coding 变体
			}
			var caps []string
			for _, cap := range m.Caps {
				caps = append(caps, string(cap))
			}
			resModelList = append(resModelList, ResData{
				ID:           m.ID,
				Object:       "model",
				Created:      1685474247,
				OwnedBy:      m.OwnedBy,
				Capabilities: dedupeCaps(caps),
			})
		}
	}

	// 去重:同 ID 保留带能力标注的更完整条目(ChatgptCDP 经 registry 聚合后
	// 可能与手写列表重复出现 gpt-5-6 / gpt-5-6-mini)。
	deduped := make([]ResData, 0, len(resModelList))
	seenIdx := make(map[string]int)
	for _, mdl := range resModelList {
		if i, ok := seenIdx[mdl.ID]; ok {
			if len(mdl.Capabilities) > 0 && len(deduped[i].Capabilities) == 0 {
				deduped[i] = mdl
			}
			continue
		}
		seenIdx[mdl.ID] = len(deduped)
		deduped = append(deduped, mdl)
	}

	c.JSON(200, JSONData{
		Object: "list",
		Data:   deduped,
	})
}

func dedupeCaps(caps []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, c := range caps {
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	return out
}
