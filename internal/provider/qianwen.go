package provider

import (
	"strings"

	"aurora/internal/config"
	"aurora/internal/qianwenweb"
	"aurora/typings/official"

	"github.com/gin-gonic/gin"
)

// defaultQianwenModels 是 QIANWEN_MODELS 未配置时的默认目录。
// 网页真实模型 id(实测 qwen-3.8-max 可出流;默认款 Qwen3.7 见 docs/QIANWEN.md §〇)。
var defaultQianwenModels = []string{
	"qwen-3.8-max",
}

// Qianwen 实现 Provider 接口,走 www.qianwen.com 网页逆向。
//
// 千问网页 API 不支持自定义外部工具(实测 tools 字段被忽略,见 docs/QIANWEN.md §〇),
// 故只有纯对话 chat 形态,没有 coding 变体。
type Qianwen struct {
	cfg    *config.Config
	client *qianwenweb.Client
	models []Model
	byID   map[string]string
	// lastCookie 记录当前生效的池 cookie header,轮换失败时避免死循环。
	lastCookie string
}

// NewQianwen 构造千问 provider。
func NewQianwen(cfg *config.Config) *Qianwen {
	d := &Qianwen{cfg: cfg, byID: make(map[string]string)}
	ids := cfg.QianwenModels
	if len(ids) == 0 {
		ids = defaultQianwenModels
	}
	for _, id := range ids {
		if !isQianwenModel(id) {
			continue
		}
		d.byID[id] = id
		// 网页通道只提供纯文本对话:无工具/无 reasoning/未接识图,能力如实标注为空。
		d.models = append(d.models, Model{ID: id, OwnedBy: "alibaba"})
	}
	return d
}

// isQianwenModel 校验模型 id 形态(前缀保护,防误吃其它 provider 的模型 id)。
func isQianwenModel(id string) bool {
	id = strings.TrimSpace(id)
	return id != "" && strings.HasPrefix(strings.ToLower(id), "qwen")
}

func (d *Qianwen) Name() string { return "qianwen" }

func (d *Qianwen) Models() []Model { return d.models }

func (d *Qianwen) Handles(model string) bool {
	_, ok := d.byID[model]
	return ok
}

// webClient 惰性构造网页客户端。
func (d *Qianwen) webClient() *qianwenweb.Client {
	if d.client == nil {
		d.client = qianwenweb.NewClient(d.cfg.QianwenWebBase, d.cfg.QianwenWebTokens, "", "")
	}
	return d.client
}

// Responses 处理 /v1/responses。
func (d *Qianwen) Responses(c *gin.Context, req *official.ResponsesAPIRequest) {
	if _, ok := d.byID[req.Model]; !ok {
		c.JSON(404, gin.H{"error": "model not found"})
		return
	}
	d.qianwenChatResponses(c, req)
}

// ChatCompletions 处理 /v1/chat/completions。
func (d *Qianwen) ChatCompletions(c *gin.Context, req *official.APIRequest) {
	if _, ok := d.byID[req.Model]; !ok {
		c.JSON(404, gin.H{"error": "model not found"})
		return
	}
	d.qianwenChatCompletions(c, req)
}
