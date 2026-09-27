package deepseekweb

import (
	"os"
	"path/filepath"
	"testing"
)

// A 项拍板(2026-09-28):池路径必须按 clientKey 确定性取 token。
// 上游 session 与建立时的 token 绑定;NextToken 轮询在多 token 池下
// 连续两次必不同,会令 Pool.Acquire 的 token 比对恒不命中(负收益)。
func TestTokenForDeterministicPerKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.txt")
	if err := os.WriteFile(path, []byte("tokA\ntokB\ntokC\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := NewClient("", path, "")
	if err != nil {
		t.Fatal(err)
	}
	// 同 key 多次取值必须一致(跨 reloadIfChanged 调用亦然)。
	want := c.TokenFor("u1")
	if want == "" {
		t.Fatal("TokenFor 返回空")
	}
	for i := 0; i < 10; i++ {
		if got := c.TokenFor("u1"); got != want {
			t.Fatalf("同 clientKey 取值应稳定: %q vs %q", got, want)
		}
	}
	// 不同 key 应落同池内某个合法 token(不要求互异,但必须可命中池)。
	for _, k := range []string{"u2", "u3", "u4"} {
		tok := c.TokenFor(k)
		if tok != "tokA" && tok != "tokB" && tok != "tokC" {
			t.Fatalf("key %q 取到池外 token %q", k, tok)
		}
	}
}

func TestTokenForEmptyKeyFallsBackToRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.txt")
	if err := os.WriteFile(path, []byte("tokA\ntokB\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := NewClient("", path, "")
	if err != nil {
		t.Fatal(err)
	}
	// 空 key(单发不进池)回落轮询:连续取值应交替(负载均衡保持)。
	first := c.TokenFor("")
	second := c.TokenFor("")
	if first == "" || second == "" {
		t.Fatalf("轮询取值不应为空: %q / %q", first, second)
	}
	if first == second {
		t.Fatalf("空 key 应走轮询(两连取应不同), got %q 两次", first)
	}
}

func TestTokenForEmptyPool(t *testing.T) {
	c, err := NewClient("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.TokenFor("u1"); got != "" {
		t.Fatalf("空池应返回空串, got %q", got)
	}
}
