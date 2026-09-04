package provider

import (
	"strings"
	"time"

	"aurora/internal/config"
	"aurora/internal/deepseekweb"
	"aurora/typings/official"

	"github.com/gin-gonic/gin"
)

// deepseek 模型变体
const (
	variantChat   = "chat"   // 纯真人对话,绝不注入工具
	variantCoding = "coding" // 文本协议工具调用
)

// deepseekMode 是 chat 变体的网页模式。
const (
	modeQuick  = "quick"  // 快速模式:可联网搜索 + 识图
	modeExpert = "expert" // 专家模式:深度思考,无搜索/识图
)

type deepseekModel struct {
	ID      string
	Variant string
	Mode    string
	Caps    []Capability
}

// DeepSeek 实现 Provider 接口,走 chat.deepseek.com 网页逆向。
type DeepSeek struct {
	cfg    *config.Config
	client *deepseekweb.Client
	models []Model
	byID   map[string]*deepseekModel
	// coding 限频(chat 不限)
	limiter *CodingLimiter
}

// defaultDeepSeekModels 是 DEEPSEEK_MODELS 未配置时的默认目录。
var defaultDeepSeekModels = []string{
	"deepseek-v4-flash-chat",
	"deepseek-v4-pro-chat",
	"deepseek-v4-flash-coding",
	"deepseek-v4-pro-coding",
}

// NewDeepSeek 构造 DeepSeek provider。无 token 池时仍可构造(返回 502 时提示)。
func NewDeepSeek(cfg *config.Config) *DeepSeek {
	d := &DeepSeek{cfg: cfg, byID: make(map[string]*deepseekModel), limiter: NewCodingLimiter(1500*time.Millisecond, 1500*time.Millisecond)}
	ids := cfg.DeepSeekModels
	if len(ids) == 0 {
		ids = defaultDeepSeekModels
	}
	for _, id := range ids {
		m := parseDeepSeekModel(id)
		if m == nil {
			continue
		}
		// 搜索关闭时如实标注:quick 档不再宣称 CapWebSearch(能力注记与实际行为一致)。
		if !cfg.DeepSeekWebSearch {
			caps := make([]Capability, 0, len(m.Caps))
			for _, cap := range m.Caps {
				if cap != CapWebSearch {
					caps = append(caps, cap)
				}
			}
			m.Caps = caps
		}
		d.byID[id] = m
		d.models = append(d.models, Model{ID: id, OwnedBy: "deepseek", Caps: m.Caps})
	}
	return d
}

// parseDeepSeekModel 从 exposed id 解析变体与能力。无法识别返回 nil。
func parseDeepSeekModel(id string) *deepseekModel {
	id = strings.TrimSpace(id)
	switch {
	case strings.HasSuffix(id, "-chat"):
		base := strings.TrimSuffix(id, "-chat")
		mode := modeExpert
		caps := []Capability{CapReasoning}
		if strings.Contains(base, "flash") {
			mode = modeQuick
			caps = []Capability{CapWebSearch, CapReasoning, CapVision}
		}
		return &deepseekModel{ID: id, Variant: variantChat, Mode: mode, Caps: caps}
	case strings.HasSuffix(id, "-coding"):
		return &deepseekModel{
			ID:      id,
			Variant: variantCoding,
			Caps:    []Capability{CapFunctionCall, CapReasoning},
		}
	default:
		return nil
	}
}

func (d *DeepSeek) Name() string { return "deepseek" }

func (d *DeepSeek) Models() []Model { return d.models }

func (d *DeepSeek) Handles(model string) bool {
	_, ok := d.byID[model]
	return ok
}

// client 惰性构造网页客户端。
func (d *DeepSeek) webClient() *deepseekweb.Client {
	if d.client == nil {
		c, err := deepseekweb.NewClient(d.cfg.DeepSeekWebBase, d.cfg.DeepSeekWebTokens, d.cfg.DeepSeekProxy)
		if err != nil {
			// 构造失败保持 nil,响应时返回 502 并带错误信息。
			d.client = nil
			return nil
		}
		d.client = c
	}
	return d.client
}

// Responses 按模型 id 路由 chat / coding 变体。
func (d *DeepSeek) Responses(c *gin.Context, req *official.ResponsesAPIRequest) {
	m, ok := d.byID[req.Model]
	if !ok {
		// 不应发生:Handles 已拦截。
		c.JSON(404, gin.H{"error": "model not found"})
		return
	}
	switch m.Variant {
	case variantChat:
		d.chatResponses(c, m, req)
	case variantCoding:
		d.codingResponses(c, m, req)
	default:
		c.JSON(400, gin.H{"error": "unknown variant"})
	}
}

// ChatCompletions 按模型 id 路由 chat / coding 变体,输出 chat.completion 格式。
func (d *DeepSeek) ChatCompletions(c *gin.Context, req *official.APIRequest) {
	m, ok := d.byID[req.Model]
	if !ok {
		c.JSON(404, gin.H{"error": "model not found"})
		return
	}
	switch m.Variant {
	case variantChat:
		d.chatCompletions(c, m, req)
	case variantCoding:
		d.codingCompletions(c, m, req)
	default:
		c.JSON(400, gin.H{"error": "unknown variant"})
	}
}
