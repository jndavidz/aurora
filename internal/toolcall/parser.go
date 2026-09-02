// Package toolcall 实现了  的工具调用文本协议解析与生成:
//
//   - 模型无法原生调用工具,所以在 system prompt 中教导它输出
//     <tool_call>{"name": "X", "arguments": {...}}</tool_call>
//   - Parser 是流式状态机:每收到一段 chunk 就吐出 (textDelta, toolCalls)
//   - RobustJSON 修复模型常见的 JSON 错误(Windows 路径反斜杠、未闭合括号)
//   - RecoverFromText 是兜底:模型有时绕过 <tool_call> 直接输出 JSON
//
// 设计取舍:对齐  行为以保证 aurora 客户端能复用相同的 prompt 模板。
package toolcall

import (
	"encoding/json"
	"regexp"
	"strings"

	"aurora/typings/official"
)

const (
	StartTag = "<tool_call>"
	EndTag   = "</tool_call>"
)

// Parser 是流式 <tool_call> 解析器。每收到一段文本,调用 Feed 拿到增量。
// 默认使用 DefaultTags(ChatGPT);DeepSeek 等用 NewParserWithTags 指定标签。
// 提供 tools 时,无 name 的"参数直给"JSON 可按 schema 推断工具名。
type Parser struct {
	buffer       string
	inside       bool
	emittedCount int
	emittedText  bool
	tags         TagSet
	tools        []official.Tool
}

// NewParser 构造一个 ChatGPT 默认标签的解析器。零值不可直接使用。
func NewParser() *Parser {
	return &Parser{tags: DefaultTags}
}

// NewParserWithTags 构造指定标签集的解析器(DeepSeek 等)。
func NewParserWithTags(tags TagSet) *Parser {
	return &Parser{tags: tags}
}

// NewParserWithTagsAndTools 构造指定标签与工具列表的解析器。
// tools 用于"参数直给"格式({file_path:...} 无 name)的工具名推断。
func NewParserWithTagsAndTools(tags TagSet, tools []official.Tool) *Parser {
	return &Parser{tags: tags, tools: tools}
}

// buildToolCallFromRaw 尝试把标签内的纯文本解析成 ToolCall。
// 优先按 name 字段;无 name 时若提供了 tools,按参数键推断工具名。
func (p *Parser) buildToolCallFromRaw(raw string) *official.ToolCall {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil
	}
	// 去掉 markdown 围栏
	s = stripMarkdownFence(s)
	idx := strings.Index(s, "{")
	if idx < 0 {
		return nil
	}
	s = s[idx:]
	obj, ok := robustJSON(s)
	if !ok {
		return nil
	}
	if tc := buildToolCallFromObject(obj); tc != nil {
		return tc
	}
	if len(p.tools) > 0 {
		return InferToolFromParams(obj, p.tools)
	}
	return nil
}

