package poolfile

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestReplaceTokenBasic(t *testing.T) {
	path := t.TempDir() + "/pool.txt"
	write(t, path, "old-token\nsecond-token\n")

	replaced, err := ReplaceToken(path, "old-token", "new-token", "test")
	if err != nil || !replaced {
		t.Fatalf("replaced=%v err=%v", replaced, err)
	}
	got := read(t, path)
	if got != "new-token\nsecond-token\n" {
		t.Fatalf("got %q", got)
	}
}

// 幂等:newToken 已在文件中时不重复追加。
func TestReplaceTokenIdempotent(t *testing.T) {
	path := t.TempDir() + "/pool.txt"
	write(t, path, "same-token\n")

	replaced, err := ReplaceToken(path, "same-token", "same-token", "test")
	if err != nil || !replaced {
		t.Fatalf("replaced=%v err=%v", replaced, err)
	}
	if got := read(t, path); strings.Count(got, "same-token") != 1 {
		t.Fatalf("不应重复追加, got %q", got)
	}
}

// 未命中则追加(旧实现语义保留)。
func TestReplaceTokenAppend(t *testing.T) {
	path := t.TempDir() + "/pool.txt"
	write(t, path, "other\n")

	replaced, err := ReplaceToken(path, "missing-old", "new-token", "test")
	if err != nil || replaced {
		t.Fatalf("replaced=%v err=%v", replaced, err)
	}
	if got := read(t, path); got != "other\nnew-token\n" {
		t.Fatalf("got %q", got)
	}
}

// 空行/# 注释行保留;CRLF 归一化。
func TestReplaceTokenPreservesFormat(t *testing.T) {
	path := t.TempDir() + "/pool.txt"
	write(t, path, "# 注释\r\n\r\nold-token\r\nsecond\r\n")

	if _, err := ReplaceToken(path, "old-token", "new-token", "test"); err != nil {
		t.Fatal(err)
	}
	got := read(t, path)
	if !strings.HasPrefix(got, "# 注释\n\nnew-token\nsecond\n") {
		t.Fatalf("格式未保留, got %q", got)
	}
}

// 锁竞争:预置 .lock → 3s 超时报错,池文件不被改动。
func TestReplaceTokenLockContention(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/pool.txt"
	write(t, path, "old-token\n")
	write(t, path+".lock", "held by someone")

	start := time.Now()
	if _, err := ReplaceToken(path, "old-token", "new-token", "test"); err == nil {
		t.Fatal("锁被持有时应报错")
	}
	if el := time.Since(start); el < 2500*time.Millisecond {
		t.Fatalf("应等待约 3s, 实际 %v", el)
	}
	if got := read(t, path); got != "old-token\n" {
		t.Fatalf("锁竞争时池文件不应被改, got %q", got)
	}
	if _, err := os.Stat(path + ".tmp.test." + strconv.Itoa(os.Getpid())); err == nil {
		t.Fatal("失败的回写不应残留 tmp 文件")
	}
}

// 陈锁抢占:锁文件 mtime 超 30s → 视为崩溃残留,删除并成功回写。
func TestReplaceTokenStaleLock(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/pool.txt"
	write(t, path, "old-token\n")
	lockPath := path + ".lock"
	write(t, lockPath, "crashed holder")
	stale := time.Now().Add(-time.Hour)
	if err := os.Chtimes(lockPath, stale, stale); err != nil {
		t.Fatal(err)
	}

	replaced, err := ReplaceToken(path, "old-token", "new-token", "test")
	if err != nil || !replaced {
		t.Fatalf("陈锁应被抢占: replaced=%v err=%v", replaced, err)
	}
	if got := read(t, path); got != "new-token\n" {
		t.Fatalf("got %q", got)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatal("回写完成后锁文件应已释放")
	}
}
