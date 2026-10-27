package handler

// provider_routing_test.go —— T05 分派接线的路由单测。
//
// 覆盖 B(原生工具透传优先)/ A(provider 工具模拟兜底)在两个表面上的分派
// 顺序与门禁:B 必须先于 ResolveCanonical,A 必须落在 provider 命中之内且
// 只在 /v1/chat/completions 生效。用假 Provider(可选择性实现 TextCollector)
// 记录调用,不触任何真实上游。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aurora/internal/config"
	"aurora/internal/provider"
	officialtypes "aurora/typings/official"

	"github.com/gin-gonic/gin"
)

// ── 假 Provider ──────────────────────────────────────────────────

// routingFakeProvider 记录被分派到哪个方法,并回写可断言的标记。
// 不实现 TextCollector(方案 A 的「非收集器」分支需要这个形态)。
type routingFakeProvider struct {
	name           string
	models         []provider.Model
	chatCalls      int
	responsesCalls int
}

func (f *routingFakeProvider) Name() string { return f.name }

func (f *routingFakeProvider) Models() []provider.Model { return f.models }

func (f *routingFakeProvider) Handles(model string) bool {
	for _, m := range f.models {
		if m.ID == model {
			return true
		}
	}
	return false
}

func (f *routingFakeProvider) Responses(c *gin.Context, _ *officialtypes.ResponsesAPIRequest) {
	f.responsesCalls++
	c.JSON(200, gin.H{"marker": f.name + "-responses"})
}

func (f *routingFakeProvider) ChatCompletions(c *gin.Context, _ *officialtypes.APIRequest) {
	f.chatCalls++
	c.JSON(200, gin.H{"marker": f.name + "-chat"})
}

// routingCollectingProvider 在假 Provider 之上实现 TextCollector,
// 用来验证方案 A 的模拟分支(记录收集次数,返回固定纯文本)。
type routingCollectingProvider struct {
	routingFakeProvider
	reply        string
	collectCalls int
}

func (f *routingCollectingProvider) CollectText(_ *gin.Context, _ *officialtypes.APIRequest) (string, error) {
	f.collectCalls++
	return f.reply, nil
}

var (
	_ provider.Provider      = (*routingFakeProvider)(nil)
	_ provider.TextCollector = (*routingCollectingProvider)(nil)
)

// ── 脚手架 ──────────────────────────────────────────────────────

func routingProvider(name, model string) *routingFakeProvider {
	return &routingFakeProvider{name: name, models: []provider.Model{{ID: model, OwnedBy: name}}}
}

func routingCollector(name, model, reply string) *routingCollectingProvider {
	return &routingCollectingProvider{
		routingFakeProvider: *routingProvider(name, model),
		reply:               reply,
	}
}

func routingHandler(cfg *config.Config, registry *provider.Registry) *ChatHandler {
	return &ChatHandler{cfg: cfg, providers: registry}
}

// routingPost 按 Nightmare/Responses 的 BindJSON 约定构造请求
// (必须带 Content-Type: application/json,否则 gin 走 form 绑定)。
func routingPost(path, body string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	return c, w
}

const routingChatBody = `{"model":"deepseek","messages":[{"role":"user","content":"hi"}],` +
	`"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object","properties":{}}}}]}`

const routingChatBodyNoTools = `{"model":"deepseek","messages":[{"role":"user","content":"hi"}]}`

const routingResponsesBody = `{"model":"deepseek","input":"hi",` +
	`"tools":[{"type":"function","function":{"name":"get_weather"}}]}`

// ── 1. 带 tools + 槽位命中 → native,provider 自身不被调用 ─────────

