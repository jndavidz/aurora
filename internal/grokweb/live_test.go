package grokweb

import (
	"fmt"
	"os"
	"testing"
)

// 直接走 grokweb 客户端测真实上游(手动运行,非 CI)。
// 用法:GROK_COOKIE="uid|cookie串" go test ./internal/grokweb/ -run TestLiveComplete -v
func TestLiveComplete(t *testing.T) {
	raw := os.Getenv("GROK_COOKIE")
	if raw == "" {
		t.Skip("GROK_COOKIE not set")
	}
	uid := "ab92569b-f731-4083-b486-9325a4602e37"
	cookie := raw
	if idx := indexByte(raw, '|'); idx >= 0 {
		uid = raw[:idx]
		cookie = raw[idx+1:]
	}
	c, err := NewClient("")
	if err != nil {
		t.Fatal(err)
	}
	c.AddAccount(uid, cookie)
	res := c.Complete(CompletionRequest{
		Prompt: "你好,用一句话介绍你自己",
	}, func(d Delta) {
		if d.Text != "" {
			fmt.Print(d.Text)
		}
	})
	fmt.Printf("\n=== finished=%v err=%q resp_id=%q\n", res.Finished, res.Err, res.ResponseID)
	if res.Err != "" && res.Text == "" {
		t.Fatalf("completion failed: %v", res.Err)
	}
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
