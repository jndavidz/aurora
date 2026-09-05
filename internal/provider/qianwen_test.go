package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"aurora/internal/config"
	"aurora/typings/official"
)

// 模型 id 前缀保护:只认 Qwen 系,防误吃其它 provider 的模型。
func TestIsQianwenModel(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{"qwen-3.8-max", true},
		{"Qwen3.7", true},
		{"qwen3.6-Flash", true}, // 大小写不敏感
		{"gpt-5-chat", false},
		{"glm-flash", false},
		{"", false},
		{"Qwen", true},
	}
	for _, c := range cases {
		if got := isQianwenModel(c.id); got != c.want {
			t.Errorf("isQianwenModel(%q) = %v, want %v", c.id, got, c.want)
		}
	}
}

// 默认目录 + 自定义 QIANWEN_MODELS 配置。
func TestNewQianwenDefaultModels(t *testing.T) {
	d := NewQianwen(&config.Config{QianwenWebTokens: "tokens/qianwen_tokens.txt"})
	if len(d.Models()) != len(defaultQianwenModels) {
		t.Fatalf("Models() = %d, want %d", len(d.Models()), len(defaultQianwenModels))
	}
	if !d.Handles("qwen-3.8-max") {
		t.Fatal("default model not handled")
	}
	if d.Handles("Qwen9.9-Max") {
		t.Fatal("unknown model should not be handled")
	}

	// 自定义模型目录(非 Qwen 前缀被跳过)
	d2 := NewQianwen(&config.Config{QianwenWebTokens: "t", QianwenModels: []string{"qwen-3.8-max", "gpt-5-chat", "Qwen3.7"}})
	if len(d2.Models()) != 2 {
		t.Fatalf("custom Models() = %d, want 2 (gpt-5-chat skipped)", len(d2.Models()))
	}
	if d2.Handles("gpt-5-chat") {
		t.Fatal("gpt-5-chat must not be handled by qianwen")
	}
}

// chat 硬规则:即使客户端带 tools,千问 messages 也绝不能含工具信息。
func TestQianwenMessagesFromResponsesNoToolInjection(t *testing.T) {
	req := &official.ResponsesAPIRequest{
		Model: "qwen-3.8-max",
		Input: json.RawMessage(`[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"列出文件"}]}
		]`),
		Instructions: json.RawMessage(`"你是助手"`),
		Tools:        []official.Tool{{Type: "function", Function: official.ToolFunction{Name: "bash"}}},
	}
	msgs := qianwenMessagesFromResponses(req)
	for _, m := range msgs {
		for _, banned := range []string{"bash", "tool_call", "TOOLS", "arguments"} {
			if strings.Contains(m.Content, banned) {
				t.Errorf("chat messages must not contain tool info %q: %q", banned, m.Content)
			}
		}
	}
	if len(msgs) != 2 { // instructions + user
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	// instructions 归入 user 消息,且带 ori_query
	if msgs[0].MetaData == nil || msgs[0].MetaData.OriQuery != msgs[0].Content {
		t.Errorf("instructions should be a user msg with ori_query: %+v", msgs[0])
	}
}

// chat 变体:function_call/function_call_output item 跳过。
func TestQianwenMessagesFromResponsesSkipsToolItems(t *testing.T) {
	req := &official.ResponsesAPIRequest{
		Model: "qwen-3.8-max",
		Input: json.RawMessage(`[
			{"type":"message","role":"user","content":"读一下"},
			{"type":"function_call","call_id":"c1","name":"read","arguments":"{\"path\":\"a.txt\"}"},
			{"type":"function_call_output","call_id":"c1","output":"文件内容"},
			{"type":"message","role":"user","content":"总结"}
		]`),
	}
	msgs := qianwenMessagesFromResponses(req)
	var all string
	for _, m := range msgs {
		all += m.Content
	}
	if strings.Contains(all, "read") || strings.Contains(all, "文件内容") {
		t.Errorf("chat messages must skip tool items: %q", all)
	}
	if !strings.Contains(all, "读一下") || !strings.Contains(all, "总结") {
		t.Errorf("chat messages lost conversation: %q", all)
	}
}

// assistant 消息不带 ori_query;user 消息带。
func TestQianwenMessagesRoleMeta(t *testing.T) {
	req := &official.ResponsesAPIRequest{
		Model: "qwen-3.8-max",
		Input: json.RawMessage(`[
			{"type":"message","role":"user","content":"你好"},
			{"type":"message","role":"assistant","content":"你好,我是千问"},
			{"type":"message","role":"user","content":"你是谁"}
		]`),
	}
	msgs := qianwenMessagesFromResponses(req)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3", len(msgs))
	}
	if msgs[0].MetaData == nil || msgs[0].MetaData.OriQuery != "你好" {
		t.Errorf("user msg[0] should carry ori_query: %+v", msgs[0])
	}
	if msgs[1].MetaData == nil || msgs[1].MetaData.OriQuery != "" {
		t.Errorf("assistant msg[1] should NOT carry ori_query: %+v", msgs[1])
	}
	if msgs[2].MetaData == nil || msgs[2].MetaData.OriQuery != "你是谁" {
		t.Errorf("user msg[2] should carry ori_query: %+v", msgs[2])
	}
}

// qianwenMessagesFromAPI:chat.completions 消息转换(system→user,tool 跳过)。
func TestQianwenMessagesFromAPI(t *testing.T) {
	req := &official.APIRequest{
		Messages: []official.APIMessage{
			{Role: "system", Content: official.MessageContent{TextValue: "你是助手"}},
			{Role: "user", Content: official.MessageContent{TextValue: "你好"}},
			{Role: "assistant", Content: official.MessageContent{TextValue: "你好,我是千问"}},
			{Role: "tool", ToolCallID: "c1", Content: official.MessageContent{TextValue: "结果"}},
		},
	}
	msgs := qianwenMessagesFromAPI(req)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3 (tool msg skipped)", len(msgs))
	}
	if msgs[0].MetaData == nil || msgs[0].MetaData.OriQuery != "你是助手" {
		t.Errorf("system→user msg[0] should carry ori_query: %+v", msgs[0])
	}
	if msgs[1].MetaData == nil || msgs[1].MetaData.OriQuery != "你好" {
		t.Errorf("user msg[1] should carry ori_query: %+v", msgs[1])
	}
	if msgs[2].MetaData == nil || msgs[2].MetaData.OriQuery != "" {
		t.Errorf("assistant msg[2] should NOT carry ori_query: %+v", msgs[2])
	}
}
