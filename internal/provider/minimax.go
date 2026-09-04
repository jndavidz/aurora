package provider

import (
	"os"
	"strings"
	"time"

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
	models []Model
	byID   map[string]*minimaxModel
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
func (d *Minimax) webClient() *minimaxweb.Client {
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
	}
	return d.client
}

// Responses 按模型 id 路由 chat / coding 变体。
func (d *Minimax) Responses(c *gin.Context, req *official.ResponsesAPIRequest) {
	m, ok := d.byID[req.Model]
	if !ok {
		c.JSON(404, gin.H{"error": "model not found"})
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
		c.JSON(404, gin.H{"error": "model not found"})
		return
	}
	if m.Variant == minimaxVariantChat {
		d.chatCompletions(c, m, req)
	} else {
		d.codingCompletions(c, m, req)
	}
}
