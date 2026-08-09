package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aurora/internal/accounts"
	"aurora/internal/config"
	officialtypes "aurora/typings/official"

	"github.com/gin-gonic/gin"
)

// ─── Test: writeChatCompletionStreamDone ─────────────────────────

func TestWriteChatCompletionStreamDoneAddsStopBeforeDone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	writer := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(writer)

	writeChatCompletionStreamDone(c, false, "auto", "conv-xxx")

	lines := sseDataLines(writer.Body.String())
	if len(lines) != 2 {
		t.Fatalf("data line count = %d, want 2; output: %s", len(lines), writer.Body.String())
	}
	var stopChunk map[string]interface{}
	if err := json.Unmarshal([]byte(lines[0]), &stopChunk); err != nil {
		t.Fatalf("invalid stop chunk: %v", err)
	}
	if stopChunk["conversation_id"] != "conv-xxx" {
		t.Fatalf("conversation_id = %#v, want conv-xxx", stopChunk["conversation_id"])
	}
	choices := stopChunk["choices"].([]interface{})
	if choices[0].(map[string]interface{})["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %#v, want stop", choices[0].(map[string]interface{})["finish_reason"])
	}
	if lines[1] != "[DONE]" {
		t.Fatalf("last data line = %q, want [DONE]", lines[1])
	}
}

func TestWriteChatCompletionStreamDoneSkipsDuplicateStop(t *testing.T) {
	gin.SetMode(gin.TestMode)
	writer := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(writer)

	writeChatCompletionStreamDone(c, true, "auto", "conv-xxx")

	lines := sseDataLines(writer.Body.String())
	if len(lines) != 1 || lines[0] != "[DONE]" {
		t.Fatalf("data lines = %#v, want only [DONE]", lines)
	}
}

// ─── Test: toolCallingEnabled ────────────────────────────────────

func TestToolCallingEnabledFromConfig(t *testing.T) {
	okCfg := &config.Config{ToolCallingEnabled: true}
	disabledCfg := &config.Config{ToolCallingEnabled: false}

	if toolCallingEnabled(nil, okCfg) {
		t.Error("toolCallingEnabled(nil, true) should be false (len(nil)==0)")
	}
	if toolCallingEnabled(nil, disabledCfg) {
		t.Error("toolCallingEnabled(nil, false) should be false")
	}
	// empty tools slice with config enabled → false
	if toolCallingEnabled([]officialtypes.Tool{}, okCfg) {
		t.Error("toolCallingEnabled([], true) should be false")
	}
	// with actual tools and config enabled → true
	tools := []officialtypes.Tool{{Type: "function", Function: officialtypes.ToolFunction{Name: "test"}}}
	if !toolCallingEnabled(tools, okCfg) {
		t.Error("toolCallingEnabled([tool], true) should be true")
	}
}

// ─── Test: original_requestHasFiles ──────────────────────────────

func TestOriginalRequestHasFiles(t *testing.T) {
	req := officialtypes.APIRequest{
		Messages: []officialtypes.APIMessage{
			{
				Role:    "user",
				Content: officialtypes.MessageContent{TextValue: "hello"},
			},
		},
	}
	if original_requestHasFiles(req) {
		t.Error("should be false when no files")
	}
}

// ─── Test: countMessagesTokens ───────────────────────────────────

func TestCountMessagesTokens(t *testing.T) {
	zero := countMessagesTokens(nil)
	if zero != 0 {
		t.Errorf("nil messages should return 0, got %d", zero)
	}
}

// ─── Test: resolveAccount ────────────────────────────────────────

func TestResolveAccountEmptyPool(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	pool := accounts.NewPool(nil)
	cfg := &config.Config{}

	acct, _, err := resolveAccount(c, pool, cfg, false)
	if err == nil {
		t.Fatal("expected error with empty pool")
	}
	if acct != nil {
		t.Fatal("expected nil account")
	}
}

func TestResolveAccountWithGlobalKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	c.Request.Header.Set("Authorization", "Bearer my-global-key")

	pool := accounts.NewPool(nil)
	acct := accounts.NewAccount("test", accounts.TypeFree, "test-token")
	acct.Status = accounts.StatusActive
	pool.AddAccount(acct)
	cfg := &config.Config{Authorization: "my-global-key"}

	result, _, err := resolveAccount(c, pool, cfg, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected account, got nil")
	}
	if result.Token != "test-token" {
		t.Errorf("got token %q, want test-token", result.Token)
	}
}

// ─── helpers ─────────────────────────────────────────────────────

func sseDataLines(output string) []string {
	var lines []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		lines = append(lines, strings.TrimPrefix(line, "data: "))
	}
	return lines
}

// ─── Test: looksLikeRequestingUserContent ────────────────────────

func TestLooksLikeRequestingUserContent(t *testing.T) {
	trueCases := []string{
		// 实测原文:模型拿到文件树后向用户索要源码正文
		"请把源码内容继续提供（或让我能读取工作区文件），我就继续。",
		"我可以读源码。请把项目路径、仓库内容，或直接上传/粘贴你想看的文件。",
		"Please provide the file contents so I can continue.",
		"Could you share the source files with me?",
		"请把 main.go 的内容发给我。",
		"你方便的话，把代码直接上传给我。",
	}
	falseCases := []string{
		"",
		"已读取文件内容，总结如下：项目结构清晰。",
		"The tool output above is the file listing. I will now summarize.",
		"我完成分析了。",
		"This tool will provide the output when executed.",
		"ls 输出正常。",
	}
	for _, s := range trueCases {
		if !looksLikeRequestingUserContent(s) {
			t.Errorf("want stall detection for %q", s)
		}
	}
	for _, s := range falseCases {
		if looksLikeRequestingUserContent(s) {
			t.Errorf("did not want stall detection for %q", s)
		}
	}
}

func TestLooksLikeRequestingUserContentVariants(t *testing.T) {
	// 实测变体:"请继续提供源码读取结果后,我会基于实际代码整理…"
	variants := []string{
		"请继续提供源码读取结果后，我会基于实际代码整理完整架构文档。",
		"拿到源码内容后，我会整理架构。",
		"等你提供文件内容我再继续。",
		"Please send me the file so I can analyze it.",
	}
	for _, s := range variants {
		if !looksLikeRequestingUserContent(s) {
			t.Errorf("want stall detection for variant %q", s)
		}
	}
}

// ─── Test: looksLikePrematureStop ────────────────────────────────

func TestLooksLikePrematureStop(t *testing.T) {
	// 实测原文:读完两个文件后输出进度报告停下,等用户发"继续"
	trueCases := []string{
		"我已经读完：\n- manifest.json\n- background.js 前半部分（消息入口、AI 注册表…）\n当前已确认核心架构：",
		"我继续通读。目前我已经确认了源码结构，但还没有拿到文件正文内容。我会按以下顺序通读：1. manifest.json 2. background.js",
		"我可以继续通读，但当前我还没有拿到源码正文，只看到了文件列表。读完后我会整理成：",
		"我继续通读源码。前面只读取到了文件列表，还没有逐文件读取正文。我会继续按源码顺序读取。",
		"I will continue reading the remaining files and summarize afterwards.",
		"Let me continue reading the next file first.",
	}
	falseCases := []string{
		"",
		"已通读全部源码，总结如下：项目是 MV3 扩展，入口为 background.js。",
		"已完成读取，结论：read_file 可用。",
		"The tool output above shows the file listing. Here is the complete summary.",
		"总结：工具调用成功。",
	}
	for _, s := range trueCases {
		if !looksLikePrematureStop(s) {
			t.Errorf("want premature-stop detection for %q", s)
		}
	}
	for _, s := range falseCases {
		if looksLikePrematureStop(s) {
			t.Errorf("did not want premature-stop detection for %q", s)
		}
	}
}

// ─── Test: writeToolCallingStream 不破坏多字节 UTF-8 ─────────────

