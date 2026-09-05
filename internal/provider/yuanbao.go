package provider

import (
	"strings"

	"aurora/internal/apierrors"
	"aurora/internal/config"
	"aurora/internal/yuanbaoweb"
	"aurora/typings/official"

	"github.com/gin-gonic/gin"
)

// yuanbaoModel 是元宝暴露模型的元数据。
type yuanbaoModel struct {
	ID          string // exposed id,如 hy3-chat
	Variant     string // variantChat | variantCoding
	ChatModelID string // 上游 chatModelId(hunyuan_gpt_175B_0404 / deep_seek_v3)
	Caps        []Capability
}

// Yuanbao 实现 Provider 接口,走 yuanbao.tencent.com 网页逆向(hy3 混元 + deepseek 双模型)。
type Yuanbao struct {
	cfg    *config.Config
	client *yuanbaoweb.Client
	models []Model
	byID   map[string]*yuanbaoModel
	// lastCred 记录当前生效的凭据(uskey+cookie),轮换失败时避免死循环。
	lastCred string
}

// defaultYuanbaoModels 是 YUANBAO_MODELS 未配置时的默认目录。
// 前缀约定:hy3- / yb-deepseek-(yb- 与 chat.deepseek.com 的现有 deepseek 模型区分)。
var defaultYuanbaoModels = []string{
	"hy3-chat",
	"hy3-coding",
	"yb-deepseek-chat",
	"yb-deepseek-coding",
}

// NewYuanbao 构造元宝 provider。
func NewYuanbao(cfg *config.Config) *Yuanbao {
	d := &Yuanbao{cfg: cfg, byID: make(map[string]*yuanbaoModel)}
	ids := cfg.YuanbaoModels
	if len(ids) == 0 {
		ids = defaultYuanbaoModels
	}
	for _, id := range ids {
		m := parseYuanbaoModel(id)
		if m == nil {
			continue
		}
		d.byID[id] = m
		d.models = append(d.models, Model{ID: id, OwnedBy: "tencent", Caps: m.Caps})
	}
	return d
}

// parseYuanbaoModel 从 exposed id 解析变体与上游模型。无法识别返回 nil。
// 前缀保护:只认 hy3- / yb-deepseek-,防 gpt-5-chat 等 id 误吃。
func parseYuanbaoModel(id string) *yuanbaoModel {
	id = strings.TrimSpace(id)
	variant := ""
	base := ""
	switch {
	case strings.HasSuffix(id, "-chat"):
		variant = variantChat
		base = strings.TrimSuffix(id, "-chat")
	case strings.HasSuffix(id, "-coding"):
		variant = variantCoding
		base = strings.TrimSuffix(id, "-coding")
	default:
		return nil
	}
	chatModelID := ""
	switch base {
	case "hy3":
		chatModelID = yuanbaoweb.ModelHy3
	case "yb-deepseek":
		chatModelID = yuanbaoweb.ModelDeepSeek
	default:
		return nil
	}
	caps := []Capability{CapWebSearch}
	if base == "yb-deepseek" {
		caps = append(caps, CapReasoning) // 网页定位"适合深度思考"
	}
	if variant == variantCoding {
		// coding 是文本协议工具调用(网页无原生 function calling,注入式模拟)。
		caps = []Capability{CapFunctionCall}
	}
	return &yuanbaoModel{ID: id, Variant: variant, ChatModelID: chatModelID, Caps: caps}
}

func (d *Yuanbao) Name() string { return "yuanbao" }

func (d *Yuanbao) Models() []Model { return d.models }

func (d *Yuanbao) Handles(model string) bool {
	_, ok := d.byID[model]
	return ok
}

// webClient 惰性构造网页客户端。
func (d *Yuanbao) webClient() *yuanbaoweb.Client {
	if d.client == nil {
		d.client = yuanbaoweb.NewClient(d.cfg.YuanbaoWebBase, d.cfg.YuanbaoWebTokens, d.cfg.YuanbaoAgentID)
	}
	return d.client
}

// Responses 按模型 id 路由 chat / coding 变体。
func (d *Yuanbao) Responses(c *gin.Context, req *official.ResponsesAPIRequest) {
	m, ok := d.byID[req.Model]
	if !ok {
		apierrors.JSONError(c, 404, "invalid_request_error", "model not found", nil, "model_not_found")
		return
	}
	switch m.Variant {
	case variantChat:
		d.yuanbaoChatResponses(c, m, req)
	case variantCoding:
		d.yuanbaoCodingResponses(c, m, req)
	default:
		apierrors.JSONError(c, 400, "invalid_request_error", "unknown variant", nil, "invalid_request_error")
	}
}

// ChatCompletions 按模型 id 路由 chat / coding 变体,输出 chat.completion 格式。
func (d *Yuanbao) ChatCompletions(c *gin.Context, req *official.APIRequest) {
	m, ok := d.byID[req.Model]
	if !ok {
		apierrors.JSONError(c, 404, "invalid_request_error", "model not found", nil, "model_not_found")
		return
	}
	switch m.Variant {
	case variantChat:
		d.yuanbaoChatCompletions(c, m, req)
	case variantCoding:
		d.yuanbaoCodingCompletions(c, m, req)
	default:
		apierrors.JSONError(c, 400, "invalid_request_error", "unknown variant", nil, "invalid_request_error")
	}
}
