package geminweb

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"
)

// 验证 wrb.fr 帧解析:文本提取 + rcid + 结束标记。
func TestParseRPCFrame(t *testing.T) {
	// 流式帧1:开始(含 rcid)。payload 是 JSON 字符串(需转义内层引号)。
	payload1 := `[null,["c_abc","r_1"],null,null,[["rc_x",["你"],null,null,null,null,null,null,[1],"zh",null,null,[],null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,[],null,null,null,null,null,null,null,null,[]]]]`
	f1 := `[["wrb.fr",null,` + strconvQuote(payload1) + `],["di",1]]`
	text, rcid, done := parseRPCFrame(f1)
	if text != "你" {
		t.Errorf("f1 text = %q, want 你", text)
	}
	if rcid != "rc_x" {
		t.Errorf("f1 rcid = %q, want rc_x", rcid)
	}
	if done {
		t.Error("f1 done = true, want false")
	}

	// 流式帧2:追加
	payload2 := `[null,["c_abc","r_1"],null,null,[["rc_x",["你好"],null,null,null,null,null,null,[1],"zh",null,null,[],null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,[],null,null,null,null,null,null,null,null,[]]]]`
	f2 := `[["wrb.fr",null,` + strconvQuote(payload2) + `],["di",1]]`
	text2, _, _ := parseRPCFrame(f2)
	if text2 != "你好" {
		t.Errorf("f2 text = %q, want 你好", text2)
	}

	// 结束帧
	payload3 := `[null,["c_abc","r_1"],{"44":true,"46":["c_abc",""],"52":[]}]`
	f3 := `[["wrb.fr",null,` + strconvQuote(payload3) + `],["di",1]]`
	_, _, done3 := parseRPCFrame(f3)
	if !done3 {
		t.Error("f3 done = false, want true")
	}
}

// strconvQuote 生成 JSON 字符串(转义内层引号)。
func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// 限频:单账号两次请求间隔 >= minInterval。
func TestRateLimit(t *testing.T) {
	acct := &Account{Cookie: "c", At: "a", SNlM6e: "s", Fsid: "f.sid=1"}
	c := NewClient([]*Account{acct})
	start := time.Now()
	a1, _, err := c.nextAccount()
	if err != nil || a1 == nil {
		t.Fatal("nextAccount 1:", err)
	}
	time.Sleep(100 * time.Millisecond)
	a2, _, err := c.nextAccount()
	if err != nil || a2 == nil {
		t.Fatal("nextAccount 2:", err)
	}
	elapsed := time.Since(start)
	if elapsed < minInterval {
		t.Errorf("second call too fast: %v < %v", elapsed, minInterval)
	}
}

// buildReqBody:含 SNlM6e + at,结构对齐抓包。
func TestBuildReqBody(t *testing.T) {
	acct := &Account{Cookie: "cookie", At: "at:123", SNlM6e: "snl6e", Fsid: "f.sid=42"}
	c := NewClient([]*Account{acct})
	body, err := c.buildReqBody(acct, CompletionRequest{Prompt: "你好"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if !strings.Contains(s, "f.req=") || !strings.Contains(s, "at=at%3A123") {
		t.Errorf("body missing f.req/at: %s", s)
	}
	if !strings.Contains(s, "snl6e") {
		t.Error("body missing SNlM6e")
	}
	// 中文被 url.Values.Encode 百分号编码
	if !strings.Contains(s, "%E4%BD%A0%E5%A5%BD") {
		t.Error("body missing prompt(encoded)")
	}
	// 解码后应含原文
	unesc, _ := url.QueryUnescape(s)
	if !strings.Contains(unesc, "你好") {
		t.Errorf("decoded body missing prompt: %s", unesc)
	}
}

// streamURL:含 f.sid。
func TestStreamURL(t *testing.T) {
	acct := &Account{Fsid: "f.sid=-555"}
	c := NewClient([]*Account{acct})
	u := c.streamURL(acct)
	if !strings.Contains(u, "f.sid=-555") {
		t.Errorf("streamURL missing f.sid: %s", u)
	}
	if !strings.Contains(u, "StreamGenerate") {
		t.Errorf("streamURL wrong: %s", u)
	}
}

// sanitizeText:剥离 googleusercontent card_content 引用占位符。
func TestSanitizeText(t *testing.T) {
	in := "http://googleusercontent.com/card_content/0\n根据天气数据,东京多云。\nhttp://googleusercontent.com/card_content/1\n明日有雨。"
	got := sanitizeText(in)
	if strings.Contains(got, "googleusercontent") {
		t.Errorf("sanitize 未剥离引用链接: %q", got)
	}
	if !strings.Contains(got, "根据天气数据") || !strings.Contains(got, "明日有雨") {
		t.Errorf("sanitize 误删正文: %q", got)
	}
	// 无引用时原样
	plain := "普通回复"
	if sanitizeText(plain) != plain {
		t.Errorf("sanitize 不应改普通文本: %q", sanitizeText(plain))
	}
}
