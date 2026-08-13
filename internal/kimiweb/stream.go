package kimiweb

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"regexp"
	"strings"
)

// 正文引用标记(联网搜索时模型在正文里嵌入的引用占位符,网页端渲染成引用卡片)。
// 形态(2026-08-14 实测):
//   - 开头块:   article🛠web_search:1#5🛠web_search:1#0🎨
//   - 引用块:   cite🛠web_search:1#5:~:text=<引文>🛠(或  收尾)
//   - 内联引用: 🛠web_search:1#5🛠
//
// 特殊字符:🛠 U+1F6E0、🎨 U+1F3A8、 U+E3A0、 U+E3A8(私有区)。
var (
	markerArticleRe = regexp.MustCompile(`\x{E3A0}article\x{1F6E0}.*?\x{1F3A8}`)
	markerCiteRe    = regexp.MustCompile(`\x{E3A0}cite\x{1F6E0}[^\x{1F6E0}\x{1F3A8}\x{E3A0}\x{E3A8}]*[\x{1F6E0}\x{E3A8}]`)
	markerRefRe     = regexp.MustCompile(`\x{1F6E0}[^\x{1F6E0}\x{1F3A8}\x{E3A0}\x{E3A8}]*\x{1F6E0}`)
	markerCleanRe   = regexp.MustCompile(`[\x{1F6E0}\x{1F3A8}\x{E3A0}\x{E3A8}]`)
)

// stripCitationMarkers 移除一段完整文本里的全部引用标记(含孤立标记字符清理)。
func stripCitationMarkers(s string) string {
	s = markerArticleRe.ReplaceAllString(s, "")
	s = markerCiteRe.ReplaceAllString(s, "")
	s = markerRefRe.ReplaceAllString(s, "")
	s = markerCleanRe.ReplaceAllString(s, "")
	return s
}

// stripCompleteMarkers 只移除完整闭合的标记(流式路径用,不清理孤立字符——
// 流中"孤立"的标记字符很可能是不完整标记的开头,清理会误伤)。
func stripCompleteMarkers(s string) string {
	s = markerArticleRe.ReplaceAllString(s, "")
	s = markerCiteRe.ReplaceAllString(s, "")
	s = markerRefRe.ReplaceAllString(s, "")
	return s
}

// citationStripper 是流式安全的引用标记剥离器:标记可能跨帧到达,
// 未闭合的标记片段留待下一帧补全,避免误删正文或泄漏标记片段。
type citationStripper struct {
	pending string
}

