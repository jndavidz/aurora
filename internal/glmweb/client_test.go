package glmweb

import (
	"sync"
	"os"
	"path/filepath"
	"testing"
)

// TestLoadTokens 验证 token 池文件加载(忽略空行/注释)。
func TestLoadTokens(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "glm_tokens.txt")
	content := "# comment\n\ntoken_alpha\n\ntoken_beta\n"
	if err := os.WriteFile(f, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	toks, err := loadTokens(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(toks) != 2 || toks[0] != "token_alpha" || toks[1] != "token_beta" {
		t.Fatalf("tokens = %v, want [token_alpha token_beta]", toks)
	}
}

// TestNewClientTokenPool 验证 NewClient 从文件加载池并取首个 token。
func TestNewClientTokenPool(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "tokens.txt")
	if err := os.WriteFile(f, []byte("t1\nt2\nt3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewClient("", f, "", "", "")
	if !c.HasToken() {
		t.Fatal("HasToken = false, want true")
	}
	if c.refreshToken != "t1" {
		t.Fatalf("initial refreshToken = %q, want t1", c.refreshToken)
	}
	if c.PoolSize() != 3 {
		t.Fatalf("PoolSize = %d, want 3", c.PoolSize())
	}
	// 轮询循环
	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		seen[c.NextToken()] = true
	}
	if len(seen) != 3 {
		t.Fatalf("NextToken cycle covered %v, want 3 distinct tokens", seen)
	}
	// SetRefreshToken 生效
	c.SetRefreshToken("t9")
	if c.refreshToken != "t9" {
		t.Fatalf("after SetRefreshToken = %q, want t9", c.refreshToken)
	}
}

// TestNewClientDirect 验证直传单个 token(无文件)时池大小为 1。
func TestNewClientDirect(t *testing.T) {
	c := NewClient("", "", "direct-token", "", "")
	if !c.HasToken() {
		t.Fatal("HasToken = false, want true")
	}
	if c.PoolSize() != 1 {
		t.Fatalf("PoolSize = %d, want 1", c.PoolSize())
	}
	if c.NextToken() != "direct-token" {
		t.Fatalf("NextToken = %q, want direct-token", c.NextToken())
	}
}

// TestNextTokenConcurrent 验证并发轮询无 data race(go test -race)且不丢轮次。
// 修复前 NextToken 的 cursor/tokens 无锁,handler 并发请求下为真实竞态
// (同 deepseekweb E3 修复;spec .scratch/arch-hardening WP3)。
func TestNextTokenConcurrent(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "tokens.txt")
	if err := os.WriteFile(f, []byte("t1\nt2\nt3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewClient("", f, "", "", "")
	var wg sync.WaitGroup
	const workers, iters = 16, 50
	seen := make(chan string, workers*iters)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				seen <- c.NextToken()
			}
		}()
	}
	wg.Wait()
	close(seen)
	pool := map[string]bool{"t1": true, "t2": true, "t3": true}
	n := 0
	for tok := range seen {
		if !pool[tok] {
			t.Fatalf("unexpected token %q", tok)
		}
		n++
	}
	if n != workers*iters {
		t.Fatalf("got %d tokens, want %d", n, workers*iters)
	}
	if c.PoolSize() != 3 {
		t.Fatalf("PoolSize = %d, want 3", c.PoolSize())
	}
}
