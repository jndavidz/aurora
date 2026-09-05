package provider

import (
	"os"
	"strings"
	"time"

	"aurora/internal/apierrors"
	"aurora/internal/config"
	"aurora/internal/minimaxweb"
	"aurora/typings/official"

	"github.com/gin-gonic/gin"
)

// minimax 模型变体
const (
	minimaxVariantChat   = "chat"   // 纯真人对话(普通模式 variant:"")
	minimaxVariantCoding = "coding" // 工具调用(围栏 JSON + FenceParser)
)

type minimaxModel struct {
	ID      string
	Variant string
	Caps    []Capability
}

// Minimax 实现 Provider 接口,走 agent.minimaxi.com 网页逆向(直连)。
type Minimax struct {
	cfg    *config.Config
	client *minimaxweb.Client
	// E3(2026-09-05)凭证热加载:记录 token 文件 mtime,变化即重建 client
	tokenMod time.Time
	models   []Model
	byID     map[string]*minimaxModel
	// coding 限频(chat 不限)
	limiter *CodingLimiter
}

// defaultMinimaxModels 是 MINIMAX_MODELS 未配置时的默认目录。
var defaultMinimaxModels = []string{"minimax-m3", "minimax-m3-coding"}

// NewMinimax 构造 MiniMax provider。无 token 池时仍可构造(请求时返回 502)。
func NewMinimax(cfg *config.Config) *Minimax {
	d := &Minimax{
		cfg:     cfg,
		byID:    make(map[string]*minimaxModel),
		limiter: NewCodingLimiter(1500*time.Millisecond, 1500*time.Millisecond),
	}
	ids := cfg.MinimaxModels
	if len(ids) == 0 {
		ids = defaultMinimaxModels
	}
	for _, id := range ids {
		m := parseMinimaxModel(id)
		if m == nil {
			continue
		}
		d.byID[id] = m
		d.models = append(d.models, Model{ID: id, OwnedBy: "minimax", Caps: m.Caps})
	}
	return d
}

func parseMinimaxModel(id string) *minimaxModel {
	id = strings.TrimSpace(id)
	if id != "minimax-m3" && !strings.HasPrefix(id, "minimax-") {
		return nil
	}
	switch {
	case id == "minimax-m3":
		// 2026-09-04 去 -chat 后缀
		return &minimaxModel{ID: id, Variant: minimaxVariantChat, Caps: []Capability{CapWebSearch, CapReasoning}}
	case strings.HasSuffix(id, "-coding"):
		return &minimaxModel{ID: id, Variant: minimaxVariantCoding, Caps: []Capability{CapFunctionCall, CapReasoning}}
	}
	return nil
}

func (d *Minimax) Name() string { return "minimax" }

func (d *Minimax) Models() []Model { return d.models }

func (d *Minimax) Handles(model string) bool {
	_, ok := d.byID[model]
	return ok
}

// webClient 惰性构造客户端(token 池来自文件,每行一个 JWT)。
// E3:每次调用先检查 token 文件 mtime,变化即置空重建(keeper scp 重推 JWT
// 后进程内生效;重建丢弃 TLS 连接池,推送频率低可接受)。stat 失败(文件暂时
// 不可见)沿用旧池。d.client/d.tokenMod 无锁 —— 与其他 provider 一致,并发
// 双构造仅为良性浪费。
func (d *Minimax) webClient() *minimaxweb.Client {
	if d.client != nil {
		if fi, err := os.Stat(d.cfg.MinimaxWebTokens); err == nil && !fi.ModTime().Equal(d.tokenMod) {
			d.client = nil
		}
	}
	if d.client == nil {
		data, err := os.ReadFile(d.cfg.MinimaxWebTokens)
		if err != nil {
			return nil
		}
		var tokens []string
		for _, line := range strings.Split(string(data), "\n") {
			if t := strings.TrimSpace(line); t != "" {
				tokens = append(tokens, t)
			}
		}
		if len(tokens) == 0 {
			return nil
		}
		agentID := d.cfg.MinimaxAgentID
		if agentID == "" {
			agentID = minimaxweb.DefaultAgentID
		}
		d.client = minimaxweb.NewClient(tokens, agentID, d.cfg.MinimaxDeviceID, d.cfg.MinimaxUserID)
		if fi, statErr := os.Stat(d.cfg.MinimaxWebTokens); statErr == nil {
			d.tokenMod = fi.ModTime()
		}
	}
	return d.client
}

// Responses 按模型 id 路由 chat / coding 变体。
func (d *Minimax) Responses(c *gin.Context, req *official.ResponsesAPIRequest) {
	m, ok := d.byID[req.Model]
	if !ok {
		apierrors.JSONError(c, 404, "invalid_request_error", "model not found", nil, "model_not_found")
		return
	}
	if m.Variant == minimaxVariantChat {
		d.chatResponses(c, m, req)
	} else {
		d.codingResponses(c, m, req)
	}
}

// ChatCompletions 按模型 id 路由 chat / coding 变体。
func (d *Minimax) ChatCompletions(c *gin.Context, req *official.APIRequest) {
	m, ok := d.byID[req.Model]
	if !ok {
		apierrors.JSONError(c, 404, "invalid_request_error", "model not found", nil, "model_not_found")
		return
	}
	if m.Variant == minimaxVariantChat {
		d.chatCompletions(c, m, req)
	} else {
		d.codingCompletions(c, m, req)
	}
}