// Feed 输入一段增量,返回可安全输出的剥离后文本。
func (s *citationStripper) Feed(chunk string) string {
	s.pending += chunk
	clean := stripCompleteMarkers(s.pending)
	// 完整标记都已剥离后,若仍有残留标记字符,说明尾部有未闭合片段:
	// 从首个残留标记字符起收回 pending,只输出其前的干净文本。
	if i := firstMarkerCharIndex(clean); i >= 0 {
		s.pending = clean[i:]
		return clean[:i]
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

// firstMarkerCharIndex 返回首个标记字符(🛠/🎨//)的字节下标;无则 -1。
func firstMarkerCharIndex(s string) int {
	for i, r := range s {
		switch r {
		case '\U0001F6E0', '\U0001F3A8', '\uE3A0', '\uE3A8':
			return i
		}
	}
	return -1
}

// Delta 是解析出的一帧增量。
type Delta struct {
	Text      string // 正文增量
	Reasoning string // 思考增量
	// ToolCall 是 Kimi 原生工具调用(模型自带工具,如 ipython / web_search):
	// 在 args 定稿(RUNNING)时整单上报,args 为完整 JSON 字符串。
	ToolCall *ToolCall
}

// ToolCall 是 Kimi 原生工具调用结构。
type ToolCall struct {
	ID        string // 归一化后的 toolCallId,如 "ipython:1" / "web_search:1"
	Name      string // 如 "ipython" / "web_search"
	Arguments string // JSON 字符串,如 {"code":"..."}
}

// StreamResult 是整条流的汇总。
type StreamResult struct {
	Text      string
	Reasoning string
	Finished  bool
	Err       string
}

// streamFrame 是 Connect 帧的通用结构(只取需要的字段)。
type streamFrame struct {
	Op        string          `json:"op"`
	Mask      string          `json:"mask"`
	Error     *streamError    `json:"error"`
	Done      json.RawMessage `json:"done"`
	Heartbeat json.RawMessage `json:"heartbeat"`
	Block     *struct {
		ID    string `json:"id"`
		Think *struct {
			Content string `json:"content"`
		} `json:"think"`
		Text *struct {
			Content string `json:"content"`
		} `json:"text"`
		Tool *struct {
			ToolCallID string `json:"toolCallId"`
			Name       string `json:"name"`
			Args       string `json:"args"`
			Status     string `json:"status"`
		} `json:"tool"`
	} `json:"block"`
	Message *struct {
		Status string `json:"status"`
	} `json:"message"`
}

type streamError struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
}

// pendingTool 是正在流式生成中的工具调用(按 block id 跟踪)。
type pendingTool struct {
	name string
	args strings.Builder
}

// ConsumeStream 消费 ChatService/Chat 的 Connect 服务端流响应。
//
// Kimi 帧格式(与请求一致):flags(1 字节) + 长度(4 字节大端) + JSON payload;
// 收尾帧 flags=2(END_OF_STREAM)。流内夹心跳 {"heartbeat":{}}。
// 内容为状态同步 op:set/append + mask:
//   - block.think / block.think.content → 思考(set 首段,append 增量)
//   - block.text / block.text.content   → 正文(set 首段,append 增量)
//   - block.tool → 原生工具调用:PENDING 起帧(name) → block.tool.args append 拼参数
//     → RUNNING 定稿(args 完整,工具 id 归一化)→ 上报 ToolCall
//
// 结束:message.status=COMPLETED / {"done":{}} / 收尾帧。
func ConsumeStream(r io.Reader, onDelta func(Delta)) StreamResult {
	var res StreamResult
	pending := make(map[string]*pendingTool)
	var lastReasoning, lastText string
	// 引用标记剥离(联网搜索时模型在正文里嵌入 🛠...🛠 引用占位符)
	var textStripper, reasoningStripper citationStripper

	header := make([]byte, 5)
	for {
		if _, err := io.ReadFull(r, header); err != nil {
			if err != io.EOF && err != io.ErrUnexpectedEOF {
				res.Err = "kimi stream: " + err.Error()
			}
			break
		}
		flags := header[0]
		length := binary.BigEndian.Uint32(header[1:5])
		if length == 0 {
			res.Finished = true
			break
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(r, payload); err != nil {
			res.Err = "kimi stream: truncated frame"
			break
		}
		var f streamFrame
		if err := json.Unmarshal(payload, &f); err != nil {
			continue // 无法解析的帧忽略
		}
		if f.Error != nil {
			if f.Error.Msg != "" {
				res.Err = f.Error.Msg
			} else if f.Error.Code != "" {
				res.Err = f.Error.Code
			}
		}
		if len(f.Done) > 0 && string(f.Done) != "null" {
			res.Finished = true
		}
		if f.Block != nil {
			if f.Block.Tool != nil {
				handleToolFrame(&f, pending, onDelta)
			}
			switch {
			case strings.Contains(f.Mask, "block.think.content") && f.Block.Think != nil:
				d := reasoningStripper.Feed(f.Block.Think.Content)
				if d == "" {
					break
				}
				res.Reasoning += d
				onDelta(Delta{Reasoning: d})
			case f.Mask == "block.think" && f.Block.Think != nil:
				// set 首段:新思考段(工具轮后可能再次出现)
				d := reasoningStripper.Feed(f.Block.Think.Content)
				if d == "" {
					break
				}
				if lastReasoning != "" {
					res.Reasoning += "\n"
					onDelta(Delta{Reasoning: "\n"})
				}
				res.Reasoning += d
				lastReasoning = d
				onDelta(Delta{Reasoning: d})
			case strings.Contains(f.Mask, "block.text.content") && f.Block.Text != nil:
				d := textStripper.Feed(f.Block.Text.Content)
				if d == "" {
					break
				}
				res.Text += d
				onDelta(Delta{Text: d})
			case f.Mask == "block.text" && f.Block.Text != nil:
				d := textStripper.Feed(f.Block.Text.Content)
				if d == "" {
					break
				}
				if lastText != "" {
					res.Text += "\n"
					onDelta(Delta{Text: "\n"})
				}
				res.Text += d
				lastText = d
				onDelta(Delta{Text: d})
			}
		}
		if f.Message != nil && f.Message.Status == "MESSAGE_STATUS_COMPLETED" {
			res.Finished = true
		}
		if flags&0x02 != 0 {
			// END_OF_STREAM 标志(收尾帧,payload 为 {} 或错误)
			res.Finished = true
			break
		}
	}
	// 流结束:冲刷剥离器遗留的干净文本(正常情况下为空)
	if tail := textStripper.Flush(); tail != "" {
		res.Text += tail
		onDelta(Delta{Text: tail})
	}
	if tail := reasoningStripper.Flush(); tail != "" {
		res.Reasoning += tail
		onDelta(Delta{Reasoning: tail})
	}
	if res.Text == "" && res.Reasoning == "" && res.Err == "" {
		res.Err = "kimi: empty stream"
	}
	return res
}

// handleToolFrame 处理 block.tool 相关帧:
//   - PENDING 起帧(mask 空):记录 name
//   - append block.tool.args:累积参数碎片
//   - set mask 含 block.tool.args(RUNNING 定稿):整单上报 ToolCall(args 完整)
func handleToolFrame(f *streamFrame, pending map[string]*pendingTool, onDelta func(Delta)) {
	t := f.Block.Tool
	// PENDING 起帧(mask 为空,status=PENDING)
	if f.Mask == "" && t.Status == "STATUS_PENDING" {
		pending[f.Block.ID] = &pendingTool{name: t.Name}
		return
	}
	pt, ok := pending[f.Block.ID]
	if !ok {
		return
	}
	if f.Op == "append" && strings.Contains(f.Mask, "block.tool.args") && t.Args != "" {
		pt.args.WriteString(t.Args)
		return
	}
	// RUNNING 定稿(set mask 含 block.tool.args):args 完整,直接覆盖累积值
	if f.Op == "set" && strings.Contains(f.Mask, "block.tool.args") {
		args := t.Args
		if args == "" {
			args = pt.args.String()
		}
		delete(pending, f.Block.ID)
		if pt.name == "" || args == "" {
			return
		}
		onDelta(Delta{ToolCall: &ToolCall{ID: t.ToolCallID, Name: pt.name, Arguments: args}})
	}
}
