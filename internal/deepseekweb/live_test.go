package deepseekweb

import (
	"fmt"
	"os"
	"testing"
)

// 直接走 deepseekweb 客户端测真实上游(手动运行,非 CI)。
func TestLiveCompletion(t *testing.T) {
	token := os.Getenv("DS_TEST_TOKEN")
	if token == "" {
		t.Skip("DS_TEST_TOKEN not set")
	}
	c, err := NewClient("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	sid, err := c.CreateSession(token)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	defer c.DeleteSession(token, sid)
	resp, err := c.Complete(token, CompletionRequest{
		SessionID:       sid,
		Prompt:          "你好",
		ModelType:       "default",
		ThinkingEnabled: false,
		SearchEnabled:   true,
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	defer resp.Body.Close()
	var text string
	res := ConsumeStream(resp.Body, func(d Delta) { text += d.Text })
	fmt.Printf("REPLY: %q\nERR: %q FINISHED: %v\n", text, res.Err, res.Finished)
}
