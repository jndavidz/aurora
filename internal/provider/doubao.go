package provider

import (
	"strings"

	"aurora/internal/config"
	"aurora/internal/doubaoweb"
	"aurora/typings/official"

	"github.com/gin-gonic/gin"
)

// doubao 模型变体
const (
	doubaoVariantChat   = "chat"   // 纯真人对话,不注入工具
	doubaoVariantCoding = "coding" // 文本协议工具调用
)

type doubaoModel struct {
	ID      string
	Variant string
	Caps    []Capability
}

// Doubao 实现 Provider 接口,走 www.doubao.com 网页逆向。
type Doubao struct {
	cfg    *config.Config
	client *doubaoweb.Client
	models []Model
	byID   map[string]*doubaoModel
}

// defaultDoubaoModels 是 DOUBAO_MODELS 未配置时的默认目录。
var defaultDoubaoModels = []string{
	"doubao-chat",
	"doubao-coding",
}

// NewDoubao 构造豆包 provider。
func NewDoubao(cfg *config.Config) *Doubao {
	d := &Doubao{cfg: cfg, byID: make(map[string]*doubaoModel)}
	ids := cfg.DoubaoModels
	if len(ids) == 0 {
		ids = defaultDoubaoModels
	}
	for _, id := range ids {
		m := parseDoubaoModel(id)
		if m == nil {
			continue
		}
		d.byID[id] = m
		d.models = append(d.models, Model{ID: id, OwnedBy: "bytedance", Caps: m.Caps})
	}
	return d
}

func parseDoubaoModel(id string) *doubaoModel {
	id = strings.TrimSpace(id)
	// 前缀保护
	if !strings.HasPrefix(id, "doubao-") {
		return nil
	}
	switch {
	case strings.HasSuffix(id, "-chat"):
		return &doubaoModel{ID: id, Variant: doubaoVariantChat, Caps: []Capability{CapReasoning, CapWebSearch}}
	case strings.HasSuffix(id, "-coding"):
		return &doubaoModel{ID: id, Variant: doubaoVariantCoding, Caps: []Capability{CapFunctionCall, CapReasoning}}
	default:
		return nil
	}
}

func (d *Doubao) Name() string { return "doubao" }

func (d *Doubao) Models() []Model { return d.models }

func (d *Doubao) Handles(model string) bool {
	_, ok := d.byID[model]
	return ok
}

// webClient 惰性构造网页客户端。
func (d *Doubao) webClient() *doubaoweb.Client {
	if d.client == nil {
		accounts, err := doubaoweb.LoadAccounts(d.cfg.DoubaoAccounts)
		if err != nil {
			d.client = nil
			return nil
		}
		d.client = doubaoweb.NewClient(accounts)
	}
	return d.client
}

// Responses 按模型 id 路由 chat / coding 变体。
func (d *Doubao) Responses(c *gin.Context, req *official.ResponsesAPIRequest) {
	m, ok := d.byID[req.Model]
	if !ok {
		c.JSON(404, gin.H{"error": "model not found"})
		return
	}
	if m.Variant == doubaoVariantChat {
		d.chatResponses(c, m, req)
	} else {
		d.codingResponses(c, m, req)
	}
}

// ChatCompletions 按模型 id 路由 chat / coding 变体。
func (d *Doubao) ChatCompletions(c *gin.Context, req *official.APIRequest) {
	m, ok := d.byID[req.Model]
	if !ok {
		c.JSON(404, gin.H{"error": "model not found"})
		return
	}
	if m.Variant == doubaoVariantChat {
		d.chatCompletions(c, m, req)
	} else {
		d.codingCompletions(c, m, req)
	}
}
