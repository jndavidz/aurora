package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"aurora/internal/config"
	"aurora/typings/official"
)

func TestParseKimiModel(t *testing.T) {
	cases := []struct {
		id      string
		variant string
		wantNil bool
	}{
		{"kimi", kimiVariantChat, false},
		{"kimi-coding", kimiVariantCoding, false},
		{"kimi-pro-chat", "", true}, // 非 kimi 家族(改名后无后缀=chat 的旧用例已无意义)
		{"gpt-5-chat", "", true},    // 非 kimi 命名 → nil
		{"kimi-coding-x", "", true},
	}
	for _, c := range cases {
		m := parseKimiModel(c.id)
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

// 默认目录 + 自定义 KIMI_MODELS 配置。
func TestNewKimiDefaultModels(t *testing.T) {
	d := NewKimi(&config.Config{KimiWebTokens: "tokens/kimi_tokens.txt"})
	if len(d.Models()) != len(defaultKimiModels) {
		t.Fatalf("Models() = %d, want %d", len(d.Models()), len(defaultKimiModels))
	}
	if !d.Handles("kimi") || !d.Handles("kimi-coding") {
		t.Fatal("default models not handled")
	}
	if d.Handles("kimi-9-chat") {
		t.Fatal("unknown model should not be handled")
	}

	// 自定义模型目录
	d2 := NewKimi(&config.Config{KimiWebTokens: "t", KimiModels: []string{"kimi", "bogus"}})
	if len(d2.Models()) != 1 {
		t.Fatalf("custom Models() = %d, want 1 (bogus skipped)", len(d2.Models()))
	}
}

// chat 变体硬规则:即使客户端带 tools,kimi 拍平文本也绝不能含工具信息。
func TestKimiFlattenResponsesNoToolInjection(t *testing.T) {
	req := &official.ResponsesAPIRequest{
		Model: "kimi",
		Input: json.RawMessage(`[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"列出文件"}]}
		]`),
		Instructions: json.RawMessage(`"你是助手"`),
		Tools:        []official.Tool{{Type: "function", Function: official.ToolFunction{Name: "bash"}}},
	}
	text := kimiFlattenResponses(req, true)
	for _, banned := range []string{"bash", "tool_call", "TOOLS", "arguments"} {
		if strings.Contains(text, banned) {
			t.Errorf("chat text must not contain tool info %q: %q", banned, text)
		}
	}
	if !strings.HasPrefix(text, "你是助手") {
		t.Errorf("instructions should lead the text: %q", text)
	}
	if !strings.Contains(text, "用户: 列出文件") {
		t.Errorf("conversation lost: %q", text)
	}
}

// chat 变体:function_call/function_call_output item 跳过。
func TestKimiFlattenResponsesSkipsToolItems(t *testing.T) {
	req := &official.ResponsesAPIRequest{
		Model: "kimi",
		Input: json.RawMessage(`[
			{"type":"message","role":"user","content":"读一下"},
			{"type":"function_call","call_id":"c1","name":"read","arguments":"{\"path\":\"a.txt\"}"},
			{"type":"function_call_output","call_id":"c1","output":"文件内容"},
			{"type":"message","role":"user","content":"总结"}
		]`),
	}
	text := kimiFlattenResponses(req, true)
	if strings.Contains(text, "read") || strings.Contains(text, "文件内容") {
		t.Errorf("chat text must skip tool items: %q", text)
	}
	if !strings.Contains(text, "读一下") || !strings.Contains(text, "总结") {
		t.Errorf("chat text lost conversation: %q", text)
	}
}

// coding 变体:工具指令注入 + function_call/function_call_output 回放。
func TestKimiCodingResponsesText(t *testing.T) {
	req := &official.ResponsesAPIRequest{
		Model: "kimi-coding",
		Input: json.RawMessage(`[
			{"type":"message","role":"user","content":"读文件"},
			{"type":"function_call","call_id":"c1","name":"read","arguments":"{\"path\":\"a.txt\"}"},
			{"type":"function_call_output","call_id":"c1","output":"内容A"},
			{"type":"message","role":"user","content":"继续"}
		]`),
		Tools: []official.Tool{{Type: "function", Function: official.ToolFunction{Name: "read"}}},
	}
	text := kimiCodingResponsesText(req)
	if !strings.Contains(text, "read") {
		t.Errorf("coding text must inject tool context: %q", text)
	}
	if !strings.Contains(text, "工具结果: 内容A") {
		t.Errorf("coding text must replay function_call_output: %q", text)
	}
	if !strings.Contains(text, "用户: 继续") {
		t.Errorf("coding text lost last user message: %q", text)
	}
}

// kimiFlattenMessages:chat.completions 消息转换(tool 消息剥离/回放)。
func TestKimiFlattenMessages(t *testing.T) {
	req := &official.APIRequest{
		Messages: []official.APIMessage{
			{Role: "system", Content: official.MessageContent{TextValue: "你是助手"}},
			{Role: "user", Content: official.MessageContent{TextValue: "你好"}},
		},
	}
	text := kimiFlattenMessages(req, true)
	if !strings.Contains(text, "用户: 你好") {
		t.Errorf("flatten lost user message: %q", text)
	}
	if strings.Contains(text, "system") {
		t.Errorf("flatten should map system to user: %q", text)
	}

	// coding 变体:tool_calls / tool role 回放
	req2 := &official.APIRequest{
		Messages: []official.APIMessage{
			{Role: "user", Content: official.MessageContent{TextValue: "读文件"}},
			{Role: "assistant", ToolCalls: []official.ToolCallRef{{Index: 0, ID: "c1", Type: "function", Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{Name: "read", Arguments: `{"path":"a.txt"}`}}}},
			{Role: "tool", ToolCallID: "c1", Content: official.MessageContent{TextValue: "内容A"}},
		},
	}
	text2 := kimiFlattenMessages(req2, false)
	if !strings.Contains(text2, `{"path":"a.txt"}`) {
		t.Errorf("coding flatten must replay tool_calls: %q", text2)
	}
	if !strings.Contains(text2, "工具结果: 内容A") {
		t.Errorf("coding flatten must replay tool result: %q", text2)
	}
}
