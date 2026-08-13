package qianwenweb

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
)

// Delta 是解析出的一帧增量。
type Delta struct {
	Text string // 正文增量(相对上一帧)
}

// StreamResult 是整条流的汇总。
type StreamResult struct {
	Text     string
	Finished bool
	Err      string
}

// streamFrame 是一帧 SSE data 载荷。
type streamFrame struct {
	Communication struct {
		Resid int `json:"resid"`
	} `json:"communication"`
	Data struct {
		Messages []struct {
			MimeType string `json:"mime_type"`
			Content  string `json:"content"`
			Status   string `json:"status"`
		} `json:"messages"`
		Status string `json:"status"`
	} `json:"data"`
	ErrorCode int    `json:"error_code"`
	ErrorMsg  string `json:"error_msg"`
	Success   bool   `json:"success"`
}

// ConsumeStream 消费 chat 的 SSE 响应。
//
// 千问 SSE 特点(与智谱同):每帧 data: 是完整 JSON,助手文本**全量重发**(非增量 patch),
// 最新一帧 data.messages 里 mime_type=="multi_load/iframe" 消息的 content 是当前累计全文。
// 因此:
//   - 取每帧最新文本 content,与上一帧做差值输出增量
//   - 结尾是 event:complete + data:true(或 data.status == "complete")
func ConsumeStream(r io.Reader, onDelta func(Delta)) StreamResult {
	var res StreamResult
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	var lastText string
	captcha := false
	for sc.Scan() {
		line := sc.Text()
		// WAF 人机验证兜底(头/TLS 指纹不符时服务器返回 captcha,而非 SSE)。
		// 两种形态:HTML 里含 "rgv587_flag:sm";JSON 里含 "FAIL_SYS_USER_VALIDATE"/"RGV587_ERROR"/x5sec。
		if isCaptchaPayload(line) {
			captcha = true
			break
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "true" { // 收尾帧 event:complete + data:true
			res.Finished = true
			continue
		}
		var fr streamFrame
		if err := json.Unmarshal([]byte(payload), &fr); err != nil {
			continue
		}
		if fr.ErrorMsg != "" {
			res.Err = fr.ErrorMsg
		}
		// 取最后一条文本消息的累计 content(全量重发,取最新一帧)
		var text string
		for _, m := range fr.Data.Messages {
			if m.MimeType == "multi_load/iframe" && m.Content != "" {
				text = m.Content
			}
		}
		// 差值输出增量
		if d := strings.TrimPrefix(text, lastText); d != "" {
			res.Text = text
			lastText = text
			onDelta(Delta{Text: d})
		}
		if fr.Data.Status == "complete" {
			res.Finished = true
		}
	}
	if res.Text == "" && res.Err == "" && !res.Finished {
		if captcha {
			res.Err = "qianwen WAF captcha (Origin/Referer/Accept/TLS fingerprint insufficient)"
		} else {
			res.Err = "empty stream"
		}
	}
	return res
}

// isCaptchaPayload 识别阿里 WAF 人机验证响应(HTML 与 JSON 两种形态)。
func isCaptchaPayload(s string) bool {
	low := strings.ToLower(s)
	for _, marker := range []string{"rgv587_flag", "rgv587_error", "fail_sys_user_validate", "x5sec", "punish"} {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}
