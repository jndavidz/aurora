package toolcall

import (
	"strings"
	"testing"

	"aurora/typings/official"
)

func fenceTools() []official.Tool {
	return []official.Tool{{Type: "function", Function: official.ToolFunction{
		Name:        "list_files",
		Description: "list files in a directory",
		Parameters: []byte(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
	}}}
}

// 流式分片:围栏 JSON 从中间切开,应正确拦截且不泄漏进正文。
func TestFenceParserStreamChunked(t *testing.T) {
	stream := []string{
		"让我查一下目录。\n```json\n",
		`{"name":"list_files","arguments":{"path":"`,
		`/tmp"}}`,
		"\n```\n好的,以上是结果。",
	}
	p := NewFenceParser(fenceTools())
	var text strings.Builder
	var calls []official.ToolCall
	for _, chunk := range stream {
		txt, c := p.Feed(chunk)
		text.WriteString(txt)
		calls = append(calls, c...)
	}
	txt, c := p.Flush()
	text.WriteString(txt)
	calls = append(calls, c...)

	got := text.String()
	if strings.Contains(got, "```") || strings.Contains(got, "list_files") {
		t.Errorf("围栏 JSON 泄漏进正文: %q", got)
	}
	if !strings.Contains(got, "让我查一下目录") || !strings.Contains(got, "好的,以上是结果") {
		t.Errorf("正文丢失: %q", got)
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if calls[0].Function.Name != "list_files" {
		t.Errorf("name = %q, want list_files", calls[0].Function.Name)
	}
	if !strings.Contains(calls[0].Function.Arguments, "/tmp") {
		t.Errorf("arguments = %q, want contain /tmp", calls[0].Function.Arguments)
	}
}

// 智谱原生结构:{"type":"tool_calls","tool_calls":{"name":..,"arguments":".."}}
func TestFenceParserNativeShape(t *testing.T) {
	text := "```json\n{\"type\":\"tool_calls\",\"tool_calls\":{\"name\":\"list_files\",\"arguments\":\"{\\\"path\\\":\\\"/tmp\\\"}\"}}\n```"
	p := NewFenceParser(fenceTools())
	calls := p.FlushCallsFromText(text)
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if calls[0].Function.Name != "list_files" {
		t.Errorf("name = %q, want list_files", calls[0].Function.Name)
	}
	// arguments 是字符串形式的 JSON,应原样保留
	if !strings.Contains(calls[0].Function.Arguments, "/tmp") {
		t.Errorf("arguments = %q, want contain /tmp", calls[0].Function.Arguments)
	}
}

// 无围栏的裸 JSON:模型没加围栏时,正文应完整返回,且不产出调用。
func TestFenceParserNoFence(t *testing.T) {
	text := "这是一个普通回答,没有工具调用。"
	p := NewFenceParser(fenceTools())
	txt, calls := p.Feed(text)
	if len(calls) != 0 {
		t.Fatalf("calls = %d, want 0", len(calls))
	}
	if txt != text {
		t.Errorf("正文应为 %q, got %q", text, txt)
	}
}

// 围栏语言标识带 json 语言名(```json 而非 ```)
func TestFenceParserLang(t *testing.T) {
	text := "```json\n{\"name\":\"list_files\",\"arguments\":{\"path\":\"a\"}}\n```"
	p := NewFenceParser(fenceTools())
	calls := p.FlushCallsFromText(text)
	if len(calls) != 1 || calls[0].Function.Name != "list_files" {
		t.Fatalf("calls = %+v, want 1 list_files", calls)
	}
}

// StripFencedBlocks:非流式全文清洗
func TestStripFencedBlocks(t *testing.T) {
	text := "开头\n```json\n{\"name\":\"list_files\",\"arguments\":{\"path\":\"a\"}}\n```\n结尾"
	clean := StripFencedBlocks(text)
	if strings.Contains(clean, "```") || strings.Contains(clean, "list_files") {
		t.Errorf("清洗后仍含围栏块: %q", clean)
	}
	if !strings.Contains(clean, "开头") || !strings.Contains(clean, "结尾") {
		t.Errorf("清洗丢失正文: %q", clean)
	}
}

// 未闭合围栏:Flush 时若可解析产出调用;否则回吐围栏起始。
func TestFenceParserUnclosed(t *testing.T) {
	// 可解析的未闭合围栏
	p := NewFenceParser(fenceTools())
	_, _ = p.Feed("```json\n{\"name\":\"list_files\",\"arguments\":{\"path\":\"a\"}}")
	_, calls := p.Flush()
	if len(calls) != 1 {
		t.Errorf("未闭合可解析围栏应产出调用, got %d", len(calls))
	}
	// 不可解析的未闭合围栏 → 回吐
	p2 := NewFenceParser(fenceTools())
	_, _ = p2.Feed("```json\n{\"name\":")
	txt, calls2 := p2.Flush()
	if len(calls2) != 0 {
		t.Errorf("未闭合不可解析不应产出调用, got %d", len(calls2))
	}
	if !strings.Contains(txt, "```") {
		t.Errorf("未闭合围栏应回吐起始, got %q", txt)
	}
}
