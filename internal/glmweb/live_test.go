package glmweb

import (
	"fmt"
	"os"
	"testing"
)

// 直接走 glmweb 客户端测真实上游(手动运行,非 CI)。
// 用法:GLM_TEST_TOKEN=<refresh_token> go test ./internal/glmweb/ -run TestLiveRefresh -v
func TestLiveRefresh(t *testing.T) {
	rt := os.Getenv("GLM_TEST_TOKEN")
	if rt == "" {
		t.Skip("GLM_TEST_TOKEN not set")
	}
	c := NewClient("", "", rt, "", "")
	if err := c.RefreshAccessToken(); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if !c.HasAccessToken() {
		t.Fatal("no access token after refresh")
	}
	fmt.Printf("ACCESS_TOKEN len=%d\n", len(c.accessToken))
}

// 完整 completion 冒烟:换发 token → 发起对话 → 收 SSE。
// 用法:GLM_TEST_TOKEN=<refresh_token> go test ./internal/glmweb/ -run TestLiveCompletion -v
func TestLiveCompletion(t *testing.T) {
	rt := os.Getenv("GLM_TEST_TOKEN")
	if rt == "" {
		t.Skip("GLM_TEST_TOKEN not set")
	}
	c := NewClient("", "", rt, "", "")
	if err := c.RefreshAccessToken(); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	resp, err := c.Complete(CompletionRequest{
		Messages: []Message{
			{Role: "user", Content: []Content{{Type: "text", Text: "你好,用一句话介绍你自己"}}},
		},
		ChatMode:     "speed",
		IsNetworking: false,
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	defer resp.Body.Close()
	var text, reasoning string
	res := ConsumeStream(resp.Body, func(d Delta) {
		text += d.Text
		reasoning += d.Reasoning
	})
	fmt.Printf("REPLY: %q\nREASONING: %q\nERR: %q FINISHED: %v CONV_ID: %s\n",
		text, reasoning, res.Err, res.Finished, res.ConversationID)
	if res.Err != "" && text == "" {
		t.Fatalf("completion failed: %v", res.Err)
	}
}
