package official

import (
	"encoding/json"
	"testing"
)

// Responses input 里的 function_call / function_call_output 应转成
// assistant tool_calls 与 role=tool 消息(多轮工具历史回放)。
func TestResponsesToAPIRequestToolItems(t *testing.T) {
	req := ResponsesAPIRequest{
		Model: "gpt-5",
		Input: json.RawMessage(`[
			{"type":"message","role":"user","content":"读文件"},
			{"type":"function_call","call_id":"c1","name":"read","arguments":"{\"path\":\"a.txt\"}"},
			{"type":"function_call_output","call_id":"c1","output":"内容A"}
		]`),
		Tools: []Tool{{Type: "function", Function: ToolFunction{Name: "bash"}}},
	}
	api, err := req.ToAPIRequest()
	if err != nil {
		t.Fatal(err)
	}
	if len(api.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(api.Messages))
	}
	if !api.Messages[1].HasToolCalls() {
		t.Fatalf("message[1] should have tool_calls")
	}
	if api.Messages[1].ToolCalls[0].ID != "c1" || api.Messages[1].ToolCalls[0].Function.Name != "read" {
		t.Errorf("tool call replay wrong: %+v", api.Messages[1].ToolCalls[0])
	}
	if !api.Messages[2].IsToolResult() || api.Messages[2].ToolCallID != "c1" || api.Messages[2].Text() != "内容A" {
		t.Errorf("tool result message wrong: %+v", api.Messages[2])
	}
	if len(api.Tools) != 1 || api.Tools[0].Function.Name != "bash" {
		t.Errorf("tools not passed through: %+v", api.Tools)
	}
}

// 纯字符串 input。
func TestResponsesToAPIRequestStringInput(t *testing.T) {
	req := ResponsesAPIRequest{
		Model: "gpt-5",
		Input: json.RawMessage(`"你好"`),
	}
	api, err := req.ToAPIRequest()
	if err != nil {
		t.Fatal(err)
	}
	if len(api.Messages) != 1 || api.Messages[0].Role != "user" || api.Messages[0].Text() != "你好" {
		t.Fatalf("string input wrong: %+v", api.Messages)
	}
}