// Feed 把新 chunk 喂入解析器,返回 (textDelta, toolCalls)。
// textDelta 是本轮新产生的可输出文本(不含 <tool_call> 标签及其内部 JSON);
// toolCalls 是本轮闭合的 tool_call 列表,顺序与出现顺序一致。
func (p *Parser) Feed(chunk string) (textDelta string, toolCalls []official.ToolCall) {
	p.buffer = NormalizeTagged(p.buffer+chunk, p.tags)
	var text strings.Builder
	for len(p.buffer) > 0 {
		if !p.inside {
			startIdx := strings.Index(p.buffer, p.tags.StartTag)
			if startIdx >= 0 {
				pre := p.buffer[:startIdx]
				if pre != "" {
					// 已经被吐出的"半个标签"防误切,以及可能被截断的 JSON 防御
					// (looksLikeJSONJunk + 还没任何输出):这两种情况不输出,留待下个 chunk
					if !(p.emittedCount == 0 && p.emittedText == false && looksLikeJSONJunk(pre)) {
						text.WriteString(pre)
						if strings.TrimSpace(pre) != "" {
							p.emittedText = true
						}
					}
				}
				p.inside = true
				p.buffer = p.buffer[startIdx+len(p.tags.StartTag):]
				continue
			}
			// 没找到 startTag;但 buffer 末尾可能是某个开标签变体的"半个标签",
			// 需要保留若干字符避免误切。
			flushIndex := len(p.buffer)
			if keep := keepPartialTagTail(p.buffer, openTagVariants(p.tags)); keep > 0 {
				flushIndex = len(p.buffer) - keep
			}
			pre := p.buffer[:flushIndex]
			if pre != "" {
				head := strings.TrimSpace(pre)
				// 防御:在还没有任何文本/工具被吐出的情况下,若 pre 看起来像 JSON
				// (被截断的 tool_call),先不要输出,等下一段判断
				if p.emittedCount == 0 && !p.emittedText && (strings.HasPrefix(head, "{") || strings.HasPrefix(head, "`")) {
					break
				}
				text.WriteString(pre)
				if strings.TrimSpace(pre) != "" {
					p.emittedText = true
				}
			}
			p.buffer = p.buffer[flushIndex:]
			break
		}
		// inside 模式:寻找 endTag。解析失败时跳过该闭合、找下一个重试 ——
		// 参数 JSON 的字符串值里可能含字面量 "</tool_call>"(合法 JSON 无需转义
		// <>/),真实场景:模型执行"生成 tool_call 文档"类任务。若不跳,整个
		// 调用会被静默丢弃(对抗测试 TestParamContainsLiteralEndTag 锁定)。
		searchFrom := 0
		matched := false
		for {
			rel := strings.Index(p.buffer[searchFrom:], p.tags.EndTag)
			if rel < 0 {
				break // 没有更多闭合标签:等待更多输入(原行为)
			}
			endIdx := searchFrom + rel
			raw := strings.TrimSpace(p.buffer[:endIdx])
			if tc := p.buildToolCallFromRaw(raw); tc != nil {
				toolCalls = append(toolCalls, *tc)
				p.emittedCount++
				p.inside = false
				p.buffer = p.buffer[endIdx+len(p.tags.EndTag):]
				matched = true
				break
			}
			searchFrom = endIdx + len(p.tags.EndTag) // 解析失败:跳过此闭合,试下一个
		}
		if !matched {
			break
		}
	}
	return text.String(), toolCalls
}

// Flush 在流结束时调用,把 buffer 中剩余的、未闭合的 <tool_call> 也尝试解析一次。
// 典型场景:模型在末尾输出了 <tool_call>{...} 但忘了写结束标签。
func (p *Parser) Flush() (textDelta string, toolCalls []official.ToolCall) {
	remaining := p.buffer
	p.buffer = ""
	if remaining == "" {
		return "", nil
	}
	if p.inside {
		// 还在工具块里 —— 尝试解析剩余内容
		if tc := p.buildToolCallFromRaw(remaining); tc != nil {
			toolCalls = append(toolCalls, *tc)
			p.emittedCount++
			return "", toolCalls
		}
		// 解析不出来 —— 把 startTag 也作为文本回吐,避免吞掉
		if p.emittedCount == 0 {
			return p.tags.StartTag + remaining, nil
		}
		return "", nil
	}
	// 不在工具块里 —— 看剩余文本是否整体就是一个 JSON
	if p.emittedCount == 0 {
		if tc := p.buildToolCallFromRaw(remaining); tc != nil {
			toolCalls = append(toolCalls, *tc)
			p.emittedCount++
			return "", toolCalls
		}
		if !p.emittedText {
			return remaining, nil
		}
	}
	return "", nil
}

func looksLikeJSONJunk(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" {
		return false
	}
	return strings.HasPrefix(t, "`") || strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[")
}

func stripMarkdownFence(s string) string {
	s = fenceOpenRe.ReplaceAllString(s, "")
	s = fenceCloseRe.ReplaceAllString(strings.TrimSpace(s), "")
	return strings.TrimSpace(s)
}

var (
	fenceOpenRe  = regexp.MustCompile("^```[a-zA-Z]*\\s*")
	fenceCloseRe = regexp.MustCompile("```$")
)

// fixLoneBackslashes 把 JSON 字符串里"非合法转义"的反斜杠变成双反斜杠,
// 解决 Windows 路径(G:\src\x.py)、正则(\d+)把 json.loads 弄坏的问题。
// 合法转义(\"、\\、\/、\b、\f、\n、\r、\t、\u)保留原样。
func fixLoneBackslashes(s string) string {
	var out strings.Builder
	out.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' {
			out.WriteByte(c)
			continue
		}
		nxt := byte(0)
		if i+1 < len(s) {
			nxt = s[i+1]
		}
		if nxt != 0 && strings.IndexByte(`"\\/bfnrtu`, nxt) >= 0 {
			out.WriteByte('\\')
			out.WriteByte(nxt)
			i++
			continue
		}
		out.WriteByte('\\')
		out.WriteByte('\\')
	}
	return out.String()
}

