package provider

import (
	"strings"

	"aurora/internal/apierrors"
	"aurora/internal/config"
	"aurora/internal/geminweb"
	"aurora/typings/official"

	"github.com/gin-gonic/gin"
)

// gemini 模型变体
const (
	geminiVariantChat   = "chat"   // 纯真人对话(模型自动用原生搜索/工具)
	geminiVariantCoding = "coding" // 云端能力助手(搜索等原生能力,客户端工具尽力而为)
)

type geminiModel struct {
	ID      string
	Variant string
	Caps    []Capability
}

// Gemini 实现 Provider 接口,走 gemini.google.com 网页逆向。
type Gemini struct {
	cfg    *config.Config
	client *geminweb.Client
	models []Model
	byID   map[string]*geminiModel
}

// defaultGeminiModels 是 GEMINI_MODELS 未配置时的默认目录。
var defaultGeminiModels = []string{
	"gemini-3-flash",
	"gemini-3-flash-coding",
}

// NewGemini 构造 Gemini provider。无账号池时仍可构造(请求时返回 502)。
func NewGemini(cfg *config.Config) *Gemini {
	d := &Gemini{cfg: cfg, byID: make(map[string]*geminiModel)}
	ids := cfg.GeminiModels
	if len(ids) == 0 {
		ids = defaultGeminiModels
	}
	for _, id := range ids {
		m := parseGeminiModel(id)
		if m == nil {
			continue
		}
		d.byID[id] = m
		d.models = append(d.models, Model{ID: id, OwnedBy: "google", Caps: m.Caps})
	}
	return d
}

func parseGeminiModel(id string) *geminiModel {
	id = strings.TrimSpace(id)
	// 前缀保护:-chat/-coding 后缀太通用,必须 gemini- 开头。
	if !strings.HasPrefix(id, "gemini-") {
		return nil
	}
	switch {
	case id == "gemini-3-flash":
		// chat:真人对话(2026-09-04 去 -chat 后缀)。Gemini 模型自动用原生搜索/地图等。
		return &geminiModel{ID: id, Variant: geminiVariantChat, Caps: []Capability{CapWebSearch, CapReasoning}}
	case strings.HasSuffix(id, "-coding"):
		// coding:云端能力助手(搜索/绘图等原生能力;客户端工具调用不保证)。
		return &geminiModel{ID: id, Variant: geminiVariantCoding, Caps: []Capability{CapWebSearch, CapReasoning}}
	default:
		return nil
	}
}

func (d *Gemini) Name() string { return "google" }

func (d *Gemini) Models() []Model { return d.models }

func (d *Gemini) Handles(model string) bool {
	_, ok := d.byID[model]
	return ok
}

// webClient 惰性构造网页客户端(账号池来自 JSON 文件)。
func (d *Gemini) webClient() *geminweb.Client {
	if d.client == nil {
		accounts, err := geminweb.LoadAccounts(d.cfg.GeminiAccounts)
		if err != nil {
			d.client = nil
			return nil
		}
		d.client = geminweb.NewClient(accounts)
	}
	return d.client
}

// Responses 按模型 id 路由 chat / coding 变体。
func (d *Gemini) Responses(c *gin.Context, req *official.ResponsesAPIRequest) {
	m, ok := d.byID[req.Model]
	if !ok {
		apierrors.JSONError(c, 404, "invalid_request_error", "model not found", nil, "model_not_found")
		return
	}
	if m.Variant == geminiVariantChat {
		d.chatResponses(c, m, req)
	} else {
		d.codingResponses(c, m, req)
	}
}

// ChatCompletions 按模型 id 路由 chat / coding 变体。
func (d *Gemini) ChatCompletions(c *gin.Context, req *official.APIRequest) {
	m, ok := d.byID[req.Model]
	if !ok {
		apierrors.JSONError(c, 404, "invalid_request_error", "model not found", nil, "model_not_found")
		return
	}
	if m.Variant == geminiVariantChat {
		d.chatCompletions(c, m, req)
	} else {
		d.codingCompletions(c, m, req)
	}
}
