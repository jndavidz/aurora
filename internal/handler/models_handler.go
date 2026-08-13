package handler

import (
	"strings"

	"aurora/internal/provider"

	"github.com/gin-gonic/gin"
)

type ModelsHandler struct {
	registry *provider.Registry
}

func NewModelsHandler(registry *provider.Registry) *ModelsHandler {
	return &ModelsHandler{registry: registry}
}

// chatgptCodingBases 是允许 -coding 后缀变体的 ChatGPT 基础模型 slug。
// 请求 gpt-5-6-coding 时路由改写为 gpt-5-6 透传上游,并强制工具调用模式。
var chatgptCodingBases = []string{"gpt-5-6"}

// normalizeCodingModel 检测 ChatGPT 的 -coding 变体后缀:
//   - gpt-5-6-coding → (gpt-5-6, true):改写为基础模型,强制工具调用
//   - 其他 -coding(非已知 base)→ (原 id, false):不改写,走默认 ChatGPT(上游大概率 400)
//   - 无后缀 → (原 id, false)
func normalizeCodingModel(model string) (string, bool) {
	if !strings.HasSuffix(model, "-coding") {
		return model, false
	}
	base := strings.TrimSuffix(model, "-coding")
	for _, b := range chatgptCodingBases {
		if b == base {
			return base, true
		}
	}
	return model, false
}

// ListModels 返回模型列表(原 initialize/handlers.go:engines)。
// ChatGPT 网页逆向的硬编码列表(owned_by=openai) + provider 聚合的模型(owned_by=deepseek 等)。
func (h *ModelsHandler) ListModels(c *gin.Context) {
	type ResData struct {
		ID           string   `json:"id"`
		Object       string   `json:"object"`
		Created      int      `json:"created"`
		OwnedBy      string   `json:"owned_by"`
		Capabilities []string `json:"capabilities,omitempty"`
	}

	type JSONData struct {
		Object string    `json:"object"`
		Data   []ResData `json:"data"`
	}

	// ChatGPT 网页逆向实际可用模型(2026-08-14 抓包 /backend-api/models,免费账号视角)。
	// 模型 id 在请求时原样透传上游,列表只保留真实存在的 slug。
	// gpt-5-6-coding 是 -coding 变体(强制工具调用),标注 function_call 能力。
	models := []string{
		"auto",
		"gpt-5-5",
		"gpt-5-6",
		"gpt-5-5-mini",
		"gpt-5-6-mini",
		"gpt-5-6-coding",
	}

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

	// 聚合 provider 模型(DeepSeek 等),并附能力标注。
	if h.registry != nil {
		for _, m := range h.registry.Models() {
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

	c.JSON(200, JSONData{
		Object: "list",
		Data:   resModelList,
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
