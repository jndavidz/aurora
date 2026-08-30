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
	toolCallsOpenRe    = regexp.MustCompile(`(?i)<tool_calls>`)
	toolCallsCloseRe   = regexp.MustCompile(`(?i)</tool_calls>`)
	toolCallAltOpenRe  = regexp.MustCompile(`(?i)<tool[_\s]call>`)
	toolCallAltCloseRe = regexp.MustCompile(`(?i)</tool[_\s]call>`)
	// DeepSeek 系(网页实测,2026-08):模型输出 <|tool▁calls▁begin|>(▁=U+2581)
	// 或丢前导 `|` 的 <tool_calls_begin|>、ASCII 下划线 <|tool_calls_begin|> 等变体。
	dsToolBeginRe = regexp.MustCompile(`(?i)<\|?tool[_\s▁]?calls?[_\s▁]?begin\|>`)
	dsToolEndRe   = regexp.MustCompile(`(?i)<\|?tool[_\s▁]?calls?[_\s▁]?end\|>`)
)

// NormalizeTagged 把常见的标签变体统一为 tags 的标准标签。
// 变体: <tool_calls> / <tool call> / <|tool_call_begin|> / <tool_calls_begin|> /
// <|tool▁calls▁begin|>(U+2581)等。
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

// openTagVariants 是模型可能输出的开标签变体(归一化前)。
// 网页实测:DeepSeek 常丢前导 `|`(如 <tool▁calls▁begin|>),或混用
// ASCII 下划线 / 单复数 call(s)。parser 用它们做"半个标签"尾部保留。
func openTagVariants(tags TagSet) []string {
	// 以 StartTag 为基础生成变体:去掉/保留前导 |,▁↔_,calls↔call。
	base := tags.StartTag
	variants := []string{base}
	if strings.HasPrefix(base, "<|") {
		variants = append(variants, "<"+base[2:])
	}
	// ASCII 下划线版
	ascii := strings.ReplaceAll(base, "\u2581", "_")
	variants = append(variants, ascii)
	if strings.HasPrefix(ascii, "<|") {
		variants = append(variants, "<"+ascii[2:])
	}
	// 单数 call 版
	variants = append(variants, strings.Replace(base, "calls", "call", 1))
	if strings.HasPrefix(base, "<|") {
		variants = append(variants, "<"+strings.Replace(base, "calls", "call", 1)[2:])
	}
	return variants
}

// keepPartialTagTail 返回 buffer 末尾需要保留的字符数:
// 若 buffer 以某个开标签变体的前缀结尾,保留该前缀,避免把"半个标签"当文本冲掉。
func keepPartialTagTail(buffer string, variants []string) int {
	best := 0
	for _, v := range variants {
		if v == "" {
			continue
		}
		for i := len(v); i >= 1; i-- {
			if strings.HasSuffix(buffer, v[:i]) {
				if i > best {
					best = i
				}
				break
			}
		}
	}
	return best
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
