package handler

import (
	"aurora/internal/provider"

	"github.com/gin-gonic/gin"
)

type ModelsHandler struct {
	registry *provider.Registry
}

func NewModelsHandler(registry *provider.Registry) *ModelsHandler {
	return &ModelsHandler{registry: registry}
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
	models := []string{
		"auto",
		"gpt-5-5",
		"gpt-5-6",
		"gpt-5-5-mini",
		"gpt-5-6-mini",
	}

	var resModelList []ResData
	for _, model := range models {
		resModelList = append(resModelList, ResData{
			ID:      model,
			Object:  "model",
			Created: 1685474247,
			OwnedBy: "openai",
		})
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
