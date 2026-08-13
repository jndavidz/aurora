package toolcall

import (
	"strings"
	"testing"

	"aurora/typings/official"
)

var dsTags = TagSet{StartTag: "<|tool\u2581calls\u2581begin|>", EndTag: "<|tool\u2581calls\u2581end|>"}

func TestNormalizeTaggedDeepSeek(t *testing.T) {
	cases := map[string]string{
		// 模型实际输出的变体(缺前导 |、ASCII 下划线、▁)
		"<tool_calls_begin|>{\"name\":\"x\"}<tool_calls_end|>": "<|tool▁calls▁begin|>{\"name\":\"x\"}<|tool▁calls▁end|>",
		"<|tool_call_begin|>{\"name\":\"x\"}<|tool_call_end|>": "<|tool▁calls▁begin|>{\"name\":\"x\"}<|tool▁calls▁end|>",
		"<|tool_calls_begin|>{\"name\":\"x\"}<|tool_calls_end|>": "<|tool▁calls▁begin|>{\"name\":\"x\"}<|tool▁calls▁end|>",
		"<|tool▁calls▁begin|>{\"name\":\"x\"}<|tool▁calls▁end|>": "<|tool▁calls▁begin|>{\"name\":\"x\"}<|tool▁calls▁end|>",
		"<tool_calls>{\"name\":\"x\"}</tool_calls>":                "<|tool▁calls▁begin|>{\"name\":\"x\"}<|tool▁calls▁end|>",
		"<tool call>{\"name\":\"x\"}</tool call>":                  "<|tool▁calls▁begin|>{\"name\":\"x\"}<|tool▁calls▁end|>",
	}
	for in, want := range cases {
		if got := NormalizeTagged(in, dsTags); got != want {
			t.Errorf("NormalizeTagged(%q) = %q, want %q", in, got, want)
		}
	}
}

// DeepSeek 标签流式解析。
func TestParserWithDeepSeekTags(t *testing.T) {
	p := NewParserWithTags(dsTags)
	chunks := []string{
		`<|tool_calls_begin|>`,
		`{"name": "bash", "arg`,
		`uments": {"command": "ls"}}`,
		`<|tool_calls_end|>`,
	}
	var text string
	var tcs []official.ToolCall
	for _, ch := range chunks {
		td, calls := p.Feed(ch)
		text += td
		tcs = append(tcs, calls...)
	}
	td, calls := p.Flush()
	text += td
	tcs = append(tcs, calls...)
	if len(tcs) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(tcs))
	}
	if tcs[0].Function.Name != "bash" {
		t.Errorf("name = %q, want bash", tcs[0].Function.Name)
	}
	if !strings.Contains(tcs[0].Function.Arguments, "ls") {
		t.Errorf("arguments = %q, want contains ls", tcs[0].Function.Arguments)
	}
	if text != "" {
		t.Errorf("text should be empty, got %q", text)
	}
}

// 默认标签(ChatGPT)行为不变:回归。
func TestParserDefaultTagsRegression(t *testing.T) {
	p := NewParser()
	_, calls := p.Feed(`<tool_call>{"name":"read","arguments":{"path":"a"}}</tool_call>`)
	if len(calls) != 1 || calls[0].Function.Name != "read" {
		t.Fatalf("default tag parse failed: %+v", calls)
	}
}

func TestStripTags(t *testing.T) {
	in := "前缀 " + dsTags.StartTag + `{"name":"x"}` + dsTags.EndTag + " 后缀"
	got := StripTags(in, dsTags)
	if got != "前缀  后缀" {
		t.Errorf("StripTags = %q", got)
	}
	// 未闭合:丢弃标签起以后内容
	in2 := "abc " + dsTags.StartTag + `{"name":"x"}`
	if got := StripTags(in2, dsTags); got != "abc " {
		t.Errorf("StripTags unclosed = %q", got)
	}
}

// 双标签提示词:BuildInstructionsWithTags 使用指定标签。
func TestBuildInstructionsWithTags(t *testing.T) {
	tools := []official.Tool{{Type: "function", Function: official.ToolFunction{Name: "bash", Description: "run"}}}
	got := BuildInstructionsWithTags(dsTags, tools, nil)
	if !strings.Contains(got, dsTags.StartTag) {
		t.Errorf("prompt missing deepseek tag:\n%s", got)
	}
	if strings.Contains(got, "<tool_call>") {
		t.Errorf("prompt must not use default tag:\n%s", got)
	}
	// 默认标签版本仍用 <tool_call>。
	gotDefault := BuildInstructions(tools, nil)
	if !strings.Contains(gotDefault, "<tool_call>") {
		t.Errorf("default prompt missing <tool_call>:\n%s", gotDefault)
	}
}
