package provider

import (
	"strings"
	"sync"
	"time"

	"aurora/internal/apierrors"
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
	// tokMu 串行化换发(并发请求同时换发会互相作废轮换链)
	tokMu sync.Mutex
	// coding 限频(chat 不限)
	limiter *CodingLimiter
}

// defaultKimiModels 是 KIMI_MODELS 未配置时的默认目录。
var defaultKimiModels = []string{
	"kimi",
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
	// 前缀保护:-chat/-coding 后缀太通用(gpt-5-chat 等),必须 kimi- 开头;
	// 2026-09-04: 暴露 id 改为 "kimi"(无后缀),精确放行。
	if id != "kimi" && !strings.HasPrefix(id, "kimi-") {
		return nil
	}
	switch {
	case id == "kimi":
		// 2026-09-04 去 -chat 后缀;kimi 暴露快速档(用户拍板)
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

// CredentialHealth 实现 HealthReporter:报告 Kimi 凭证池的过期时间线。
// refresh_token ~90 天有效且换发时轮换,池内最早过期者决定"距重抓还剩多久"。
func (d *Kimi) CredentialHealth() CredentialHealth {
	client := d.webClient()
	h := CredentialHealth{Name: d.Name(), Accounts: client.PoolSize()}
	if !fillRefreshExpiry(&h, client.RefreshTokenExps()) {
		h.Status = "empty"
		h.Detail = "refresh token 池为空或均无法解析 exp(当前 NAS 池即此状态,需 grab-kimi.mjs 重抓)"
	}
	return h
}

// Responses 按模型 id 路由 chat / coding 变体。
func (d *Kimi) Responses(c *gin.Context, req *official.ResponsesAPIRequest) {
	m, ok := d.byID[req.Model]
	if !ok {
		apierrors.JSONError(c, 404, "invalid_request_error", "model not found", nil, "model_not_found")
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
		apierrors.JSONError(c, 404, "invalid_request_error", "model not found", nil, "model_not_found")
		return
	}
	if m.Variant == kimiVariantChat {
		d.chatCompletions(c, m, req)
	} else {
		d.codingCompletions(c, m, req)
	}
}
