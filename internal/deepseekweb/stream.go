package deepseekweb

import (
	"bufio"
	"encoding/json"
	"io"
	"regexp"
	"strings"
)

// Delta 是解析出的一帧增量:正文或思维链(至少一个非空)。
type Delta struct {
	Text      string
	Reasoning string
}

// StreamResult 是整条流的汇总。
type StreamResult struct {
	Text          string
	Reasoning     string
	ResponseMsgID string
	RequestMsgID  string
	Finished      bool
	Err           string
}

// 正文引用标记(联网搜索时模型在正文里嵌入的引用占位符,网页端渲染成引用卡片)。
// 形态: `[citation:1]`、`[citation:7]`(纯文本,可多个连排)。
var (
	citationFullRe = regexp.MustCompile(`\[\s*citation\s*:\s*[^\]]*\]`)
	citationTailRe = regexp.MustCompile(`\[\s*citation\s*:[^\]]*$`) // 尾部未闭合片段
)

// stripCitationMarkers 移除一段完整文本里的全部 citation 标记。
func stripCitationMarkers(s string) string {
	return citationFullRe.ReplaceAllString(s, "")
}

// citationStripper 是流式安全的引用标记剥离器:标记可能跨帧到达,
// 未闭合的标记片段留待下一帧补全,避免误删正文或泄漏标记片段。
type citationStripper struct {
	pending string
}

// Feed 输入一段增量,返回可安全输出的剥离后文本。
func (s *citationStripper) Feed(chunk string) string {
	s.pending += chunk
	clean := citationFullRe.ReplaceAllString(s.pending, "")
	// 若尾部有未闭合的 `[citation:` 片段,收回 pending,只输出其前的干净文本。
	// (i[1] == len(clean) 表示未闭合片段一直延伸到末尾)
	if i := citationTailRe.FindStringIndex(clean); i != nil && i[1] == len(clean) {
		s.pending = clean[i[0]:]
		return clean[:i[0]]
	}
	s.pending = ""
	return clean
}

// Flush 清空遗留的 pending(正常为空;异常截断时做完整剥离)。
func (s *citationStripper) Flush() string {
	clean := stripCitationMarkers(s.pending)
	s.pending = ""
	return clean
}

// ConsumeStream 消费 completion 的 SSE 响应,逐帧回调 onDelta。
//
// 帧格式:每帧 `\n\n` 分隔,含 event: 与 data: 行;data 为 p/o/v JSON-Patch。
// 支持 V4(fragments 数组:THINK/RESPONSE)与 V3(response/content、
// response/thinking_content)双格式;`event: close` 或 `data: [DONE]` 收尾。
func ConsumeStream(r io.Reader, onDelta func(Delta)) StreamResult {
	var res StreamResult
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	// 正文/思维链各用独立剥离器,流式增量过剥(引用标记可能跨帧)。
	var textStripper, reasoningStripper citationStripper
	origDelta := onDelta
	onDelta = func(d Delta) {
		if d.Text != "" {
			d.Text = textStripper.Feed(d.Text)
		}
		if d.Reasoning != "" {
			d.Reasoning = reasoningStripper.Feed(d.Reasoning)
		}
		origDelta(d)
	}

	var eventName string
	var dataLines []string
	flush := func() {
		// event 无 data 行(event: close 等)也要处理。
		if len(dataLines) == 0 {
			if eventName == "close" {
				res.Finished = true
			}
			eventName = ""
			return
		}
		payload := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		applyPayload(payload, eventName, onDelta, &res)
		eventName = ""
	}

	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			continue
		}
	}
	flush()

	// 非流式汇总也过一遍剥离(onDelta 只覆盖流式路径)。
	// res.Text 累积的是原始增量(含 citation 标记),整体剥一次即可;
	// reasoning 同理。
	res.Text = stripCitationMarkers(res.Text)
	res.Reasoning = stripCitationMarkers(res.Reasoning)

	if res.ResponseMsgID == "" && res.RequestMsgID == "" && res.Text == "" && res.Reasoning == "" && !res.Finished {
		res.Err = "empty stream"
	}
	return res
}

func applyPayload(payload, event string, onDelta func(Delta), res *StreamResult) {
	switch event {
	case "close":
		res.Finished = true
		return
	}
	if payload == "[DONE]" {
		res.Finished = true
		return
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return
	}

	// ready:记录 message id(续轮/停止用)
	if rawMsg, ok := raw["response_message_id"]; ok {
		res.ResponseMsgID = rawJSONString(rawMsg)
	}
	if rawMsg, ok := raw["request_message_id"]; ok {
		res.RequestMsgID = rawJSONString(rawMsg)
	}
	if event == "ready" {
		return
	}

	// hint:错误/限流
	if hint, ok := raw["content"]; ok && event == "hint" {
		res.Err = rawJSONString(hint)
		res.Finished = true
		return
	}

	// p/o/v patch 三字段
	p, hasP := raw["p"]
	_, hasO := raw["o"]
	v, hasV := raw["v"]

	switch {
	case hasV && hasP && hasO:
		applyPatch(strings.Trim(string(p), `"`), strings.Trim(string(v), `"`), raw, onDelta, res)
	case hasV && !hasP && !hasO:
		// 纯 v 帧:可能是 V4 初始快照(对象)或纯文本 delta(字符串)。
		applyBareV(v, onDelta, res)
	}
}