func TestNightmarePrefersNativeToolsProvider(t *testing.T) {
	reg := provider.NewRegistry()
	normal := routingProvider("deepseek", "deepseek")
	reg.Register(normal)
	native := routingProvider("native", "deepseek")
	reg.SetNativeTools(native)

	h := routingHandler(&config.Config{}, reg)
	c, w := routingPost("/v1/chat/completions", routingChatBody)
	h.Nightmare(c)

	if native.chatCalls != 1 {
		t.Errorf("native.ChatCompletions 调用次数 = %d, 想要 1", native.chatCalls)
	}
	if normal.chatCalls != 0 {
		t.Errorf("provider.ChatCompletions 调用次数 = %d, 想要 0(槽位优先)", normal.chatCalls)
	}
	if !strings.Contains(w.Body.String(), "native-chat") {
		t.Errorf("响应应来自 native; body=%s", w.Body.String())
	}
}

// ── 2. 槽位未命中 + 开关开 + 是 TextCollector → 走模拟 ────────────

func TestNightmareFallsBackToProviderToolEmulation(t *testing.T) {
	reg := provider.NewRegistry()
	collecting := routingCollector("deepseek", "deepseek", "plain answer")
	reg.Register(collecting)
	// 槽位登记的是别的模型:本次请求不该命中它。
	reg.SetNativeTools(routingProvider("native", "other-model"))

	cfg := &config.Config{ProviderToolEmulation: true, ProviderToolRetries: 0}
	h := routingHandler(cfg, reg)
	c, w := routingPost("/v1/chat/completions", routingChatBody)
	h.Nightmare(c)

	if collecting.collectCalls != 1 {
		t.Errorf("CollectText 调用次数 = %d, 想要 1", collecting.collectCalls)
	}
	if collecting.chatCalls != 0 {
		t.Errorf("provider.ChatCompletions 调用次数 = %d, 想要 0(模拟接管)", collecting.chatCalls)
	}
	if !strings.Contains(w.Body.String(), "plain answer") {
		t.Errorf("响应应含收集到的正文; body=%s", w.Body.String())
	}
}

// ── 3. 开关关 → 走 provider ChatCompletions(回归判据)─────────────

func TestNightmareEmulationSwitchOffUsesProviderChat(t *testing.T) {
	reg := provider.NewRegistry()
	collecting := routingCollector("deepseek", "deepseek", "plain answer")
	reg.Register(collecting)

	cfg := &config.Config{ProviderToolEmulation: false}
	h := routingHandler(cfg, reg)
	c, w := routingPost("/v1/chat/completions", routingChatBody)
	h.Nightmare(c)

	if collecting.collectCalls != 0 {
		t.Errorf("CollectText 调用次数 = %d, 想要 0(开关关)", collecting.collectCalls)
	}
	if collecting.chatCalls != 1 {
		t.Errorf("provider.ChatCompletions 调用次数 = %d, 想要 1", collecting.chatCalls)
	}
	if !strings.Contains(w.Body.String(), "deepseek-chat") {
		t.Errorf("响应应来自 provider; body=%s", w.Body.String())
	}
}

// ── 4. 开关开但不在白名单 → 走 provider ChatCompletions ───────────

func TestNightmareEmulationWhitelistMissUsesProviderChat(t *testing.T) {
	reg := provider.NewRegistry()
	collecting := routingCollector("deepseek", "deepseek", "plain answer")
	reg.Register(collecting)

	cfg := &config.Config{ProviderToolEmulation: true, ProviderToolModels: []string{"glm"}}
	h := routingHandler(cfg, reg)
	c, _ := routingPost("/v1/chat/completions", routingChatBody)
	h.Nightmare(c)

	if collecting.collectCalls != 0 {
		t.Errorf("CollectText 调用次数 = %d, 想要 0(不在白名单)", collecting.collectCalls)
	}
	if collecting.chatCalls != 1 {
		t.Errorf("provider.ChatCompletions 调用次数 = %d, 想要 1", collecting.chatCalls)
	}
}

// ── 5. 无 tools → 新旧分支都不进(回归判据)──────────────────────

