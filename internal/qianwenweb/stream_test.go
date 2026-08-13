package qianwenweb

import (
	"strconv"
	"strings"
	"testing"
)

// 模拟一帧 SSE(千问全量重发格式)。
func frame(resid int, status, text string) string {
	msg := `{"meta_data":{"operation_types":[["regenerate"],["enable_web_search"]]},"mime_type":"signal/post","status":"complete"}`
	bar := `{"meta_data":{"elements":[{"type":"text","content":""}],"type":"bar_update"},"mime_type":"bar/progress","status":"complete"}`
	var txt string
	if text != "" {
		txt = `,{"content":"` + text + `","meta_data":{},"mime_type":"multi_load/iframe","status":"processing"}`
	}
	if status == "complete" {
		// 收尾帧:文本消息也 complete
		txt = `,{"content":"` + text + `","meta_data":{},"mime_type":"multi_load/iframe","status":"complete"}`
	}
	payload := `{"communication":{"chat_assistant_name":"MainChatAgent","reqid":"r1","resid":` + strconv.Itoa(resid) + `},"data":{"messages":[` + msg + `,` + bar + txt + `],"status":"` + status + `"},"error_code":0,"error_msg":"","success":true}`
	return "event:message\ndata:" + payload + "\n\n"
}

func TestConsumeStreamFullResendDeltas(t *testing.T) {
	var sse strings.Builder
	sse.WriteString(frame(0, "processing", ""))
	sse.WriteString(frame(1, "processing", "你好"))
	sse.WriteString(frame(2, "processing", "你好,世界"))
	sse.WriteString(frame(3, "complete", "你好,世界!"))
	sse.WriteString("event:complete\ndata:true\n\n")

	var deltas []string
	res := ConsumeStream(strings.NewReader(sse.String()), func(d Delta) {
		if d.Text != "" {
			deltas = append(deltas, d.Text)
		}
	})
	if res.Err != "" {
		t.Fatalf("unexpected err: %s", res.Err)
	}
	want := []string{"你好", ",世界", "!"}
	if len(deltas) != len(want) {
		t.Fatalf("deltas = %v, want %v", deltas, want)
	}
	for i := range want {
		if deltas[i] != want[i] {
			t.Errorf("delta[%d] = %q, want %q", i, deltas[i], want[i])
		}
	}
	if !res.Finished {
		t.Error("stream should be finished")
	}
	if res.Text != "你好,世界!" {
		t.Errorf("res.Text = %q, want 你好,世界!", res.Text)
	}
}

func TestConsumeStreamErrorFrame(t *testing.T) {
	sse := "event:message\ndata:" + `{"communication":{"resid":0},"data":{"messages":[],"status":"processing"},"error_code":1001,"error_msg":"签名错误","success":false}` + "\n\n"
	res := ConsumeStream(strings.NewReader(sse), func(Delta) {})
	if res.Err == "" {
		t.Error("expected error from error_msg")
	}
}

func TestConsumeStreamCaptcha(t *testing.T) {
	// HTML 形态
	sse := `<script>window._config_ = {"action":"captcha"};</script><!--rgv587_flag:sm-->`
	res := ConsumeStream(strings.NewReader(sse), func(Delta) {})
	if res.Err == "" || !strings.Contains(res.Err, "WAF") {
		t.Errorf("expected WAF captcha error, got %q", res.Err)
	}
	// JSON 形态(FAIL_SYS_USER_VALIDATE / x5sec)
	jsonCap := `{"ret":["FAIL_SYS_USER_VALIDATE","RGV587_ERROR::SM::哎哟喂"],"data":{"url":".../punish?x5secdata=..."}}`
	res2 := ConsumeStream(strings.NewReader(jsonCap), func(Delta) {})
	if res2.Err == "" || !strings.Contains(res2.Err, "WAF") {
		t.Errorf("expected WAF captcha error for JSON variant, got %q", res2.Err)
	}
}

func TestConsumeStreamEmpty(t *testing.T) {
	res := ConsumeStream(strings.NewReader(""), func(Delta) {})
	if res.Err == "" || !strings.Contains(res.Err, "empty stream") {
		t.Errorf("expected empty stream error, got %q", res.Err)
	}
}
