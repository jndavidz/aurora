package deepseekweb

import (
	"encoding/base64"
	"os"
	"testing"
)

// 手动测试:完整识图流程 upload→fetch→fork→completion(model_type=vision)。
func TestLiveVisionFullFlow(t *testing.T) {
	token := os.Getenv("DS_TEST_TOKEN")
	if token == "" {
		t.Skip("DS_TEST_TOKEN not set")
	}
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
	c, _ := NewClient("", "", "")
	sid, _ := c.CreateSession(token)
	defer c.DeleteSession(token, sid)
	id, err := c.UploadFile(token, "test.png", "image/png", png)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	t.Logf("uploaded=%s", id)
	for i := 0; i < 6; i++ {
		if st, _ := c.FetchFiles(token, id); st == "READY" {
			break
		}
		_ = i
	}
	vid, err := c.ForkFileToVision(token, id)
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	t.Logf("vision file=%s", vid)
	r, err := c.Complete(token, CompletionRequest{
		SessionID:     sid,
		Prompt:        "这张图是什么颜色",
		ModelType:     "vision",
		SearchEnabled: true,
		RefFileIDs:    []string{vid},
	})
	if err != nil {
		t.Fatalf("vision complete: %v", err)
	}
	var text string
	res := ConsumeStream(r.Body, func(d Delta) { text += d.Text })
	t.Logf("vision text=%q err=%q", text, res.Err)
}