func TestNightmareWithoutToolsSkipsNewBranches(t *testing.T) {
	reg := provider.NewRegistry()
	collecting := routingCollector("deepseek", "deepseek", "plain answer")
	reg.Register(collecting)
	native := routingProvider("native", "deepseek")
	reg.SetNativeTools(native)

	cfg := &config.Config{ProviderToolEmulation: true, ProviderToolRetries: 0}
	h := routingHandler(cfg, reg)
	c, _ := routingPost("/v1/chat/completions", routingChatBodyNoTools)
	h.Nightmare(c)

	if native.chatCalls != 0 {
		t.Errorf("native.ChatCompletions 调用次数 = %d, 想要 0(无 tools 不进 B)", native.chatCalls)
	}
	if collecting.collectCalls != 0 {
		t.Errorf("CollectText 调用次数 = %d, 想要 0(无 tools 不进 A)", collecting.collectCalls)
	}
	if collecting.chatCalls != 1 {
		t.Errorf("provider.ChatCompletions 调用次数 = %d, 想要 1", collecting.chatCalls)
	}
}

// ── 6. 开关开但 provider 非 TextCollector → ChatCompletions,不 panic ─

func TestNightmareNonCollectorProviderUsesChat(t *testing.T) {
	reg := provider.NewRegistry()
	plain := routingProvider("deepseek", "deepseek")
	reg.Register(plain)

	cfg := &config.Config{ProviderToolEmulation: true}
	h := routingHandler(cfg, reg)
	c, w := routingPost("/v1/chat/completions", routingChatBody)
	h.Nightmare(c)

	if plain.chatCalls != 1 {
		t.Errorf("provider.ChatCompletions 调用次数 = %d, 想要 1", plain.chatCalls)
	}
	if !strings.Contains(w.Body.String(), "deepseek-chat") {
		t.Errorf("响应应来自 provider; body=%s", w.Body.String())
	}
}

// ── 7. Responses + tools + 槽位命中 → 走 native.Responses ────────

func TestResponsesPrefersNativeToolsProvider(t *testing.T) {
	reg := provider.NewRegistry()
	normal := routingProvider("deepseek", "deepseek")
	reg.Register(normal)
	native := routingProvider("native", "deepseek")
	reg.SetNativeTools(native)

	h := routingHandler(&config.Config{}, reg)
	c, w := routingPost("/v1/responses", routingResponsesBody)
	h.Responses(c)

	if native.responsesCalls != 1 {
		t.Errorf("native.Responses 调用次数 = %d, 想要 1", native.responsesCalls)
	}
	if normal.responsesCalls != 0 {
		t.Errorf("provider.Responses 调用次数 = %d, 想要 0(槽位优先)", normal.responsesCalls)
	}
	if !strings.Contains(w.Body.String(), "native-responses") {
		t.Errorf("响应应来自 native; body=%s", w.Body.String())
	}
}

// ── 8. A 的白名单按 canonical 回填后的模型判断 ───────────────────

func TestNightmareEmulationUsesCanonicalModel(t *testing.T) {
	reg := provider.NewRegistry()
	collecting := routingCollector("deepseek", "deepseek", "plain answer")
	reg.Register(collecting)

	// 白名单只含真实 id;请求用规范化后等价的大写形式。
	cfg := &config.Config{
		ProviderToolEmulation: true,
		ProviderToolModels:    []string{"deepseek"},
		ProviderToolRetries:   0,
	}
	h := routingHandler(cfg, reg)
	body := `{"model":"DeepSeek","messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"function","function":{"name":"get_weather"}}]}`
	c, _ := routingPost("/v1/chat/completions", body)
	h.Nightmare(c)

	if collecting.collectCalls != 1 {
		t.Errorf("CollectText 调用次数 = %d, 想要 1(canonical 回填后命中白名单)", collecting.collectCalls)
	}
	if collecting.chatCalls != 0 {
		t.Errorf("provider.ChatCompletions 调用次数 = %d, 想要 0", collecting.chatCalls)
	}
}
