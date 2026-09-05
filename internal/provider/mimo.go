package provider

import (
	"os"
	"strings"
	"time"

	"aurora/internal/apierrors"
	"aurora/internal/config"
	"aurora/internal/mimoweb"
	"aurora/typings/official"

	"github.com/gin-gonic/gin"
)

// mimo 模型变体
const (
	mimoVariantChat   = "chat"   // 纯真人对话(默认模型 mimo-v2.5-pro)
	mimoVariantCoding = "coding" // 工具调用(围栏 JSON + FenceParser)
	mimoVariantASR    = "asr"    // 语音识别(经 /v1/audio/transcriptions 调用)
)

type mimoModel struct {
	ID      string
	Variant string
	Caps    []Capability
}

// Mimo 实现 Provider 接口,走 aistudio.xiaomimimo.com 网页逆向(直连)。
type Mimo struct {
	cfg    *config.Config
	client *mimoweb.Client
	// E3(2026-09-05)凭证热加载:记录 token 文件 mtime,变化即重建 client
	tokenMod time.Time
	models   []Model
	byID     map[string]*mimoModel
	// coding 限频(chat 不限)
	limiter *CodingLimiter
}

// defaultMimoModels 是 MIMO_MODELS 未配置时的默认目录。
var defaultMimoModels = []string{"mimo-v2.5-pro", "mimo-v2.5-pro-coding", "mimo-v2.5-asr"}

// NewMimo 构造 Mimo provider。无 token 池时仍可构造(请求时返回 502)。
func NewMimo(cfg *config.Config) *Mimo {
	d := &Mimo{
		cfg:     cfg,
		byID:    make(map[string]*mimoModel),
		limiter: NewCodingLimiter(1500*time.Millisecond, 1500*time.Millisecond),
	}
	ids := cfg.MimoModels
	if len(ids) == 0 {
		ids = defaultMimoModels
	}
	for _, id := range ids {
		m := parseMimoModel(id)
		if m == nil {
			continue
		}
		d.byID[id] = m
		d.models = append(d.models, Model{ID: id, OwnedBy: "xiaomi", Caps: m.Caps})
	}
	return d
}

func parseMimoModel(id string) *mimoModel {
	id = strings.TrimSpace(id)
	if id != "mimo-v2.5-pro" && !strings.HasPrefix(id, "mimo-") {
		return nil
	}
	switch {
	case id == "mimo-v2.5-pro":
		// 2026-09-04 去 -chat 后缀
		return &mimoModel{ID: id, Variant: mimoVariantChat, Caps: []Capability{CapWebSearch, CapReasoning}}
	case strings.HasSuffix(id, "-coding"):
		return &mimoModel{ID: id, Variant: mimoVariantCoding, Caps: []Capability{CapFunctionCall, CapReasoning}}
	case strings.HasSuffix(id, "-asr"):
		return &mimoModel{ID: id, Variant: mimoVariantASR, Caps: []Capability{"asr"}}
	}
	return nil
}

func (d *Mimo) Name() string { return "xiaomi" }

func (d *Mimo) Models() []Model { return d.models }

func (d *Mimo) Handles(model string) bool {
	_, ok := d.byID[model]
	return ok
}

// webClient 惰性构造客户端(token 池来自文件,每行一个 xiaomichatbot_ph)。
// E3:每次调用先检查 token 文件 mtime,变化即置空重建(keeper scp 重推后
// 进程内生效)。stat 失败沿用旧池;无锁与其他 provider 一致(良性竞态)。
func (d *Mimo) webClient() *mimoweb.Client {
	if d.client != nil {
		if fi, err := os.Stat(d.cfg.MimoWebTokens); err == nil && !fi.ModTime().Equal(d.tokenMod) {
			d.client = nil
		}
	}
	if d.client == nil {
		data, err := os.ReadFile(d.cfg.MimoWebTokens)
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
		d.client = mimoweb.NewClient(tokens)
		if fi, statErr := os.Stat(d.cfg.MimoWebTokens); statErr == nil {
			d.tokenMod = fi.ModTime()
		}
	}
	return d.client
}

// AsrText 供 /v1/audio/transcriptions 调用:上传音频并识别。
func (d *Mimo) AsrText(fileName string, audio []byte) (string, error) {
	client := d.webClient()
	if client == nil || !client.HasToken() {
		return "", os.ErrNotExist
	}
	token := client.NextToken()
	return client.ASR(token, fileName, audio)
}

// Responses 按模型 id 路由 chat / coding 变体(asr 不走对话接口)。
func (d *Mimo) Responses(c *gin.Context, req *official.ResponsesAPIRequest) {
	m, ok := d.byID[req.Model]
	if !ok {
		apierrors.JSONError(c, 404, "invalid_request_error", "model not found", nil, "model_not_found")
		return
	}
	if m.Variant == mimoVariantASR {
		apierrors.JSONError(c, 400, "invalid_request_error", "mimo-v2.5-asr 请使用 /v1/audio/transcriptions 接口", nil, "invalid_request_error")
		return
	}
	if m.Variant == mimoVariantChat {
		d.chatResponses(c, m, req)
	} else {
		d.codingResponses(c, m, req)
	}
}

// ChatCompletions 按模型 id 路由 chat / coding 变体。
func (d *Mimo) ChatCompletions(c *gin.Context, req *official.APIRequest) {
	m, ok := d.byID[req.Model]
	if !ok {
		apierrors.JSONError(c, 404, "invalid_request_error", "model not found", nil, "model_not_found")
		return
	}
	if m.Variant == mimoVariantASR {
		apierrors.JSONError(c, 400, "invalid_request_error", "mimo-v2.5-asr 请使用 /v1/audio/transcriptions 接口", nil, "invalid_request_error")
		return
	}
	if m.Variant == mimoVariantChat {
		d.chatCompletions(c, m, req)
	} else {
		d.codingCompletions(c, m, req)
	}
}
