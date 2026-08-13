package geminweb

import (
	"fmt"
	"os"
	"testing"
)

// 直接走 geminweb 客户端测真实上游(手动运行,非 CI)。
// 用法:GEMINI_ACCOUNT_FILE=<accounts.json> go test ./internal/geminweb/ -run TestLiveComplete -v
// accounts.json 格式见 docs/GEMINI.md §六。
func TestLiveComplete(t *testing.T) {
	path := os.Getenv("GEMINI_ACCOUNT_FILE")
	if path == "" {
		t.Skip("GEMINI_ACCOUNT_FILE not set")
	}
	accounts, err := LoadAccounts(path)
	if err != nil || len(accounts) == 0 {
		t.Fatalf("load accounts: %v", err)
	}
	c := NewClient(accounts)
	res := c.Complete(CompletionRequest{
		Prompt: "用一句话介绍你自己",
	}, func(d Delta) {
		if d.Text != "" {
			fmt.Print(d.Text)
		}
	})
	fmt.Printf("\n=== done=%v err=%q rcid=%q\n", res.Done, res.Err, res.RCID)
	if res.Err != "" && res.Text == "" {
		t.Fatalf("completion failed: %v", res.Err)
	}
	if res.Text == "" {
		t.Fatal("empty response")
	}
}
