package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"aurora/internal/config"
	"aurora/typings/official"
)

func TestParseGeminiModel(t *testing.T) {
	cases := []struct {
		id      string
		variant string
		wantNil bool
	}{
		{"gemini-3-flash-chat", geminiVariantChat, false},
		{"gemini-3-flash-coding", geminiVariantCoding, false},
		{"gemini-3-flash", "", true}, // 无变体后缀 → nil
		{"gpt-5-chat", "", true},     // 非 gemini 命名 → nil
		{"gemini-2-pro-coding", geminiVariantCoding, false},
	}
	for _, c := range cases {
		m := parseGeminiModel(c.id)
		if c.wantNil {
			if m != nil {
				t.Errorf("%s: expected nil, got %+v", c.id, m)
			}
			continue
		}
		if m == nil {
			t.Errorf("%s: expected model, got nil", c.id)
			continue
		}
		if m.Variant != c.variant {
			t.Errorf("%s: variant = %s, want %s", c.id, m.Variant, c.variant)
		}
	}
}

// 默认目录 + 自定义配置。
func TestNewGeminiDefaultModels(t *testing.T) {
	d := NewGemini(&config.Config{GeminiAccounts: "accounts/gemini.json"})
	if len(d.Models()) != len(defaultGeminiModels) {
		t.Fatalf("Models() = %d, want %d", len(d.Models()), len(defaultGeminiModels))
	}
	if !d.Handles("gemini-3-flash-chat") || !d.Handles("gemini-3-flash-coding") {
		t.Fatal("default models not handled")
	}
	if d.Handles("gemini-9-chat") {
		t.Fatal("unknown model should not be handled")
	}
	d2 := NewGemini(&config.Config{GeminiAccounts: "a", GeminiModels: []string{"gemini-3-flash-chat", "bogus"}})
	if len(d2.Models()) != 1 {
		t.Fatalf("custom Models() = %d, want 1", len(d2.Models()))
	}
}

// chat 变体硬规则:即使带 tools,prompt 也不含工具信息。
func TestGeminiChatNoToolInjection(t *testing.T) {
	req := &official.ResponsesAPIRequest{
		Model: "gemini-3-flash-chat",
		Input: json.RawMessage(`[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"列出文件"}]}
		]`),
		Instructions: json.RawMessage(`"你是助手"`),
		Tools:        []official.Tool{{Type: "function", Function: official.ToolFunction{Name: "bash"}}},
	}
	prompt := flattenChatInput(req, true)
	for _, banned := range []string{"bash", "tool_call", "TOOLS", "arguments"} {
		if strings.Contains(prompt, banned) {
			t.Errorf("chat prompt must not contain tool info %q: %q", banned, prompt)
		}
	}
	if !strings.Contains(prompt, "你是助手") || !strings.Contains(prompt, "列出文件") {
		t.Errorf("chat prompt lost conversation: %q", prompt)
	}
}

// coding prompt:工具指令注入 + function_call 回放。
func TestGeminiCodingPromptFromResponses(t *testing.T) {
	req := &official.ResponsesAPIRequest{
		Model: "gemini-3-flash-coding",
		Input: json.RawMessage(`[
			{"type":"message","role":"user","content":"读文件"},
			{"type":"function_call","call_id":"c1","name":"read","arguments":"{\"path\":\"a.txt\"}"},
			{"type":"function_call_output","call_id":"c1","output":"内容A"}
		]`),
		Tools: []official.Tool{{Type: "function", Function: official.ToolFunction{Name: "read"}}},
	}
	prompt := geminiCodingPromptFromResponses(req, "")
	if !strings.Contains(prompt, "TOOLS AVAILABLE") {
		t.Errorf("prompt should contain tool instructions: %q", prompt)
	}
	if !strings.Contains(prompt, "```json") || !strings.Contains(prompt, `{"path":"a.txt"}`) {
		t.Errorf("prompt should replay function_call as fence JSON: %q", prompt)
	}
	if !strings.Contains(prompt, "Tool result: 内容A") {
		t.Errorf("prompt should replay tool result: %q", prompt)
	}
}
