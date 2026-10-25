package qianwenweb

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestParseCookies(t *testing.T) {
	h := "tongyi_sso_ticket=abc123; x5sec=7b227d; x5sectag=550566; sm_ruid=a|||b"
	cks := parseCookies(h)
	if len(cks) != 4 {
		t.Fatalf("cookies = %d, want 4: %+v", len(cks), cks)
	}
	if cks[0].Name != "tongyi_sso_ticket" || cks[0].Value != "abc123" {
		t.Errorf("cookie[0] = %+v", cks[0])
	}
	if cks[2].Name != "x5sectag" || cks[2].Value != "550566" {
		t.Errorf("cookie[2] = %+v", cks[2])
	}
	if cks[3].Name != "sm_ruid" || cks[3].Value != "a|||b" {
		t.Errorf("cookie[3] = %+v (value contains |||)", cks[3])
	}
	// 空与异常输入
	if got := parseCookies(""); len(got) != 0 {
		t.Errorf("empty header should yield 0 cookies, got %d", len(got))
	}
	if got := parseCookies("nohashvalue"); len(got) != 0 {
		t.Errorf("malformed part should be skipped, got %d", len(got))
	}
}

// TestNextTokenConcurrent 验证并发轮询/轮换无 data race(go test -race)。
// 修复前 qianwen 的 cookies/tokens/cursor 完全无锁(spec .scratch/arch-hardening WP3)。
func TestNextTokenConcurrent(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "tokens.txt")
	if err := os.WriteFile(f, []byte("t1\nt2\nt3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewClient("", f, "", "")
	var wg sync.WaitGroup
	const workers, iters = 16, 50
	seen := make(chan string, workers*iters)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				seen <- c.NextToken()
				c.SetCookieHeader("t1") // 与轮换写入并发,-race 验证同锁
			}
		}()
	}
	wg.Wait()
	close(seen)
	pool := map[string]bool{"t1": true, "t2": true, "t3": true}
	for tok := range seen {
		if !pool[tok] {
			t.Fatalf("unexpected token %q", tok)
		}
	}
}