// applyBareV 处理纯 v 帧:
//   - v 是对象 → V4 初始快照(v.response.fragments)
//   - v 是字符串 → 当前 fragment 的纯文本 delta
func applyBareV(v json.RawMessage, onDelta func(Delta), res *StreamResult) {
	var s string
	if err := json.Unmarshal(v, &s); err == nil {
		if s != "" {
			res.Text += s
			onDelta(Delta{Text: s})
		}
		return
	}
	applyV4Snapshot(v, onDelta, res)
}

// applyV4Snapshot 处理 V4 初始快照 v.response.fragments[]。
func applyV4Snapshot(v json.RawMessage, onDelta func(Delta), res *StreamResult) {
	var top struct {
		Response struct {
			Fragments []struct {
				Type    string `json:"type"`
				Content string `json:"content"`
			} `json:"fragments"`
		} `json:"response"`
	}
	if err := json.Unmarshal(v, &top); err != nil {
		return
	}
	for _, f := range top.Response.Fragments {
		emitFragment(f.Type, f.Content, onDelta, res)
	}
}

func emitFragment(typ, content string, onDelta func(Delta), res *StreamResult) {
	if content == "" {
		return
	}
	switch strings.ToUpper(typ) {
	case "THINK":
		res.Reasoning += content
		onDelta(Delta{Reasoning: content})
	default: // RESPONSE 及其他
		res.Text += content
		onDelta(Delta{Text: content})
	}
}

// applyPatch 处理 p/o/v 增量。v 可能仍是 JSON 字符串/对象。
// 覆盖:V3 response/content、response/thinking_content;V4 fragments APPEND 与
// fragments/-1/content delta;BATCH(子补丁列表);response/status 收尾。
func applyPatch(path, vstr string, raw map[string]json.RawMessage, onDelta func(Delta), res *StreamResult) {
	switch {
	case path == "response" && isBatch(raw["v"]):
		// BATCH op:{"p":"response","o":"BATCH","v":[{"p":...,"v":...},...]}
		applyBatch(raw["v"], onDelta, res)
		return

	case strings.HasPrefix(path, "response/status"):
		if strings.Trim(vstr, `"`) == "FINISHED" || strings.Trim(vstr, `"`) == "INCOMPLETE" {
			res.Finished = true
		}
		return

	case strings.HasPrefix(path, "response/fragments"):
		applyV4Patch(path, raw["v"], onDelta, res)
		return

	case strings.HasPrefix(path, "response/thinking_content"):
		content := strings.Trim(vstr, `"`)
		if content != "" {
			res.Reasoning += content
			onDelta(Delta{Reasoning: content})
		}
		return

	case strings.HasPrefix(path, "response/content"):
		content := strings.Trim(vstr, `"`)
		if content != "" {
			res.Text += content
			onDelta(Delta{Text: content})
		}
		return
	}
}

// isBatch 报告 v 是否是 BATCH 的子补丁数组([{"p":...,"v":...}]).
func isBatch(v json.RawMessage) bool {
	var list []struct {
		P string          `json:"p"`
		V json.RawMessage `json:"v"`
	}
	return json.Unmarshal(v, &list) == nil
}

// applyBatch 依次应用 BATCH 里的子补丁。
func applyBatch(v json.RawMessage, onDelta func(Delta), res *StreamResult) {
	var list []struct {
		P string          `json:"p"`
		V json.RawMessage `json:"v"`
	}
	if err := json.Unmarshal(v, &list); err != nil {
		return
	}
	for _, sub := range list {
		subV := sub.V
		var s string
		if err := json.Unmarshal(subV, &s); err == nil {
			applyPatch(sub.P, s, map[string]json.RawMessage{"v": subV}, onDelta, res)
			continue
		}
		applyPatch(sub.P, string(subV), map[string]json.RawMessage{"v": subV}, onDelta, res)
	}
}

// applyV4Patch 处理 V4 fragments 的 APPEND 与 delta 增量。
// [P0] APPEND 值结构 [[fragment]] 与 delta 路径需官网实测确认。
func applyV4Patch(path string, v json.RawMessage, onDelta func(Delta), res *StreamResult) {
	if strings.HasSuffix(path, "/content") || path == "response/fragments" {
		// delta:可能是纯字符串,或 {content: ...}
		var s string
		if err := json.Unmarshal(v, &s); err == nil && s != "" {
			res.Text += s
			onDelta(Delta{Text: s})
			return
		}
		var frag struct {
			Type    string `json:"type"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(v, &frag); err == nil {
			emitFragment(frag.Type, frag.Content, onDelta, res)
			return
		}
	}
	// APPEND:[[fragment]] 追加(双层数组)
	var nested [][]struct {
		Type    string `json:"type"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(v, &nested); err == nil {
		for _, batch := range nested {
			for _, f := range batch {
				emitFragment(f.Type, f.Content, onDelta, res)
			}
		}
		return
	}
	// 单层 [fragment] 兜底
	var flat []struct {
		Type    string `json:"type"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(v, &flat); err == nil {
		for _, f := range flat {
			emitFragment(f.Type, f.Content, onDelta, res)
		}
	}
}
