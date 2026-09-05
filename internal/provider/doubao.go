package provider

import (
	"strings"

	"aurora/internal/apierrors"
	"aurora/internal/config"
	"aurora/internal/doubaoweb"
	"aurora/typings/official"

	"github.com/gin-gonic/gin"
)

// doubao 模型变体
const (
	doubaoVariantChat = "chat" // 纯真人对话,不注入工具
	// doubaoVariantCoding = "coding" // 文本协议工具调用(已注释禁用,豆包只做 chat)
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
	"doubao",
	// "doubao-coding", // 已注释禁用(豆包只做 chat)
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
	// 前缀保护(2026-09-04: 精确放行 "doubao" 无后缀暴露名)
	if id != "doubao" && !strings.HasPrefix(id, "doubao-") {
		return nil
	}
	switch {
	case id == "doubao":
		// 2026-09-04 去 -chat 后缀
		return &doubaoModel{ID: id, Variant: doubaoVariantChat, Caps: []Capability{CapReasoning, CapWebSearch}}
	// case strings.HasSuffix(id, "-coding"):
	// 	return &doubaoModel{ID: id, Variant: doubaoVariantCoding, Caps: []Capability{CapFunctionCall, CapReasoning}}
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

// webClient 每次请求重新加载账号参数(a_bogus/web_tab_id/ms_token 等为会话级签名,
// 由 PC 侧 capture-doubao.mjs 持续更新 doubao_accounts.json;缓存会导致签名过期失效)。
func (d *Doubao) webClient() *doubaoweb.Client {
	accounts, err := doubaoweb.LoadAccounts(d.cfg.DoubaoAccounts)
	if err != nil || len(accounts) == 0 {
		return nil
	}
	return doubaoweb.NewClient(accounts)
}

// Responses 处理豆包 chat 变体(/v1/responses)。
func (d *Doubao) Responses(c *gin.Context, req *official.ResponsesAPIRequest) {
	m, ok := d.byID[req.Model]
	if !ok {
		apierrors.JSONError(c, 404, "invalid_request_error", "model not found", nil, "model_not_found")
		return
	}
	d.chatResponses(c, m, req)
	// 原 coding 分支(已注释禁用,豆包只做 chat):
	// if m.Variant == doubaoVariantChat {
	// 	d.chatResponses(c, m, req)
	// } else {
	// 	d.codingResponses(c, m, req)
	// }
}

// ChatCompletions 处理豆包 chat 变体(/v1/chat/completions)。
func (d *Doubao) ChatCompletions(c *gin.Context, req *official.APIRequest) {
	m, ok := d.byID[req.Model]
	if !ok {
		apierrors.JSONError(c, 404, "invalid_request_error", "model not found", nil, "model_not_found")
		return
	}
	d.chatCompletions(c, m, req)
	// 原 coding 分支(已注释禁用,豆包只做 chat):
	// if m.Variant == doubaoVariantChat {
	// 	d.chatCompletions(c, m, req)
	// } else {
	// 	d.codingCompletions(c, m, req)
	// }
}
