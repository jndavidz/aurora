package glmweb

import (
	"bufio"
	"encoding/json"
	"io"
	"regexp"
	"strings"
)

// turnSearchRe 匹配 GLM 联网搜索引用标记:【turn0search9】(全角括号;兼容半角与带后缀变体)
var turnSearchRe = regexp.MustCompile(`[【\[]\s*turn0search[^】\]]*[】\]]`)

// Delta 是解析出的一帧增量。
type Delta struct {
	Text      string // 正文增量(相对上一帧)
	Reasoning string // 思考增量(相对上一帧)
	// ToolCall 是智谱原生 tool_calls content(非文本协议):
	// {"type":"tool_calls","tool_calls":{"id":"tool-xxx","name":"...","arguments":"..."}}
	// 模型在"全部工具智能体"训练下会输出该结构;finish 哨兵(name=finish)表示
	// 工具阶段结束,不作为真实调用上报。
	ToolCall *ToolCall
}

// ToolCall 是智谱原生 tool_calls 结构的解析结果。
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON 字符串(原生格式)
}

// IsFinish 报告是否为 finish 哨兵(工具阶段结束标记)。
func (t *ToolCall) IsFinish() bool { return t != nil && t.Name == "finish" }

// StreamResult 是整条流的汇总。
type StreamResult struct {
	Text           string
	Reasoning      string
	ConversationID string
	Model          string
	Finished       bool
	Err            string
}

// streamFrame 是一帧 assistant/stream 的 data 载荷。
type streamFrame struct {
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id"`
	Status         string `json:"status"`
	Model          string `json:"model"`
	LastError      struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	} `json:"last_error"`
	Parts []struct {
		Role    string `json:"role"`
		Content []struct {
			Type      string          `json:"type"` // "text" | "think" | "tool_calls"
			Text      string          `json:"text"`
			Think     string          `json:"think"`
			ToolCalls json.RawMessage `json:"tool_calls"`
		} `json:"content"`
	} `json:"parts"`
}

// ConsumeStream 消费 completion 的 SSE 响应。
//
// 智谱 SSE 特点:每帧 data: 是完整 JSON,parts 数组**全量重发**(非增量 patch),
// 最新一帧的 parts 是当前累计内容。因此:
//   - 取每帧最新 assistant part 的 think/text 字段,与上一帧做差值输出增量
//   - status == "finish" 收尾
func ConsumeStream(r io.Reader, onDelta func(Delta)) StreamResult {
	var res StreamResult
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	var lastText, lastReasoning string
	var lastToolCallKey string

	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			if payload == "[DONE]" {
				res.Finished = true
			}
			continue
		}
		var fr streamFrame
		if err := json.Unmarshal([]byte(payload), &fr); err != nil {
			continue
		}
		if fr.ConversationID != "" {
			res.ConversationID = fr.ConversationID
		}
		if fr.Model != "" {
			res.Model = fr.Model
		}
		// 提取累计 think / text,以及原生 tool_calls
		var think, text string
		var nativeTC *ToolCall
		for _, part := range fr.Parts {
			if part.Role != "assistant" {
				continue
			}
			for _, c := range part.Content {
				switch c.Type {
				case "think":
					if c.Think != "" {
						think = c.Think
					}
				case "text":
					if c.Text != "" {
						text = c.Text
					}
				case "tool_calls":
					// 原生工具调用(全量重发,取最新一帧;finish 哨兵另处理)
					if tc := parseNativeToolCall(c.ToolCalls); tc != nil {
						nativeTC = tc
					}
				}
			}
		}
		// 搜索引用标记过滤(全量重发模式下直接清洗累计文本,差值自然干净)
		text = turnSearchRe.ReplaceAllString(text, "")
		// 差值输出增量(全量重发,只发新增部分)
		if d := strings.TrimPrefix(think, lastReasoning); d != "" {
			res.Reasoning = think
			lastReasoning = think
			onDelta(Delta{Reasoning: d})
		}
		if d := strings.TrimPrefix(text, lastText); d != "" {
			res.Text = text
			lastText = text
			onDelta(Delta{Text: d})
		}
		// 原生 tool_calls:全量重发,仅在内容变化时上报(去重)。
		if nativeTC != nil && !nativeTC.IsFinish() {
			key := nativeTC.ID + "\x00" + nativeTC.Name + "\x00" + nativeTC.Arguments
			if key != lastToolCallKey {
				lastToolCallKey = key
				onDelta(Delta{ToolCall: nativeTC})
			}
		}
		if fr.Status == "finish" {
			res.Finished = true
		}
		if fr.LastError.Msg != "" {
			res.Err = fr.LastError.Msg
		}
	}
	if res.Text == "" && res.Reasoning == "" && res.Err == "" && !res.Finished {
		res.Err = "empty stream"
	}
	return res
}

// parseNativeToolCall 解析智谱原生 tool_calls content:
//
//	{"name":"search","arguments":"{\"q\":\"...\"}"}
//	{"id":"tool-xxx","name":"finish","arguments":"{}"}
//
// 返回 nil 表示无法解析(非工具帧)。
func parseNativeToolCall(raw json.RawMessage) *ToolCall {
	if len(raw) == 0 {
		return nil
	}
	var tc ToolCall
	if err := json.Unmarshal(raw, &tc); err != nil {
		return nil
	}
	if tc.Name == "" && tc.ID == "" {
		return nil
	}
	return &tc
}
