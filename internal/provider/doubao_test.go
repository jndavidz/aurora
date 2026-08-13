package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"aurora/internal/config"
	"aurora/typings/official"
)

func TestParseDoubaoModel(t *testing.T) {
	cases := []struct {
		id      string
		variant string
		wantNil bool
	}{
		{"doubao-chat", doubaoVariantChat, false},
		// coding 变体已注释禁用(豆包只做 chat),下列用例保留作恢复参考:
		// {"doubao-coding", doubaoVariantCoding, false},
		{"doubao", "", true},     // 无变体后缀 → nil
		{"gpt-5-chat", "", true}, // 非 doubao 命名 → nil
		// {"doubao-pro-coding", doubaoVariantCoding, false},
	}
	for _, c := range cases {
		m := parseDoubaoModel(c.id)
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
func TestNewDoubaoDefaultModels(t *testing.T) {
	d := NewDoubao(&config.Config{DoubaoAccounts: "accounts/doubao.json"})
	if len(d.Models()) != len(defaultDoubaoModels) {
		t.Fatalf("Models() = %d, want %d", len(d.Models()), len(defaultDoubaoModels))
	}
	// coding 变体已注释禁用,`doubao-coding` 不再注册(保留作恢复参考):
	// if !d.Handles("doubao-chat") || !d.Handles("doubao-coding") {
	if !d.Handles("doubao-chat") {
		t.Fatal("default models not handled")
	}
	d2 := NewDoubao(&config.Config{DoubaoAccounts: "a", DoubaoModels: []string{"doubao-chat", "bogus"}})
	if len(d2.Models()) != 1 {
		t.Fatalf("custom Models() = %d, want 1", len(d2.Models()))
	}
}

// chat 变体:剥离工具。
func TestDoubaoMessagesNoToolInjection(t *testing.T) {
	req := &official.ResponsesAPIRequest{
		Model: "doubao-chat",
		Input: json.RawMessage(`[
			{"type":"message","role":"user","content":"读文件"},
			{"type":"function_call","call_id":"c1","name":"read","arguments":"{\"path\":\"a.txt\"}"},
			{"type":"message","role":"user","content":"继续"}
		]`),
		Tools: []official.Tool{{Type: "function", Function: official.ToolFunction{Name: "read"}}},
	}
	msgs := doubaoMessagesFromResponses(req, true)
	var all string
	for _, m := range msgs {
		all += m.Content
	}
	if strings.Contains(all, "read") {
		t.Errorf("chat messages 不应含工具信息: %q", all)
	}
	if !strings.Contains(all, "读文件") || !strings.Contains(all, "继续") {
		t.Errorf("chat messages 丢失对话: %q", all)
	}
}

// coding 变体已注释禁用,测试保留作恢复参考(启用需同时恢复 doubao.go
// 与 doubao_coding.go 的 coding 部分):
// // coding prompt:工具指令 + 回放。
// func TestDoubaoCodingPrompt(t *testing.T) {
// 	req := &official.ResponsesAPIRequest{
// 		Model: "doubao-coding",
// 		Input: json.RawMessage(`[
// 			{"type":"message","role":"user","content":"读文件"},
// 			{"type":"function_call","call_id":"c1","name":"read","arguments":"{\"path\":\"a.txt\"}"},
// 			{"type":"function_call_output","call_id":"c1","output":"内容A"}
// 		]`),
// 		Tools: []official.Tool{{Type: "function", Function: official.ToolFunction{Name: "read"}}},
// 	}
// 	prompt := doubaoCodingPromptFromResponses(req)
// 	if !strings.Contains(prompt, "TOOLS AVAILABLE") {
// 		t.Errorf("coding prompt 应含工具指令: %q", prompt)
// 	}
// 	if !strings.Contains(prompt, "<tool_call>") || !strings.Contains(prompt, `{"path":"a.txt"}`) {
// 		t.Errorf("coding prompt 应回放工具调用: %q", prompt)
// 	}
// 	if !strings.Contains(prompt, "Tool result: 内容A") {
// 		t.Errorf("coding prompt 应回放工具结果: %q", prompt)
// 	}
// }

// lastUserText:取最后一条用户消息。
func TestLastUserText(t *testing.T) {
	req := &official.ResponsesAPIRequest{
		Input: json.RawMessage(`[
			{"type":"message","role":"user","content":"第一问"},
			{"type":"message","role":"assistant","content":"回答"},
			{"type":"message","role":"user","content":"第二问"}
		]`),
	}
	if got := lastUserText(req); got != "第二问" {
		t.Errorf("lastUserText = %q, want 第二问", got)
	}
}
