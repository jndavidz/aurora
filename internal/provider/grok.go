package provider

import (
	"strings"

	"aurora/internal/config"
	"aurora/internal/grokweb"
	"aurora/typings/official"

	"github.com/gin-gonic/gin"
)

// grok 模型变体
const (
	grokVariantChat   = "chat"   // 纯真人对话(模型自动用原生搜索/沙盒)
	grokVariantCoding = "coding" // 云端沙盒代码执行助手(同智谱定位)
)

type grokModel struct {
	ID      string
	Variant string
	Caps    []Capability
}

// Grok 实现 Provider 接口,走 grok.com 网页逆向(WebSocket)。
type Grok struct {
	cfg    *config.Config
	client *grokweb.Client
	models []Model
	byID   map[string]*grokModel
}

// defaultGrokModels 是 GROK_MODELS 未配置时的默认目录。
var defaultGrokModels = []string{
	"grok-3-chat",
	"grok-3-coding",
}

// NewGrok 构造 Grok provider。无账号池时仍可构造(返回 502 时提示)。
func NewGrok(cfg *config.Config) *Grok {
	d := &Grok{cfg: cfg, byID: make(map[string]*grokModel)}
	ids := cfg.GrokModels
	if len(ids) == 0 {
		ids = defaultGrokModels
	}
	for _, id := range ids {
		m := parseGrokModel(id)
		if m == nil {
			continue
		}
		d.byID[id] = m
		d.models = append(d.models, Model{ID: id, OwnedBy: "xai", Caps: m.Caps})
	}
	return d
}

func parseGrokModel(id string) *grokModel {
	id = strings.TrimSpace(id)
	// 前缀保护:-chat/-coding 后缀太通用,必须 grok- 开头。
	if !strings.HasPrefix(id, "grok-") {
		return nil
	}
	switch {
	case strings.HasSuffix(id, "-chat"):
		// chat:真人对话。Grok 模型自动用原生搜索+沙盒,能力如实标注。
		return &grokModel{ID: id, Variant: grokVariantChat, Caps: []Capability{CapWebSearch, CapReasoning, CapSandboxCode}}
	case strings.HasSuffix(id, "-coding"):
		// coding:云端沙盒代码执行助手(见 docs/GLM.md §四 同款定位)。
		return &grokModel{ID: id, Variant: grokVariantCoding, Caps: []Capability{CapSandboxCode, CapReasoning, CapWebSearch}}
	default:
		return nil
	}
}

func (d *Grok) Name() string { return "xai" }

func (d *Grok) Models() []Model { return d.models }

func (d *Grok) Handles(model string) bool {
	_, ok := d.byID[model]
	return ok
}

// webClient 惰性构造网页客户端(账号池来自 cookie 文件)。
func (d *Grok) webClient() *grokweb.Client {
	if d.client == nil {
		c, err := grokweb.NewClient(d.cfg.GrokCookies)
		if err != nil {
			d.client = nil
			return nil
		}
		d.client = c
	}
	return d.client
}

// Responses 按模型 id 路由 chat / coding 变体。
func (d *Grok) Responses(c *gin.Context, req *official.ResponsesAPIRequest) {
	m, ok := d.byID[req.Model]
	if !ok {
		c.JSON(404, gin.H{"error": "model not found"})
		return
	}
	if m.Variant == grokVariantChat {
		d.chatResponses(c, m, req)
	} else {
		d.codingResponses(c, m, req)
	}
}

// ChatCompletions 按模型 id 路由 chat / coding 变体。
func (d *Grok) ChatCompletions(c *gin.Context, req *official.APIRequest) {
	m, ok := d.byID[req.Model]
	if !ok {
		c.JSON(404, gin.H{"error": "model not found"})
		return
	}
	if m.Variant == grokVariantChat {
		d.chatCompletions(c, m, req)
	} else {
		d.codingCompletions(c, m, req)
	}
}