func TestWriteToolCallingStreamPreservesUTF8(t *testing.T) {
	gin.SetMode(gin.TestMode)
	long := "我目前只能看到项目文件列表（目录结构），还没有获得源代码正文内容，因此不能负责任地声称“已经读完所有源码并完整分析”。仅凭文件名总结会变成猜测。" +
		"接下来我会按以下顺序逐个读取：manifest.json、background.js、sidepanel/panel.html、content/base.js，然后给出完整分析。"
	if len(long) <= 200 {
		t.Fatalf("test text too short: %d bytes", len(long))
	}
	writer := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(writer)
	h := &ChatHandler{}
	h.writeToolCallingStream(c, "auto", long, nil, "conv-1", 10, 50, false)

	var rebuilt string
	for _, line := range sseDataLines(writer.Body.String()) {
		if line == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 {
			rebuilt += chunk.Choices[0].Delta.Content
		}
	}
	if strings.Contains(rebuilt, "\uFFFD") {
		t.Fatalf("rebuilt text contains U+FFFD (byte-sliced UTF-8): %q", rebuilt)
	}
	if rebuilt != long {
		t.Fatalf("rebuilt != original\norig: %q\nnew : %q", long, rebuilt)
	}
}

// ─── Test: looksLikeEnvironmentExcuse ────────────────────────────

func TestLooksLikeEnvironmentExcuse(t *testing.T) {
	// 实测原文:模型拿到 ls 文件列表后声称环境找不到项目目录,让用户重连
	trueCases := []string{
		"当前可用的源码读取环境中没有找到对应的 ai-roundtable 项目目录，请重新连接/打开该项目环境后，我会直接读取以下关键文件并输出分析。",
		"项目目录不可用，需要你重新打开环境。",
		"找不到该项目，请挂载项目目录后再试。",
		"I could not locate the project directory. Please reconnect the environment.",
		"环境未挂载，无法读取源码。",
	}
	falseCases := []string{
		"",
		"已读取源码，总结如下。",
		"read_file 读取成功：main.go 内容如下。",
		"The command output shows the file listing.",
		"正在读取 manifest.json。",
	}
	for _, s := range trueCases {
		if !looksLikeEnvironmentExcuse(s) {
			t.Errorf("want env-excuse detection for %q", s)
		}
	}
	for _, s := range falseCases {
		if looksLikeEnvironmentExcuse(s) {
			t.Errorf("did not want env-excuse detection for %q", s)
		}
	}
}

// ─── Test: looksLikeSandboxRefusal 新沙箱幻觉变体 ────────────────

func TestLooksLikeSandboxRefusalCaasToolbox(t *testing.T) {
	// 实测:模型编造 '/caas_toolbox' 沙箱路径,声称"当前工作目录:/"
	trueCases := []string{
		"已检查：当前工作目录：`/`，搜索 manifest.json：只发现 `/caas_toolbox/manifest.json`",
		"当前实际挂载的文件系统中没有找到 ai-roundtable 项目目录。",
		"I found /caas_toolbox/manifest.json but no project files.",
	}
	falseCases := []string{
		"",
		"已读取 manifest.json，内容如下。",
		"pwd 显示当前目录是项目根。",
	}
	for _, s := range trueCases {
		if !looksLikeSandboxRefusal(s) {
			t.Errorf("want sandbox-refusal detection for %q", s)
		}
	}
	for _, s := range falseCases {
		if looksLikeSandboxRefusal(s) {
			t.Errorf("did not want sandbox-refusal detection for %q", s)
		}
	}
}

// ─── Test: isValidToolReply ──────────────────────────────────────

func TestIsValidToolReply(t *testing.T) {
	valid := []string{
		"已通读全部源码，总结如下：项目为 MV3 扩展。",
		"read_file 成功：main.go 内容为 package main。",
		"The tool output shows the complete summary.",
	}
	invalid := []string{
		"",
		"   ",
		"我继续读取源码后整理。",
		"请把源码内容提供给我。",
		"请重新连接/打开项目环境。",
		"当前工作目录：`/`，只发现 /caas_toolbox/manifest.json",
		"我无法访问你机器上的文件系统。",
	}
	for _, s := range valid {
		if !isValidToolReply(s) {
			t.Errorf("want valid for %q", s)
		}
	}
	for _, s := range invalid {
		if isValidToolReply(s) {
			t.Errorf("did not want valid for %q", s)
		}
	}
}
