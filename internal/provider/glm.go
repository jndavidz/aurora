package provider

import (
	"strings"

	"aurora/internal/config"
	"aurora/internal/glmweb"
	"aurora/typings/official"

	"github.com/gin-gonic/gin"
)

// glm 模型变体
const (
	glmVariantChat   = "chat"   // 纯真人对话,不注入工具
	glmVariantCoding = "coding" // 工具调用
)

type glmModel struct {
	ID      string
	Variant string
	Mode    string // glmweb ChatMode:"speed"(快速)|"thinking"(思考),仅 chat 变体用
	Caps    []Capability
}

// Glm 实现 Provider 接口,走 chatglm.cn 网页逆向。
type Glm struct {
	cfg    *config.Config
	client *glmweb.Client
	models []Model
	byID   map[string]*glmModel
	// lastToken 记录当前生效的池 token,轮换失败时避免死循环。
	lastToken string
}

// defaultGlmModels 是 GLM_MODELS 未配置时的默认目录。
var defaultGlmModels = []string{
	"glm-5.2-chat",          // 快速挡(默认)
	"glm-5.2-chat-thinking", // 思考挡
	"glm-5.2-coding",
}

// NewGlm 构造智谱 provider。
func NewGlm(cfg *config.Config) *Glm {
	d := &Glm{cfg: cfg, byID: make(map[string]*glmModel)}
	ids := cfg.GlmModels
	if len(ids) == 0 {
		ids = defaultGlmModels
	}
	for _, id := range ids {
		m := parseGlmModel(id)
		if m == nil {
			continue
		}
		d.byID[id] = m
		d.models = append(d.models, Model{ID: id, OwnedBy: "zhipu", Caps: m.Caps})
	}
	return d
}

func parseGlmModel(id string) *glmModel {
	id = strings.TrimSpace(id)
	// 前缀保护:必须 glm- 开头 + 版本白名单。
	// glm-5 系列已下线(用户 2026-08-14 决定只留 5.2),只接受 glm-5.2- 前缀。
	if !strings.HasPrefix(id, "glm-5.2-") {
		return nil
	}
	switch {
	case strings.HasSuffix(id, "-chat-thinking"):
		// 思考挡(chat_mode=thinking):深度思考 + 联网搜索 + 识图。
		return &glmModel{ID: id, Variant: glmVariantChat, Mode: "thinking", Caps: []Capability{CapReasoning, CapWebSearch, CapVision}}
	case strings.HasSuffix(id, "-chat"):
		// 快速挡(chat_mode=speed,默认):联网搜索 + 识图,无深度思考。
		// 用户 2026-08-14 决定默认快速而非 thinking。
		return &glmModel{ID: id, Variant: glmVariantChat, Mode: "speed", Caps: []Capability{CapWebSearch, CapVision}}
	case strings.HasSuffix(id, "-coding"):
		// 定位:云端沙箱代码执行助手(见 docs/GLM.md §四)。
		// 智谱模型无 function calling 训练,不标 CapFunctionCall,如实标 CapSandboxCode。
		return &glmModel{ID: id, Variant: glmVariantCoding, Caps: []Capability{CapSandboxCode, CapReasoning}}
	default:
		return nil
	}
}

func (d *Glm) Name() string { return "zhipu" }

func (d *Glm) Models() []Model { return d.models }

func (d *Glm) Handles(model string) bool {
	_, ok := d.byID[model]
	return ok
}

// client 惰性构造网页客户端。
func (d *Glm) webClient() *glmweb.Client {
	if d.client == nil {
		d.client = glmweb.NewClient(d.cfg.GlmWebBase, d.cfg.GlmWebTokens, "", "", "")
	}
	return d.client
}

// Responses 按模型 id 路由 chat / coding 变体。
func (d *Glm) Responses(c *gin.Context, req *official.ResponsesAPIRequest) {
	m, ok := d.byID[req.Model]
	if !ok {
		c.JSON(404, gin.H{"error": "model not found"})
		return
	}
	if m.Variant == glmVariantChat {
		d.chatResponses(c, m, req)
	} else {
		d.codingResponses(c, m, req)
	}
}

// ChatCompletions 按模型 id 路由 chat / coding 变体。
func (d *Glm) ChatCompletions(c *gin.Context, req *official.APIRequest) {
	m, ok := d.byID[req.Model]
	if !ok {
		c.JSON(404, gin.H{"error": "model not found"})
		return
	}
	if m.Variant == glmVariantChat {
		d.chatCompletions(c, m, req)
	} else {
		d.codingCompletions(c, m, req)
	}
}
