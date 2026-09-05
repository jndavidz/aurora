package provider

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"aurora/internal/config"
)

// E3(2026-09-05):minimax/mimo 凭证热加载 —— token 文件 mtime 变化后,
// webClient() 应重建 client 并读到新池(keeper scp 重推后进程内生效)。

func TestMinimaxHotReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.txt")
	if err := os.WriteFile(path, []byte("jwt-a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t1 := time.Now().Add(-2 * time.Second)
	if err := os.Chtimes(path, t1, t1); err != nil {
		t.Fatal(err)
	}

	d := NewMinimax(&config.Config{MinimaxWebTokens: path})

	c1 := d.webClient()
	if c1 == nil {
		t.Fatal("first webClient = nil")
	}
	// mtime 未变化:复用缓存 client
	if c2 := d.webClient(); c2 != c1 {
		t.Fatal("client should be cached before mtime change")
	}

	// 换全新 token 并触碰 mtime → 重建且新池生效
	if err := os.WriteFile(path, []byte("jwt-z\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t2 := time.Now()
	if err := os.Chtimes(path, t2, t2); err != nil {
		t.Fatal(err)
	}
	c3 := d.webClient()
	if c3 == nil || c3 == c1 {
		t.Fatal("client should be rebuilt after mtime change")
	}
	if got := c3.NextToken(); got != "jwt-z" {
		t.Fatalf("after reload token = %q, want jwt-z(新池已生效)", got)
	}
}

func TestMimoHotReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.txt")
	if err := os.WriteFile(path, []byte("ph-a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t1 := time.Now().Add(-2 * time.Second)
	if err := os.Chtimes(path, t1, t1); err != nil {
		t.Fatal(err)
	}

	d := NewMimo(&config.Config{MimoWebTokens: path})

	c1 := d.webClient()
	if c1 == nil {
		t.Fatal("first webClient = nil")
	}
	if c2 := d.webClient(); c2 != c1 {
		t.Fatal("client should be cached before mtime change")
	}

	if err := os.WriteFile(path, []byte("ph-z\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t2 := time.Now()
	if err := os.Chtimes(path, t2, t2); err != nil {
		t.Fatal(err)
	}
	c3 := d.webClient()
	if c3 == nil || c3 == c1 {
		t.Fatal("client should be rebuilt after mtime change")
	}
	if got := c3.NextToken(); got != "ph-z" {
		t.Fatalf("after reload token = %q, want ph-z(新池已生效)", got)
	}
}
