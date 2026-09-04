package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"aurora/internal/config"
	"aurora/typings/official"
)

func TestParseGlmModel(t *testing.T) {
	cases := []struct {
		id      string
		variant string
		mode    string
		wantNil bool
	}{
		{"glm-flash", glmVariantChat, "speed", false},
		{"glm-5.2-coding", glmVariantCoding, "", false},
		// {"glm-5-coding", ...}: 2026-09-04 白名单放宽为 glm- 前缀,coding 变体可解析
		{"glm-5.2", "", "", true},      // 无变体后缀 → nil
		{"gpt-5-chat", "", "", true},   // 非 glm 命名 → nil
		{"glm-5.2-coding-x", "", "", true},
	}
	for _, c := range cases {
		m := parseGlmModel(c.id)
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
		if m.Mode != c.mode {
			t.Errorf("%s: mode = %q, want %q", c.id, m.Mode, c.mode)
		}
	}
}

// 默认目录 + 自定义 GLM_MODELS 配置。
func TestNewGlmDefaultModels(t *testing.T) {
	d := NewGlm(&config.Config{GlmWebTokens: "tokens/glm_tokens.txt"})
	if len(d.Models()) != len(defaultGlmModels) {
		t.Fatalf("Models() = %d, want %d", len(d.Models()), len(defaultGlmModels))
	}
	if !d.Handles("glm-flash") || !d.Handles("glm-5.2-coding") {
		t.Fatal("default models not handled")
	}
	if d.Handles("glm-9-chat") {
		t.Fatal("unknown model should not be handled")
	}

	// 自定义模型目录
	d2 := NewGlm(&config.Config{GlmWebTokens: "t", GlmModels: []string{"glm-flash", "bogus"}})
	if len(d2.Models()) != 1 {
		t.Fatalf("custom Models() = %d, want 1 (bogus skipped)", len(d2.Models()))
	}
}

// chat 变体硬规则:即使客户端带 tools,glm messages 也绝不能含工具信息。
func TestGlmMessagesFromResponsesNoToolInjection(t *testing.T) {
	req := &official.ResponsesAPIRequest{
		Model: "glm-flash",
		Input: json.RawMessage(`[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"列出文件"}]}
		]`),
		Instructions: json.RawMessage(`"你是助手"`),
		Tools:        []official.Tool{{Type: "function", Function: official.ToolFunction{Name: "bash"}}},
	}
	msgs := glmMessagesFromResponses(req, true)
	for _, m := range msgs {
		for _, c := range m.Content {
			for _, banned := range []string{"bash", "tool_call", "TOOLS", "arguments"} {
				if strings.Contains(c.Text, banned) {
					t.Errorf("chat messages must not contain tool info %q: %q", banned, c.Text)
				}
			}
		}
	}
	if len(msgs) != 2 { // instructions + user
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	// instructions 归入 user role(智谱无 system)
	if msgs[0].Role != "user" {
		t.Errorf("instructions role = %q, want user", msgs[0].Role)
	}
}

// chat 变体:function_call/function_call_output item 跳过。
func TestGlmMessagesFromResponsesSkipsToolItems(t *testing.T) {
	req := &official.ResponsesAPIRequest{
		Model: "glm-flash",
		Input: json.RawMessage(`[
			{"type":"message","role":"user","content":"读一下"},
			{"type":"function_call","call_id":"c1","name":"read","arguments":"{\"path\":\"a.txt\"}"},
			{"type":"function_call_output","call_id":"c1","output":"文件内容"},
			{"type":"message","role":"user","content":"总结"}
		]`),
	}
	msgs := glmMessagesFromResponses(req, true)
	var all string
	for _, m := range msgs {
		for _, c := range m.Content {
			all += c.Text
		}
	}
	if strings.Contains(all, "read") || strings.Contains(all, "文件内容") {
		t.Errorf("chat messages must skip tool items: %q", all)
	}
	if !strings.Contains(all, "读一下") || !strings.Contains(all, "总结") {
		t.Errorf("chat messages lost conversation: %q", all)
	}
}

// coding 变体:工具指令注入 + function_call/function_call_output 回放。
func TestGlmCodingMessagesFromResponses(t *testing.T) {
	req := &official.ResponsesAPIRequest{
		Model: "glm-5.2-coding",
		Input: json.RawMessage(`[
			{"type":"message","role":"user","content":"读文件"},
			{"type":"function_call","call_id":"c1","name":"read","arguments":"{\"path\":\"a.txt\"}"},
			{"type":"function_call_output","call_id":"c1","output":"内容A"},
			{"type":"message","role":"user","content":"继续"}
		]`),
		Tools: []official.Tool{{Type: "function", Function: official.ToolFunction{Name: "read"}}},
	}
	msgs := glmCodingMessagesFromResponses(req)
	if len(msgs) < 3 {
		t.Fatalf("messages = %d, want >= 3 (inst + history)", len(msgs))
	}
	if !strings.Contains(msgs[0].Content[0].Text, "```json") {
		t.Errorf("first message should contain tool instructions, got: %q", msgs[0].Content[0].Text)
	}
	// function_call → assistant ```json 回放
	foundReplay := false
	for _, m := range msgs {
		if m.Role == "assistant" && strings.Contains(m.Content[0].Text, "```json") {
			foundReplay = true
		}
	}
	if !foundReplay {
		t.Error("coding messages must replay function_call as assistant ```json block")
	}
	// function_call_output → user Tool result
	foundResult := false
	for _, m := range msgs {
		if m.Role == "user" && strings.Contains(m.Content[0].Text, "Tool result: 内容A") {
			foundResult = true
		}
	}
	if !foundResult {
		t.Error("coding messages must replay function_call_output as user Tool result")
	}
}

// glmMessagesFromAPI:chat.completions 消息转换。
func TestGlmMessagesFromAPI(t *testing.T) {
	req := &official.APIRequest{
		Messages: []official.APIMessage{
			{Role: "system", Content: official.MessageContent{TextValue: "你是助手"}},
			{Role: "user", Content: official.MessageContent{TextValue: "你好"}},
		},
	}
	msgs := glmMessagesFromAPI(req)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	if msgs[0].Role != "user" {
		t.Errorf("system role = %q, want user", msgs[0].Role)
	}
	if msgs[1].Role != "user" || msgs[1].Content[0].Text != "你好" {
		t.Errorf("user message wrong: %+v", msgs[1])
	}
}

// glmCodingMessagesFromAPI:工具消息转换(tool_calls / tool role)。
func TestGlmCodingMessagesFromAPI(t *testing.T) {
	req := &official.APIRequest{
		Messages: []official.APIMessage{
			{Role: "user", Content: official.MessageContent{TextValue: "读文件"}},
			{Role: "assistant", ToolCalls: []official.ToolCallRef{{Index: 0, ID: "c1", Type: "function", Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{Name: "read", Arguments: `{"path":"a.txt"}`}}}},
			{Role: "tool", ToolCallID: "c1", Content: official.MessageContent{TextValue: "内容A"}},
		},
	}
	msgs := glmCodingMessagesFromAPI(req)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3", len(msgs))
	}
	if msgs[1].Role != "assistant" || !strings.Contains(msgs[1].Content[0].Text, "```json") || !strings.Contains(msgs[1].Content[0].Text, `{"path":"a.txt"}`) {
		t.Errorf("assistant tool_calls replay wrong: %+v", msgs[1])
	}
	if msgs[2].Role != "user" || !strings.Contains(msgs[2].Content[0].Text, "Tool result: 内容A") {
		t.Errorf("tool role replay wrong: %+v", msgs[2])
	}
}
