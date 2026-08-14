package provider

import (
	"strings"
	"time"

	"aurora/internal/config"
	"aurora/internal/kimiweb"
	"aurora/typings/official"

	"github.com/gin-gonic/gin"
)

// kimi 模型变体
const (
	kimiVariantChat   = "chat"   // 纯真人对话,不注入工具
	kimiVariantCoding = "coding" // 注入工具上下文 + 原生工具透传(见 docs/KIMI.md)
)

type kimiModel struct {
	ID      string
	Variant string
	Caps    []Capability
}

// Kimi 实现 Provider 接口,走 www.kimi.com 网页逆向(快速模式 K2.6)。
type Kimi struct {
	cfg    *config.Config
	client *kimiweb.Client
	models []Model
	byID   map[string]*kimiModel
	// lastToken 记录当前生效的池 token,轮换失败时避免死循环。
	lastToken string
	// coding 限频(chat 不限)
	limiter *CodingLimiter
}

// defaultKimiModels 是 KIMI_MODELS 未配置时的默认目录。
var defaultKimiModels = []string{
	"kimi-chat",
	"kimi-coding",
}

// NewKimi 构造 Kimi provider。
func NewKimi(cfg *config.Config) *Kimi {
	d := &Kimi{cfg: cfg, byID: make(map[string]*kimiModel), limiter: NewCodingLimiter(1500*time.Millisecond, 1500*time.Millisecond)}
	ids := cfg.KimiModels
	if len(ids) == 0 {
		ids = defaultKimiModels
	}
	for _, id := range ids {
		m := parseKimiModel(id)
		if m == nil {
			continue
		}
		d.byID[id] = m
		d.models = append(d.models, Model{ID: id, OwnedBy: "moonshot", Caps: m.Caps})
	}
	return d
}

func parseKimiModel(id string) *kimiModel {
	id = strings.TrimSpace(id)
	// 前缀保护:-chat/-coding 后缀太通用(gpt-5-chat 等),必须 kimi- 开头。
	if !strings.HasPrefix(id, "kimi-") {
		return nil
	}
	switch {
	case strings.HasSuffix(id, "-chat"):
		return &kimiModel{ID: id, Variant: kimiVariantChat, Caps: []Capability{CapReasoning, CapWebSearch}}
	case strings.HasSuffix(id, "-coding"):
		return &kimiModel{ID: id, Variant: kimiVariantCoding, Caps: []Capability{CapFunctionCall, CapReasoning}}
	default:
		return nil
	}
}

func (d *Kimi) Name() string { return "kimi" }

func (d *Kimi) Models() []Model { return d.models }

func (d *Kimi) Handles(model string) bool {
	_, ok := d.byID[model]
	return ok
}

// client 惰性构造网页客户端。
func (d *Kimi) webClient() *kimiweb.Client {
	if d.client == nil {
		d.client = kimiweb.NewClient(d.cfg.KimiWebBase, d.cfg.KimiWebTokens)
	}
	return d.client
}

// Responses 按模型 id 路由 chat / coding 变体。
func (d *Kimi) Responses(c *gin.Context, req *official.ResponsesAPIRequest) {
	m, ok := d.byID[req.Model]
	if !ok {
		c.JSON(404, gin.H{"error": "model not found"})
		return
	}
	if m.Variant == kimiVariantChat {
		d.chatResponses(c, m, req)
	} else {
		d.codingResponses(c, m, req)
	}
}

// ChatCompletions 按模型 id 路由 chat / coding 变体。
func (d *Kimi) ChatCompletions(c *gin.Context, req *official.APIRequest) {
	m, ok := d.byID[req.Model]
	if !ok {
		c.JSON(404, gin.H{"error": "model not found"})
		return
	}
	if m.Variant == kimiVariantChat {
		d.chatCompletions(c, m, req)
	} else {
		d.codingCompletions(c, m, req)
	}
}
