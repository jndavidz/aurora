package toolcall

import (
	"strings"

	"aurora/typings/official"
)

// FenceParser 流式拦截 markdown 围栏 JSON 工具调用(智谱 GLM 风格)。
//
// 与 Parser 的标签机制不同:GLM 网页版模型(chatglm.cn "全部工具智能体")
// 输出工具调用的自然格式是 markdown 围栏 JSON,如:
//
//	```json
//	{"type":"tool_calls","tool_calls":{"name":"list_files","arguments":"{\"path\":\"/tmp\"}"}}
//	```
//
// 而 ChatGPT 的 <tool_call> 标签、DeepSeek 的 <|tool▁calls▁begin|> 标签对它都无效
// (实测:GLM 会直接忽略标签,把任务当散文回答或编造 shell 输出)。
// FenceParser 检测 ```json/``` 围栏,围栏内的 JSON 解析为 ToolCall,不进入正文流;
// 围栏外的文本正常输出。
type FenceParser struct {
	buffer       string // 围栏内累积的原始内容(不含围栏符)
	inFence      bool   // 是否在围栏内
	lang         string // 围栏语言标识(如 json / 空)
	emittedCount int
	tools        []official.Tool
}

// NewFenceParser 构造围栏 JSON 解析器。tools 用于"参数直给"格式的工具名推断。
func NewFenceParser(tools []official.Tool) *FenceParser {
	return &FenceParser{tools: tools}
}

// Feed 喂入新 chunk,返回 (textDelta, toolCalls)。
// textDelta 是围栏外的正文增量;toolCalls 是本轮闭合围栏解析出的工具调用。
func (p *FenceParser) Feed(chunk string) (textDelta string, toolCalls []official.ToolCall) {
	p.buffer += chunk
	var text strings.Builder
	for {
		if !p.inFence {
			start := strings.Index(p.buffer, "```")
			if start < 0 {
				// 未出现围栏起始:整段都是正文。但末尾可能是"半个 ```"前几个反引号,
				// 保留最后 2 字符不输出,等下一段确认是否成围栏。
				keep := 0
				tail := p.buffer
				if i := strings.LastIndex(tail, "`"); i >= 0 && i > len(tail)-3 {
					keep = len(tail) - i
				}
				if keep > 0 {
					text.WriteString(p.buffer[:len(p.buffer)-keep])
					p.buffer = p.buffer[len(p.buffer)-keep:]
				} else {
					text.WriteString(p.buffer)
					p.buffer = ""
				}
				break
			}
			// 围栏起始前的内容是正文
			pre := p.buffer[:start]
			text.WriteString(pre)
			p.buffer = p.buffer[start:]
			p.inFence = true
			// 读语言标识(``` 后到行尾)
			rest := p.buffer[3:]
			nl := strings.Index(rest, "\n")
			if nl < 0 {
				// 语言标识可能还在路上;等下一段
				break
			}
			p.lang = strings.TrimSpace(rest[:nl])
			p.buffer = rest[nl+1:]
			continue
		}
		// inFence:找闭合 ```(行首或独立一行)
		closeIdx := findFenceClose(p.buffer)
		if closeIdx < 0 {
			break
		}
		raw := p.buffer[:closeIdx]
		p.buffer = p.buffer[closeIdx+3:]
		p.inFence = false
		p.lang = ""
		if tc := p.buildToolCallFromRaw(raw); tc != nil {
			toolCalls = append(toolCalls, *tc)
			p.emittedCount++
		}
		// 解析失败:围栏内容不回吐(避免 JSON 泄漏进正文),静默丢弃
	}
	return text.String(), toolCalls
}

// Flush 收尾:处理未闭合的围栏。规则:
//   - 围栏内剩余内容若可解析成工具调用,产出调用;
//   - 否则视为正文回吐(只回吐确定非 JSON 的部分,防止半个 JSON 泄漏)。
func (p *FenceParser) Flush() (textDelta string, toolCalls []official.ToolCall) {
	remaining := p.buffer
	p.buffer = ""
	if remaining == "" {
		return "", nil
	}
	if p.inFence {
		// 未闭合围栏:尝试解析;解析不出且无任何输出时,把围栏起始当正文回吐
		if tc := p.buildToolCallFromRaw(remaining); tc != nil {
			p.emittedCount++
			return "", []official.ToolCall{*tc}
		}
		if p.emittedCount == 0 {
			return "```" + remaining, nil
		}
		return "", nil
	}
	// 不在围栏内:剩余是普通正文
	return remaining, nil
}

// FlushCalls 提取当前 buffer 中的所有围栏 JSON 工具调用(非流式场景用)。
// 注意:与 Flush 一样会消耗 buffer;不要与 Feed/Flush 混用。
func (p *FenceParser) FlushCalls() []official.ToolCall {
	var calls []official.ToolCall
	_, parsed := p.Feed("")
	calls = append(calls, parsed...)
	_, parsed = p.Flush()
	calls = append(calls, parsed...)
	return calls
}

// FlushCallsFromText 一次性喂入全文并提取所有围栏 JSON 工具调用(非流式便捷入口)。
func (p *FenceParser) FlushCallsFromText(text string) []official.ToolCall {
	_, calls := p.Feed(text)
	calls = append(calls, p.FlushCalls()...)
	return calls
}

// StripFencedBlocks 移除文本中的所有 ```json ... ``` 围栏块(含内容),
// 返回剩余正文。用于非流式场景:工具调用 JSON 已单独解析,正文不再保留围栏块。
func StripFencedBlocks(text string) string {
	var out strings.Builder
	rest := text
	for {
		start := strings.Index(rest, "```")
		if start < 0 {
			out.WriteString(rest)
			break
		}
		out.WriteString(rest[:start])
		rest = rest[start+3:]
		// 语言标识行
		if nl := strings.Index(rest, "\n"); nl >= 0 {
			rest = rest[nl+1:]
		}
		closeIdx := findFenceClose(rest)
		if closeIdx < 0 {
			// 未闭合:丢弃剩余
			break
		}
		rest = rest[closeIdx+3:]
	}
	return strings.TrimSpace(out.String())
}

// buildToolCallFromRaw 复用 Parser 的围栏 JSON 解析逻辑。
func (p *FenceParser) buildToolCallFromRaw(raw string) *official.ToolCall {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil
	}
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

// findFenceClose 在围栏内容中查找闭合 ``` 的位置。
// 允许闭合符独占一行或紧随内容。返回 ``` 起始索引;找不到返回 -1。
func findFenceClose(s string) int {
	for i := 0; i+3 <= len(s); i++ {
		if s[i] != '`' {
			continue
		}
		// 必须是连续三个反引号
		if s[i+1] == '`' && s[i+2] == '`' {
			// 前一个字符若是反引号,属于更长围栏(````),不算闭合
			if i > 0 && s[i-1] == '`' {
				continue
			}
			// 闭合围栏可以紧跟内容(...```)或独立一行(\n```\n)
			return i
		}
	}
	return -1
}
