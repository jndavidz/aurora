package toolcall

import (
	"testing"

	"aurora/typings/official"
)

// ZCode 实测失败场景:模型输出 <|tool_call_begin|>{"file_path":"..."}<|tool_call_end|>
// (无 name 字段),应按参数键推断为 read 工具。
func TestInferToolFromParams(t *testing.T) {
	tools := []official.Tool{
		{Type: "function", Function: official.ToolFunction{Name: "read", Description: "Read file", Parameters: jsonRaw(`{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}`)}},
		{Type: "function", Function: official.ToolFunction{Name: "bash", Description: "Run command", Parameters: jsonRaw(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`)}},
	}
	// {"file_path": "..."} → read
	tc := InferToolFromParams(map[string]any{"file_path": "D:/repos/ai-roundtable/content/base.js"}, tools)
	if tc == nil || tc.Function.Name != "read" {
		t.Fatalf("应推断为 read, got %+v", tc)
	}
	if tc.Function.Arguments == "" {
		t.Error("arguments 不应为空")
	}
	// {"command": "ls"} → bash
	tc2 := InferToolFromParams(map[string]any{"command": "ls"}, tools)
	if tc2 == nil || tc2.Function.Name != "bash" {
		t.Fatalf("应推断为 bash, got %+v", tc2)
	}
	// 键不匹配任何工具 → nil
	if tc3 := InferToolFromParams(map[string]any{"foo": "bar"}, tools); tc3 != nil {
		t.Fatalf("不匹配应返回 nil, got %+v", tc3)
	}
	// 多工具匹配(键冲突)→ nil(不猜测)
	tools2 := []official.Tool{
		{Type: "function", Function: official.ToolFunction{Name: "a", Parameters: jsonRaw(`{"type":"object","properties":{"path":{"type":"string"}}}`)}},
		{Type: "function", Function: official.ToolFunction{Name: "b", Parameters: jsonRaw(`{"type":"object","properties":{"path":{"type":"string"}}}`)}},
	}
	if tc4 := InferToolFromParams(map[string]any{"path": "x"}, tools2); tc4 != nil {
		t.Fatalf("歧义应返回 nil, got %+v", tc4)
	}
}

// Parser 带 tools:标签内参数直给 JSON 也能解析。
func TestParserParamDirect(t *testing.T) {
	p := NewParserWithTagsAndTools(dsTags, []official.Tool{
		{Type: "function", Function: official.ToolFunction{Name: "read", Parameters: jsonRaw(`{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}`)}},
	})
	_, calls := p.Feed(dsTags.StartTag + `{"file_path":"a.js"}` + dsTags.EndTag)
	if len(calls) != 1 || calls[0].Function.Name != "read" {
		t.Fatalf("参数直给解析失败: %+v", calls)
	}
}

func jsonRaw(s string) []byte { return []byte(s) }
