package toolcall

import (
	"regexp"
	"strings"
)

// TagSet 描述一套工具调用文本协议的标签。
// ChatGPT 网页逆向用 <tool_call>/</tool_call>;
// DeepSeek 网页用 <|tool_calls_begin|>/<|tool_calls_end|> 系(全角变体 P0 实测后补)。
type TagSet struct {
	StartTag string
	EndTag   string
}

// DefaultTags 是 ChatGPT 的标签集(现有行为,保持默认)。
var DefaultTags = TagSet{StartTag: "<tool_call>", EndTag: "</tool_call>"}

var (
	toolCallsOpenRe  = regexp.MustCompile(`(?i)<tool_calls>`)
	toolCallsCloseRe = regexp.MustCompile(`(?i)</tool_calls>`)
	toolCallAltOpenRe  = regexp.MustCompile(`(?i)<tool[_\s]call>`)
	toolCallAltCloseRe = regexp.MustCompile(`(?i)</tool[_\s]call>`)
	dsToolBeginRe = regexp.MustCompile(`(?i)<\|tool[_\s]?calls?[_]?begin\|>`)
	dsToolEndRe   = regexp.MustCompile(`(?i)<\|tool[_\s]?calls?[_]?end\|>`)
)

// NormalizeTagged 把常见的标签变体统一为 tags 的标准标签。
// 变体: <tool_calls> / <tool call> / <|tool_call_begin|> / <|tool_calls_end|> 等。
func NormalizeTagged(s string, tags TagSet) string {
	if tags.StartTag == "" {
		tags = DefaultTags
	}
	s = toolCallsOpenRe.ReplaceAllString(s, tags.StartTag)
	s = toolCallsCloseRe.ReplaceAllString(s, tags.EndTag)
	s = toolCallAltOpenRe.ReplaceAllString(s, tags.StartTag)
	s = toolCallAltCloseRe.ReplaceAllString(s, tags.EndTag)
	s = dsToolBeginRe.ReplaceAllString(s, tags.StartTag)
	s = dsToolEndRe.ReplaceAllString(s, tags.EndTag)
	return s
}

// StripTags 移除文本里成对的工具标签块(含块内 JSON),只留正文。
// 用于非流式响应里把 <tool_call>{...}</tool_call> 从最终文本中剥离。
func StripTags(text string, tags TagSet) string {
	if tags.StartTag == "" || tags.EndTag == "" {
		return text
	}
	var sb strings.Builder
	for {
		start := strings.Index(text, tags.StartTag)
		if start < 0 {
			sb.WriteString(text)
			break
		}
		sb.WriteString(text[:start])
		rest := text[start+len(tags.StartTag):]
		end := strings.Index(rest, tags.EndTag)
		if end < 0 {
			// 未闭合:丢弃标签起以后的内容
			break
		}
		text = rest[end+len(tags.EndTag):]
	}
	return sb.String()
}
