package yuanbaoweb

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
)

// Delta 是一次内容增量。元宝网页 SSE 只发文本增量(无 reasoning 帧,
// 深度思考未接入;meta 帧含 tokenUsage 但网关层不需要)。
type Delta struct {
	Text string
}

// StreamResult 是流消费汇总。
type StreamResult struct {
	Err string // 上游 error 帧信息(如 21007 回答拉取失败)
}

// frame 是 SSE 里 data: 行的 JSON 帧。
type frame struct {
	Type string `json:"type"`
	Msg  string `json:"msg"`
}

// ConsumeStream 消费元宝 /api/chat 的 SSE 流,把内容增量回调给 onDelta。
//
// 帧结构(实测,见 docs/YUANBAO.md §四):
//   - data: {"type":"text"}              开局哨兵(无 msg,跳过)
//   - data: {"type":"text","msg":"..."}  内容增量(逐帧小段,直接输出)
//   - event: speech_type / data: status|text  语音帧,忽略
//   - data: {"type":"tips",...}          联网搜索提示,忽略
//   - data: {"type":"meta",...}          元数据(stopReason/tokenUsage),忽略
//   - data: [plugin: ]/[MSGINDEX:n]/[TRACEID:...]  内部标记,忽略
//   - data: {"type":"error","msg":"...","code":"21007"}  错误帧
//   - data: [DONE]                       结束
func ConsumeStream(r io.Reader, onDelta func(Delta)) StreamResult {
	var res StreamResult
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			break
		}
		if !strings.HasPrefix(payload, "{") {
			// [plugin: ] / [MSGINDEX:n] / [TRACEID:...] 等内部标记,忽略。
			continue
		}
		var f frame
		if err := json.Unmarshal([]byte(payload), &f); err != nil {
			continue
		}
		switch f.Type {
		case "text":
			if f.Msg != "" {
				onDelta(Delta{Text: f.Msg})
			}
		case "error":
			if res.Err == "" {
				res.Err = f.Msg
			}
		}
	}
	if err := sc.Err(); err != nil {
		res.Err = err.Error()
	}
	return res
}
