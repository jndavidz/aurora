package deepseekweb

import (
	"strings"
	"testing"
)

// V3 格式:response/thinking_content + response/content + FINISHED。
func TestConsumeStreamV3(t *testing.T) {
	sse := `event: ready
data: {"request_message_id":"r1","response_message_id":"m1"}

event: message
data: {"p":"response/thinking_content","o":"REPLACE","v":"思考中"}

event: message
data: {"p":"response/content","o":"REPLACE","v":"你好，世界"}

event: message
data: {"p":"response/status","o":"REPLACE","v":"FINISHED"}
`
	var text, reasoning string
	res := ConsumeStream(strings.NewReader(sse), func(d Delta) {
		text += d.Text
		reasoning += d.Reasoning
	})
	if text != "你好，世界" {
		t.Errorf("text = %q", text)
	}
	if reasoning != "思考中" {
		t.Errorf("reasoning = %q", reasoning)
	}
	if !res.Finished {
		t.Error("expected finished")
	}
	if res.ResponseMsgID != "m1" {
		t.Errorf("response msg id = %q", res.ResponseMsgID)
	}
}

// V4 格式:初始快照(无 p,含 fragments)+ APPEND + delta。
func TestConsumeStreamV4(t *testing.T) {
	sse := `event: ready
data: {"request_message_id":"r1","response_message_id":"m1"}

event: message
data: {"v":{"response":{"fragments":[{"type":"RESPONSE","content":"你好"}]}}}

event: message
data: {"p":"response/fragments","o":"APPEND","v":[[{"type":"RESPONSE","content":"，世界"}]]}

event: message
data: {"p":"response/fragments/-1/content","o":"REPLACE","v":"！"}

event: message
data: {"p":"response/status","o":"REPLACE","v":"FINISHED"}
`
	var text string
	res := ConsumeStream(strings.NewReader(sse), func(d Delta) {
		text += d.Text
	})
	if text != "你好，世界！" {
		t.Errorf("text = %q", text)
	}
	if !res.Finished {
		t.Error("expected finished")
	}
}

// close / [DONE] 收尾。
func TestConsumeStreamCloseAndDone(t *testing.T) {
	for _, end := range []string{"event: close", "data: [DONE]"} {
		sse := `event: message
data: {"p":"response/content","o":"REPLACE","v":"hi"}

` + end + "\n"
		var text string
		res := ConsumeStream(strings.NewReader(sse), func(d Delta) { text += d.Text })
		if text != "hi" {
			t.Errorf("text = %q", text)
		}
		if !res.Finished {
			t.Errorf("expected finished for %q", end)
		}
	}
}

// hint 错误帧。
func TestConsumeStreamHint(t *testing.T) {
	sse := `event: hint
data: {"content":"rate_limit"}

event: close
`
	res := ConsumeStream(strings.NewReader(sse), func(d Delta) {})
	if res.Err != "rate_limit" {
		t.Errorf("err = %q, want rate_limit", res.Err)
	}
}

// 空流。
func TestConsumeStreamEmpty(t *testing.T) {
	res := ConsumeStream(strings.NewReader(""), func(d Delta) {})
	if res.Err != "empty stream" {
		t.Errorf("err = %q, want empty stream", res.Err)
	}
}
