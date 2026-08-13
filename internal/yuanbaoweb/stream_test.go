package yuanbaoweb

import (
	"strings"
	"testing"
)

// frame 拼一条 SSE 帧(带空行分隔,与网页一致)。
func sseFrame(lines ...string) string {
	return strings.Join(lines, "\n") + "\n\n"
}

// 真实帧结构(2026-08-13 抓包):text 增量 / speech_type / tips / 内部标记 / meta / [DONE]。
func TestConsumeStreamHappyPath(t *testing.T) {
	sse := sseFrame(`data: {"type":"text"}`) +
		sseFrame(`event: speech_type`, `data: status`) +
		sseFrame(`event: speech_type`, `data: text`) +
		sseFrame(`data: {"type":"text","msg":"我是"}`) +
		sseFrame(`data: {"type":"text","msg":"元宝"}`) +
		sseFrame(`data: {"type":"tips","status":0,"internetFlag":0,"targetFunctionId":"autoInternetSearch"}`) +
		sseFrame(`data: [plugin: ]`) +
		sseFrame(`data: [MSGINDEX:10]`) +
		sseFrame(`data: {"type":"meta","messageId":"x","stopReason":"stop","endConv":false}`) +
		sseFrame(`data: [TRACEID:abc]`) +
		sseFrame(`data: [DONE]`)

	var got []string
	res := ConsumeStream(strings.NewReader(sse), func(d Delta) { got = append(got, d.Text) })
	if res.Err != "" {
		t.Fatalf("Err = %q, want empty", res.Err)
	}
	if len(got) != 2 || got[0] != "我是" || got[1] != "元宝" {
		t.Fatalf("deltas = %v, want [我是 元宝]", got)
	}
}

// 开局哨兵 {"type":"text"} 无 msg,不产生 delta。
func TestConsumeStreamSkipsSentinel(t *testing.T) {
	sse := sseFrame(`data: {"type":"text"}`) + sseFrame(`data: [DONE]`)
	var got []string
	ConsumeStream(strings.NewReader(sse), func(d Delta) { got = append(got, d.Text) })
	if len(got) != 0 {
		t.Fatalf("sentinel should not produce delta: %v", got)
	}
}

// error 帧:错误信息进入 StreamResult.Err。
func TestConsumeStreamErrorFrame(t *testing.T) {
	sse := sseFrame(`event: error`, `data: {"type":"error","msg":"回答拉取失败，正在重试","code":"21007","error":{}}`)
	res := ConsumeStream(strings.NewReader(sse), func(d Delta) {})
	if !strings.Contains(res.Err, "回答拉取失败") {
		t.Fatalf("Err = %q, want contain 回答拉取失败", res.Err)
	}
}

// [DONE] 提前截断:之后的帧不处理。
func TestConsumeStreamStopsAtDone(t *testing.T) {
	sse := sseFrame(`data: {"type":"text","msg":"前面"}`) +
		sseFrame(`data: [DONE]`) +
		sseFrame(`data: {"type":"text","msg":"后面"}`)
	var got []string
	ConsumeStream(strings.NewReader(sse), func(d Delta) { got = append(got, d.Text) })
	if len(got) != 1 || got[0] != "前面" {
		t.Fatalf("deltas = %v, want [前面]", got)
	}
}

// 非 data 行(如 event:)不解析,不 panic。
func TestConsumeStreamIgnoresEventLines(t *testing.T) {
	sse := sseFrame(`event: speech_type`, `data: status`) + sseFrame(`data: [DONE]`)
	res := ConsumeStream(strings.NewReader(sse), func(d Delta) {})
	if res.Err != "" {
		t.Fatalf("Err = %q", res.Err)
	}
}
