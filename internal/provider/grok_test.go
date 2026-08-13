package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"aurora/internal/config"
	"aurora/typings/official"
)

func TestParseGrokModel(t *testing.T) {
	cases := []struct {
		id      string
		variant string
		wantNil bool
	}{
		{"grok-3-chat", grokVariantChat, false},
		{"grok-3-coding", grokVariantCoding, false},
		{"grok-3", "", true},      // 无变体后缀 → nil
		{"gpt-5-chat", "", true},  // 非 grok 命名 → nil
		{"grok-4-coding", grokVariantCoding, false},
	}
	for _, c := range cases {
		m := parseGrokModel(c.id)
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

// 默认目录 + 自定义 GROK_MODELS 配置。
func TestNewGrokDefaultModels(t *testing.T) {
	d := NewGrok(&config.Config{GrokCookies: "cookies/grok_cookies.txt"})
	if len(d.Models()) != len(defaultGrokModels) {
		t.Fatalf("Models() = %d, want %d", len(d.Models()), len(defaultGrokModels))
	}
	if !d.Handles("grok-3-chat") || !d.Handles("grok-3-coding") {
		t.Fatal("default models not handled")
	}
	if d.Handles("grok-9-chat") {
		t.Fatal("unknown model should not be handled")
	}

	// 自定义模型目录
	d2 := NewGrok(&config.Config{GrokCookies: "c", GrokModels: []string{"grok-3-chat", "bogus"}})
	if len(d2.Models()) != 1 {
		t.Fatalf("custom Models() = %d, want 1 (bogus skipped)", len(d2.Models()))
	}
}

// coding prompt:工具指令注入 + function_call 回放。
func TestGrokCodingPromptFromResponses(t *testing.T) {
	req := &official.ResponsesAPIRequest{
		Model: "grok-3-coding",
		Input: json.RawMessage(`[
			{"type":"message","role":"user","content":"读文件"},
			{"type":"function_call","call_id":"c1","name":"read","arguments":"{\"path\":\"a.txt\"}"},
			{"type":"function_call_output","call_id":"c1","output":"内容A"},
			{"type":"message","role":"user","content":"继续"}
		]`),
		Tools: []official.Tool{{Type: "function", Function: official.ToolFunction{Name: "read"}}},
	}
	prompt := grokCodingPromptFromResponses(req)
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

// chat 变体硬规则:即使客户端带 tools,prompt 也不含工具信息。
func TestGrokChatNoToolInjection(t *testing.T) {
	req := &official.ResponsesAPIRequest{
		Model: "grok-3-chat",
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

// splitGrokThinking:剥离 "Thinking about your request" 前缀。
func TestSplitGrokThinking(t *testing.T) {
	body, reasoning := splitGrokThinking("Thinking about your request让我想想。最终答案")
	if reasoning != "" {
		t.Errorf("reasoning = %q, want empty (无法判定思考边界)", reasoning)
	}
	if body != "让我想想。最终答案" {
		t.Errorf("body = %q, want 让我想想。最终答案", body)
	}
	// 无前缀
	body2, r2 := splitGrokThinking("普通回答")
	if body2 != "普通回答" || r2 != "" {
		t.Errorf("no-prefix case: body=%q reasoning=%q", body2, r2)
	}
	// 前缀前有文本 → 视为推理
	body3, r3 := splitGrokThinking("思考中Thinking about your request正文")
	if r3 != "思考中" || body3 != "正文" {
		t.Errorf("prefix-with-pre case: body=%q reasoning=%q", body3, r3)
	}
}
