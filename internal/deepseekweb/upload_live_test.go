package deepseekweb

import (
	"encoding/base64"
	"os"
	"testing"
)

// 手动测试:真实上传 red64.png(手动运行,非 CI)。
func TestLiveUpload(t *testing.T) {
	token := os.Getenv("DS_TEST_TOKEN")
	if token == "" {
		t.Skip("DS_TEST_TOKEN not set")
	}
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
	c, _ := NewClient("", "", "")
	id, err := c.UploadFile(token, "test.png", "image/png", png)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	t.Logf("file_id=%s", id)
	status, err := c.FetchFiles(token, id)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	t.Logf("status=%s", status)
}
