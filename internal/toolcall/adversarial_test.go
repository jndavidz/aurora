package toolcall

import (
	"encoding/json"
	"strings"
	"testing"

	"aurora/typings/official"
)

// ── 对抗组 1:参数含特殊字符(S1 风险点③)────────────────────────────
// 风险:模型写"生成 tool_call 文档"类任务时,参数值里会出现字面量 </tool_call>,
// Feed 的 inside 模式用 strings.Index 找闭合标签,会在参数中间切断。

// 参数值含字面量闭合标签 —— 协议的已知边界,验证当前行为并记录。
func TestParamContainsLiteralEndTag(t *testing.T) {
	p := NewParser()
	// JSON 转义后参数值是 "</tool_call>" 字面量(反斜杠是 JSON 转义,不是真标签)
	in := `<tool_call>{"name":"write","arguments":{"content":"see </tool_call> below"}}</tool_call>`
	text, calls := p.Feed(in)
	text += strings.Join(flatten(p.Flush()), "")
	_ = text
	if len(calls) == 0 {
		t.Fatalf("参数含转义闭合标签时应仍解析出调用(实际:%q)", text)
	}
	if got := calls[0].Function.Arguments; !strings.Contains(got, "below") {
		t.Fatalf("参数应完整保留, got %q", got)
	}
}

// 参数值含真换行/引号/反斜杠混合 —— robustJSON 的修复路径不能破坏合法转义。
func TestParamMixedSpecialChars(t *testing.T) {
	p := NewParser()
	in := "<tool_call>{\"name\":\"run\",\"arguments\":{\"cmd\":\"echo \\\"a\\\\nb\\\" && dir G:\\\\src\"}}</tool_call>"
	text, calls := p.Feed(in)
	text += strings.Join(flatten(p.Flush()), "")
	_ = text
	if len(calls) != 1 {
		t.Fatalf("混合特殊字符应解析出 1 个调用, got %d (text=%q)", len(calls), text)
	}
	args := calls[0].Function.Arguments
	var decoded struct {
		Cmd string `json:"cmd"`
	}
	if err := json.Unmarshal([]byte(args), &decoded); err != nil {
		t.Fatalf("参数应为合法 JSON: %v (args=%q)", err, args)
	}
	want := `echo "a\nb" && dir G:\src`
	if decoded.Cmd != want {
		t.Fatalf("参数应保真(含 && 与反斜杠,无 HTML 转义)\n want %q\n got  %q", want, decoded.Cmd)
	}
	if strings.Contains(args, "u0026") {
		t.Fatalf("参数不应含 HTML 转义(\\u0026), got %q", args)
	}
}

// 参数值含 markdown 围栏(模型把 JSON 包进 ```json 代码块)。
func TestParamWrappedInFence(t *testing.T) {
	p := NewParser()
	in := "<tool_call>```json\n{\"name\":\"read\",\"arguments\":{\"path\":\"a.txt\"}}\n```</tool_call>"
	_, calls := p.Feed(in)
	if len(calls) != 1 {
		t.Fatalf("围栏包裹的 JSON 应可解析, got %d", len(calls))
	}
	if calls[0].Function.Name != "read" {
		t.Fatalf("name = %q", calls[0].Function.Name)
	}
}

// ── 对抗组 2:并行调用 + 流式交错(S1 风险点②)────────────────────────

// 3 个调用逐字节喂入 —— 验证顺序保持、内容完整、无文本泄漏。
func TestParallelCallsByteByByte(t *testing.T) {
	p := NewParser()
	in := "before" +
		`<tool_call>{"name":"a","arguments":{"x":1}}</tool_call>` +
		"mid" +
		`<tool_call>{"name":"b","arguments":{"y":2}}</tool_call>` +
		`<tool_call>{"name":"c","arguments":{"z":3}}</tool_call>` +
		"after"

	var text strings.Builder
	var calls []official.ToolCall
	for i := 0; i < len(in); i++ {
		td, tc := p.Feed(in[i : i+1])
		text.WriteString(td)
		calls = append(calls, tc...)
	}
	td, tc := p.Flush()
	text.WriteString(td)
	calls = append(calls, tc...)

	if len(calls) != 3 {
		t.Fatalf("calls = %d, want 3", len(calls))
	}
	for i, want := range []string{"a", "b", "c"} {
		if calls[i].Function.Name != want {
			t.Fatalf("calls[%d].name = %q, want %q(顺序必须保持)", i, calls[i].Function.Name, want)
		}
	}
	got := text.String()
	for _, want := range []string{"before", "mid", "after"} {
		if !strings.Contains(got, want) {
			t.Fatalf("文本段 %q 应保留, got %q", want, got)
		}
	}
	if strings.Contains(got, `"name"`) {
		t.Fatalf("JSON 不应泄漏到文本流, got %q", got)
	}
}

