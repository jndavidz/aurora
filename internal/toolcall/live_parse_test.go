package toolcall

import (
	"fmt"
	"testing"
)

// 用模型真实输出的文本模拟流式分块,验证 DeepSeek 标签解析。
func TestParseRealDeepSeekOutput(t *testing.T) {
	tags := TagSet{StartTag: "<|tool\u2581calls\u2581begin|>", EndTag: "<|tool\u2581calls\u2581end|>"}
	p := NewParserWithTags(tags)
	// 模拟分块:按 3 字符切
	chunks := splitChunks("根据要求，使用bash工具列出当前目录。用bash命令，比如ls。输出工具调用块。<tool\u2581calls\u2581begin|>\n{\"name\": \"bash\", \"arguments\": {\"command\": \"ls -la\"}}\n<|tool\u2581calls\u2581end|>", 3)
	var calls []string
	var text string
	for _, ch := range chunks {
		td, cs := p.Feed(ch)
		text += td
		for _, c := range cs {
			calls = append(calls, fmt.Sprintf("%s(%s)", c.Function.Name, c.Function.Arguments))
		}
	}
	td, cs := p.Flush()
	text += td
	for _, c := range cs {
		calls = append(calls, fmt.Sprintf("%s(%s)", c.Function.Name, c.Function.Arguments))
	}
	fmt.Printf("text=%q\ncalls=%v\n", text, calls)
	if len(calls) != 1 {
		t.Fatalf("calls = %v, want 1", calls)
	}
}

func splitChunks(s string, n int) []string {
	var out []string
	r := []rune(s)
	for i := 0; i < len(r); i += n {
		end := i + n
		if end > len(r) {
			end = len(r)
		}
		out = append(out, string(r[i:end]))
	}
	return out
}
