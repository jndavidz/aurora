package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"aurora/internal/config"
	"aurora/internal/yuanbaoweb"
	"aurora/typings/official"
)

// 模型解析:-chat/-coding 后缀 + hy3-/yb-deepseek- 前缀保护。
func TestParseYuanbaoModel(t *testing.T) {
	cases := []struct {
		id          string
		wantVariant string
		wantChatMID string
		wantCaps    bool
	}{
		{"hy3-chat", variantChat, yuanbaoweb.ModelHy3, true},
		{"hy3-coding", variantCoding, yuanbaoweb.ModelHy3, true},
		{"yb-deepseek-chat", variantChat, yuanbaoweb.ModelDeepSeek, true},
		{"yb-deepseek-coding", variantCoding, yuanbaoweb.ModelDeepSeek, true},
		{"gpt-5-chat", "", "", false},     // 无前缀保护,不认
		{"glm-5.2-chat", "", "", false},   // 不认其它 provider 模型
		{"deepseek-chat", "", "", false},  // 不认裸 deepseek(现有 DeepSeek provider 的域)
		{"hy3", "", "", false},            // 无后缀
		{"yb-deepseek", "", "", false},    // 无后缀
		{"", "", "", false},
	}
	for _, c := range cases {
		m := parseYuanbaoModel(c.id)
		if !c.wantCaps {
			if m != nil {
				t.Errorf("parseYuanbaoModel(%q) = %+v, want nil", c.id, m)
			}
			continue
		}
		if m == nil {
			t.Errorf("parseYuanbaoModel(%q) = nil, want variant %q", c.id, c.wantVariant)
			continue
		}
		if m.Variant != c.wantVariant || m.ChatModelID != c.wantChatMID {
			t.Errorf("parseYuanbaoModel(%q) = %+v, want variant %q chatModelId %q", c.id, m, c.wantVariant, c.wantChatMID)
		}
	}
}

// 默认目录 + 自定义 YUANBAO_MODELS。
func TestNewYuanbaoDefaultModels(t *testing.T) {
	d := NewYuanbao(&config.Config{YuanbaoWebTokens: "tokens/yuanbao_tokens.txt"})
	if len(d.Models()) != len(defaultYuanbaoModels) {
		t.Fatalf("Models() = %d, want %d", len(d.Models()), len(defaultYuanbaoModels))
	}
	for _, id := range defaultYuanbaoModels {
		if !d.Handles(id) {
			t.Errorf("default model %q not handled", id)
		}
	}
	if d.Handles("hy3-pro-chat") {
		t.Fatal("unknown model should not be handled")
	}
	if d.Handles("deepseek-chat") {
		t.Fatal("deepseek-chat must not be handled by yuanbao (belongs to deepseek provider)")
	}

	// 自定义目录(非 hy3-/yb-deepseek- 前缀被跳过)
	d2 := NewYuanbao(&config.Config{YuanbaoWebTokens: "t", YuanbaoModels: []string{"hy3-chat", "yb-deepseek-coding", "gpt-5-chat", "deepseek-chat"}})
	if len(d2.Models()) != 2 {
		t.Fatalf("custom Models() = %d, want 2 (unrecognized skipped)", len(d2.Models()))
	}
	if d2.Handles("gpt-5-chat") || d2.Handles("deepseek-chat") {
		t.Fatal("unrecognized ids must be skipped")
	}
}

// chat 硬规则:即使客户端带 tools,chat prompt 也绝不能含工具信息。
func TestYuanbaoChatPromptNoToolInjection(t *testing.T) {
	req := &official.ResponsesAPIRequest{
		Model:        "hy3-chat",
		Instructions: json.RawMessage(`"你是助手"`),
		Input: json.RawMessage(`[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"列出文件"}]}
		]`),
		Tools: []official.Tool{{Type: "function", Function: official.ToolFunction{Name: "bash"}}},
	}
	prompt := flattenChatItems(responsesInputItems(req.Input), rawResponsesText(req.Instructions))
	for _, banned := range []string{"bash", "tool_call", "TOOLS", "arguments", "<tool_call>"} {
		if strings.Contains(prompt, banned) {
			t.Errorf("chat prompt must not contain tool info %q: %q", banned, prompt)
		}
	}
	if !strings.Contains(prompt, "列出文件") || !strings.Contains(prompt, "你是助手") {
		t.Errorf("chat prompt lost conversation: %q", prompt)
	}
}

// chat 变体:function_call/function_call_output item 跳过。
func TestYuanbaoChatPromptSkipsToolItems(t *testing.T) {
	req := &official.ResponsesAPIRequest{
		Model: "hy3-chat",
		Input: json.RawMessage(`[
			{"type":"message","role":"user","content":"读一下"},
			{"type":"function_call","call_id":"c1","name":"read","arguments":"{\"path\":\"a.txt\"}"},
			{"type":"function_call_output","call_id":"c1","output":"文件内容"},
			{"type":"message","role":"user","content":"总结"}
		]`),
	}
	prompt := flattenChatItems(responsesInputItems(req.Input), "")
	if strings.Contains(prompt, "read") || strings.Contains(prompt, "文件内容") {
		t.Errorf("chat prompt must skip tool items: %q", prompt)
	}
	if !strings.Contains(prompt, "读一下") || !strings.Contains(prompt, "总结") {
		t.Errorf("chat prompt lost conversation: %q", prompt)
	}
}

// coding prompt:注入工具指令 + 标签回放。
func TestYuanbaoCodingPromptInjectsTools(t *testing.T) {
	req := &official.ResponsesAPIRequest{
		Model: "hy3-coding",
		Input: json.RawMessage(`[
			{"type":"message","role":"user","content":"用 bash 列出文件"}
		]`),
		Tools: []official.Tool{{Type: "function", Function: official.ToolFunction{Name: "bash", Description: "run shell"}}},
	}
	prompt := yuanbaoCodingPrompt(req)
	if !strings.Contains(prompt, "bash") {
		t.Errorf("coding prompt must inject tool names: %q", prompt)
	}
	if !strings.Contains(prompt, "<tool_call>") {
		t.Errorf("coding prompt must instruct tag format: %q", prompt)
	}
}