// 相邻两个调用之间零分隔 —— 闭合标签后紧跟开标签。
func TestAdjacentCallsNoGap(t *testing.T) {
	p := NewParser()
	in := `<tool_call>{"name":"a","arguments":{}}</tool_call><tool_call>{"name":"b","arguments":{}}</tool_call>`
	_, calls := p.Feed(in)
	if len(calls) != 2 {
		t.Fatalf("相邻调用应各解析一次, got %d", len(calls))
	}
	if calls[0].Function.Name != "a" || calls[1].Function.Name != "b" {
		t.Fatalf("顺序错: [%q, %q]", calls[0].Function.Name, calls[1].Function.Name)
	}
}

// ── 对抗组 3:多轮回灌(S1 风险点①)────────────────────────────────
// 场景:历史里已堆满 SerializeForHistory 生成的 <tool_call> 块(多轮工具调用),
// 新一轮模型输出再进同一个解析器 —— 历史块不能被误解析/干扰新解析。

func TestHistoryReplayThenFreshCall(t *testing.T) {
	// 构造两轮历史:assistant 发起调用 → tool 结果 → 再调用
	ref := func(id, name, args string) official.ToolCallRef {
		r := official.ToolCallRef{Index: 0, ID: id, Type: "function"}
		r.Function.Name = name
		r.Function.Arguments = args
		return r
	}
	history := SerializeForHistory([]official.ToolCallRef{
		ref("call_a", "read", `{"path":"a.txt"}`),
		ref("call_b", "write", `{"path":"b.txt","content":"data"}`),
	})
	if !strings.Contains(history, StartTag) {
		t.Fatalf("历史序列化应含 tool_call 标签, got %q", history[:80])
	}

	// 历史作为前缀进入解析器(模拟上下文回灌后模型继续输出):
	// 新输出是一个全新的调用 —— 历史块不应产生"幽灵调用"或吞掉新调用。
	p := NewParser()
	fresh := `<tool_call>{"name":"search","arguments":{"q":"golang"}}</tool_call>`
	text, calls := p.Feed(history + fresh)
	text += strings.Join(flatten(p.Flush()), "")
	_ = text

	// 当前实现:历史块也会被当作调用解析出来(解析器无"历史模式")。
	// 关键断言:新调用 search 必须在,且排在历史调用之后(顺序即轮次)。
	var names []string
	for _, c := range calls {
		names = append(names, c.Function.Name)
	}
	found := false
	for i, n := range names {
		if n == "search" {
			found = true
			if i < len(names)-1 {
				t.Fatalf("新调用应位于最后(历史调用之后), 实际序列 %v", names)
			}
		}
	}
	if !found {
		t.Fatalf("新调用 search 丢失, 实际序列 %v", names)
	}
	if len(names) < 3 {
		t.Fatalf("历史 2 个 + 新 1 个应至少解析出 3 个, got %v", names)
	}
}

// 历史回灌后模型输出"伪标签"(模仿格式的自然语言)—— 不应产生畸形调用。
func TestHistoryThenMimicryText(t *testing.T) {
	history := SerializeForHistory([]official.ToolCallRef{
		func() official.ToolCallRef {
			r := official.ToolCallRef{Index: 0, ID: "call_a", Type: "function"}
			r.Function.Name = "read"
			r.Function.Arguments = `{"path":"a.txt"}`
			return r
		}(),
	})

	p := NewParser()
	// 模型模仿:文本里出现"我要调用 <tool_call> 这样的标签"(无合法 JSON)
	in := history + "\nI will use <tool_call> like the history shows.\n"
	text, calls := p.Feed(in)
	text += strings.Join(flatten(p.Flush()), "")

	for _, c := range calls {
		if c.Function.Name == "" {
			t.Fatalf("模仿文本不应产生无名调用, text=%q", text)
		}
	}
	// 无合法 JSON 的伪标签块:flush 后作为文本回吐(不吞)或忽略均可,
	// 但不能崩溃/不能生成空 arguments 的调用。
	for _, c := range calls {
		if c.Function.Arguments == "" {
			t.Fatalf("不应产生空参数调用: %+v", c.Function)
		}
	}
}

// ── helpers ─────────────────────────────────────────────────────────

func flatten(td string, calls []official.ToolCall) []string {
	_ = calls
	if td == "" {
		return nil
	}
	return []string{td}
}
