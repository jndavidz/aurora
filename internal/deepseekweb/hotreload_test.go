package deepseekweb

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// E3(2026-09-05):凭证热加载 —— token 文件 mtime 变化后,NextToken 应
// 无需重建客户端即读到新池;读失败沿用旧池(keep-last-good)。

// 基础热加载:改文件 → NextToken 立即反映新池。
func TestHotReloadOnMtimeChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.txt")
	if err := os.WriteFile(path, []byte("tok-a\ntok-b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 强制 mtime 与未来写入可区分(部分文件系统 mtime 粒度粗)
	t1 := time.Now().Add(-2 * time.Second)
	if err := os.Chtimes(path, t1, t1); err != nil {
		t.Fatal(err)
	}

	c, err := NewClient("", path, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.NextToken(); got != "tok-a" {
		t.Fatalf("first token = %q, want tok-a", got)
	}

	// 换成全新内容并触碰 mtime:热加载后第一个 token 必须来自新池。
	// (reload 会重置 cursor,用全新 token 前缀避免与旧池序列歧义)
	if err := os.WriteFile(path, []byte("tok-x1\ntok-x2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t2 := time.Now()
	if err := os.Chtimes(path, t2, t2); err != nil {
		t.Fatal(err)
	}

	if got := c.NextToken(); got != "tok-x1" {
		t.Fatalf("after reload token = %q, want tok-x1(新池已生效)", got)
	}
}

// keep-last-good:mtime 变了但文件读出空内容 → 沿用旧池,不缩容为空。
func TestHotReloadKeepsLastGoodOnEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.txt")
	if err := os.WriteFile(path, []byte("tok-a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t1 := time.Now().Add(-2 * time.Second)
	if err := os.Chtimes(path, t1, t1); err != nil {
		t.Fatal(err)
	}

	c, err := NewClient("", path, "")
	if err != nil {
		t.Fatal(err)
	}

	// 空文件 + mtime 触碰:重读被拒绝,旧池保留
	if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	t2 := time.Now()
	if err := os.Chtimes(path, t2, t2); err != nil {
		t.Fatal(err)
	}
	if got := c.NextToken(); got != "tok-a" {
		t.Fatalf("token = %q, want tok-a(空文件不应清空旧池)", got)
	}
}

// 并发安全:多 goroutine 并发 NextToken + 触碰文件,不产生数据竞争
// (go test -race 下验证;cursor 旧实现无锁)。
func TestHotReloadConcurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.txt")
	if err := os.WriteFile(path, []byte("tok-a\ntok-b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := NewClient("", path, "")
	if err != nil {
		t.Fatal(err)
	}
	// close(done) 广播停止(time.After 只投递一个值,只能放行一个 goroutine)
	done := make(chan struct{})
	go func() {
		time.Sleep(300 * time.Millisecond)
		close(done)
	}()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				if n == 0 {
					// 写者:轮换文件内容触碰 mtime
					content := "tok-a\ntok-b\n"
					if time.Now().UnixNano()%2 == 0 {
						content = "tok-c\ntok-d\n"
					}
					_ = os.WriteFile(path, []byte(content), 0o600)
					time.Sleep(5 * time.Millisecond)
				} else {
					_ = c.NextToken()
				}
			}
		}(i)
	}
	wg.Wait()
}