// robustJSON 尽力把一段"看起来像 JSON"的字符串解析成 map[string]any。
// 依次尝试:反斜杠修复 → 完整解析 → 截取到第一个完整花括号后解析。
func robustJSON(s string) (map[string]any, bool) {
	if s == "" {
		return nil, false
	}
	repaired := fixLoneBackslashes(s)
	if v, err := parseObject(repaired); err == nil {
		return v, true
	}
	// 兜底:截到第一个完整 {} 区间
	if end := firstBalancedObject(repaired); end > 0 {
		if v, err := parseObject(repaired[:end+1]); err == nil {
			return v, true
		}
	}
	return nil, false
}

func parseObject(s string) (map[string]any, error) {
	var v map[string]any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil, err
	}
	return v, nil
}

// firstBalancedObject 返回与首个 { 配对的 } 索引;解析失败返回 -1。
// 处理引号和反斜杠转义。
func firstBalancedObject(s string) int {
	depth := 0
	inStr := false
	esc := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if esc {
			esc = false
			continue
		}
		if c == '\\' {
			esc = true
			continue
		}
		if c == '"' {
			inStr = !inStr
			continue
		}
		if inStr {
			continue
		}
		switch c {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// buildToolCallFromObject 从 map 抽出 name + arguments 拼成 ToolCall。
// 兼容字段别名: name / tool / tool_name / function。
// arguments 接受 string、map、缺失三种形态。
func buildToolCallFromObject(obj map[string]any) *official.ToolCall {
	if obj == nil {
		return nil
	}
	name := pickString(obj, "name", "tool", "tool_name", "function")
	if name == "" {
		// 智谱原生结构:{"type":"tool_calls","tool_calls":{"name":..,"arguments":".."}}
		// name/arguments 在嵌套对象里。
		if nested, ok := obj["tool_calls"].(map[string]any); ok {
			if n := pickString(nested, "name", "tool", "tool_name", "function"); n != "" {
				return buildToolCallFromObject(nested)
			}
		}
		return nil
	}
	args := extractArguments(obj)
	return &official.ToolCall{
		Index: 0, // Index 在 StreamToToolCallDeltas 里统一分配
		ID:    generateCallID(),
		Type:  "function",
		Function: official.ToolCallFunc{
			Name:      name,
			Arguments: marshalArguments(args),
		},
	}
}

func pickString(obj map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := obj[k]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	return ""
}

func extractArguments(obj map[string]any) any {
	for _, k := range []string{"arguments", "parameters", "args"} {
		if v, ok := obj[k]; ok {
			return v
		}
	}
	// 没有 arguments 字段:把其余字段都当作 arguments(对齐 Python 行为)
	remaining := make(map[string]any, len(obj))
	for k, v := range obj {
		if k == "name" || k == "tool" || k == "tool_name" || k == "function" {
			continue
		}
		remaining[k] = v
	}
	return remaining
}

func marshalArguments(v any) string {
	switch t := v.(type) {
	case nil:
		return "{}"
	case string:
		s := strings.TrimSpace(t)
		// 模型有时直接给对象,有时给字符串;统一为合法 JSON
		if strings.HasPrefix(s, "{") {
			if _, ok := robustJSON(s); ok {
				return s
			}
		}
		// 字符串不是 JSON,作为 command 类参数包一层
		return marshalNoHTMLEscape(map[string]string{"command": s})
	case map[string]any:
		return marshalNoHTMLEscape(t)
	default:
		return marshalNoHTMLEscape(t)
	}
}

// marshalNoHTMLEscape 与 json.Marshal 等价,但禁用 HTML 转义:
// 默认 Marshal 会把 & < > 变 \u0026 等 —— shell 命令里的 `&&` 是超高频参数,
// 转义虽可被下游 Unmarshal 还原,但违背"参数保真"且干扰中间层肉眼核对。
func marshalNoHTMLEscape(v any) string {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		b, _ := json.Marshal(v)
		return string(b)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// generateCallID 分配 call_xxx 风格的 ID,与 Python  保持一致
// (24 个 hex 字符的随机后缀)。
func generateCallID() string {
	return "call_" + newCallIDSuffix()
}
