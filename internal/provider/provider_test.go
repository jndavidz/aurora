package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"aurora/internal/config"
	"aurora/typings/official"
)

func newTestConfig() *config.Config {
	return &config.Config{
		DeepSeekWebTokens: "tokens/deepseek_tokens.txt",
		DeepSeekModels:    nil, // 用默认目录
	}
}

func TestParseDeepSeekModel(t *testing.T) {
	cases := []struct {
		id      string
		variant string
		mode    string
		caps    []Capability
	}{
		{"deepseek-v4-flash-chat", variantChat, modeQuick, []Capability{CapWebSearch, CapReasoning, CapVision}},
		{"deepseek-v4-pro-chat", variantChat, modeExpert, []Capability{CapReasoning}},
		{"deepseek-v4-flash-coding", variantCoding, "", []Capability{CapFunctionCall, CapReasoning}},
		{"deepseek-v4-pro-coding", variantCoding, "", []Capability{CapFunctionCall, CapReasoning}},
		{"gpt-5", "", "", nil},           // 非 deepseek 命名 → nil
		{"deepseek-v4-chat", variantChat, modeExpert, []Capability{CapReasoning}}, // 无 flash/pro → 默认 expert
	}
	for _, c := range cases {
		m := parseDeepSeekModel(c.id)
		if c.variant == "" && c.caps == nil {
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
		if c.mode != "" && m.Mode != c.mode {
			t.Errorf("%s: mode = %s, want %s", c.id, m.Mode, c.mode)
		}
		if len(m.Caps) != len(c.caps) {
			t.Errorf("%s: caps = %v, want %v", c.id, m.Caps, c.caps)
		}
	}
}

func TestRegistryResolve(t *testing.T) {
	d := NewDeepSeek(newTestConfig())
	r := NewRegistry()
	r.Register(d)

	if p := r.Resolve("deepseek-v4-flash-chat"); p == nil {
		t.Fatal("Resolve(deepseek-v4-flash-chat) = nil, want DeepSeek")
	}
	if p := r.Resolve("auto"); p != nil {
		t.Fatalf("Resolve(auto) = %v, want nil (default ChatGPT)", p.Name())
	}
	if p := r.Resolve("gpt-5-6"); p != nil {
		t.Fatalf("Resolve(gpt-5-6) = %v, want nil", p.Name())
	}
	if len(r.Models()) != 4 {
		t.Errorf("Models() = %d entries, want 4", len(r.Models()))
	}
}

// chat 变体硬规则:即使客户端带 tools,拍平的 prompt 也绝不能含任何工具信息。
func TestFlattenChatInputNoToolInjection(t *testing.T) {
	req := &official.ResponsesAPIRequest{
		Model: "deepseek-v4-flash-chat",
		Input: json.RawMessage(`[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"列出文件"}]}
		]`),
		Instructions: json.RawMessage(`"你是助手"`),
		// 客户端带了 tools —— chat 变体必须剥离
		Tools: []official.Tool{{Type: "function", Function: official.ToolFunction{Name: "bash"}}},
	}
	req.ToolChoice = &official.ToolChoice{Type: "auto"}

	prompt := flattenChatInput(req, true)
	for _, banned := range []string{"bash", "tool_call", "TOOLS", "tool_calls", "arguments"} {
		if strings.Contains(prompt, banned) {
			t.Errorf("chat prompt must not contain tool info %q:\n%s", banned, prompt)
		}
	}
	if !strings.Contains(prompt, "你是助手") || !strings.Contains(prompt, "列出文件") {
		t.Errorf("chat prompt lost conversation content:\n%s", prompt)
	}
}

// chat 变体:function_call/function_call_output item 防御性跳过,不上游注入。
func TestFlattenChatInputSkipsToolItems(t *testing.T) {
	req := &official.ResponsesAPIRequest{
		Model: "deepseek-v4-flash-chat",
		Input: json.RawMessage(`[
			{"type":"message","role":"user","content":"读一下"},
			{"type":"function_call","call_id":"c1","name":"read","arguments":"{\"path\":\"a.txt\"}"},
			{"type":"function_call_output","call_id":"c1","output":"文件内容"},
			{"type":"message","role":"user","content":"总结"}
		]`),
	}
	prompt := flattenChatInput(req, true)
	if strings.Contains(prompt, "read") || strings.Contains(prompt, "文件内容") {
		t.Errorf("chat prompt must skip tool items:\n%s", prompt)
	}
	if !strings.Contains(prompt, "读一下") || !strings.Contains(prompt, "总结") {
		t.Errorf("chat prompt lost messages:\n%s", prompt)
	}
}

// 多轮 input 重放:function_call/function_call_output 保留(供 coding 变体回放)。
func TestResponsesInputItemsMultiTurn(t *testing.T) {
	req := &official.ResponsesAPIRequest{
		Input: json.RawMessage(`[
			{"type":"message","role":"user","content":"读文件"},
			{"type":"function_call","call_id":"c1","name":"read","arguments":"{\"path\":\"a.txt\"}"},
			{"type":"function_call_output","call_id":"c1","output":"内容A"},
			{"type":"message","role":"user","content":"继续"}
		]`),
	}
	items := responsesInputItems(req.Input)
	if len(items) != 4 {
		t.Fatalf("items = %d, want 4", len(items))
	}
	if items[1].Type != "function_call" || items[1].Text != `{"path":"a.txt"}` {
		t.Errorf("function_call item wrong: %+v", items[1])
	}
	if items[2].Type != "function_call_output" || items[2].Text != "内容A" {
		t.Errorf("function_call_output item wrong: %+v", items[2])
	}
}
