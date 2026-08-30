package deepseekweb

import (
	"encoding/hex"
	"testing"
)

// 官方测试向量(取自 ai-wen/ds2api,与 DeepSeek 官方 WASM 一致):
//
//	hash("testsalt_1700000000_42") == d4a2ea58c89e40887c933484868380c6f803eaa8dc53a3b9df8e431b921a4f09
func TestDeepSeekHashV1Vector(t *testing.T) {
	got := hashDeepSeekV1([]byte("testsalt_1700000000_42"))
	want := "d4a2ea58c89e40887c933484868380c6f803eaa8dc53a3b9df8e431b921a4f09"
	if hex.EncodeToString(got) != want {
		t.Fatalf("hash mismatch:\n got %x\nwant %s", got, want)
	}
}

// 空串向量:hash("") == e594808bc5b7151ac160c6d39a02e0a8e261ed588578403099e3561dc40c26b3
func TestDeepSeekHashV1Empty(t *testing.T) {
	got := hashDeepSeekV1(nil)
	want := "e594808bc5b7151ac160c6d39a02e0a8e261ed588578403099e3561dc40c26b3"
	if hex.EncodeToString(got) != want {
		t.Fatalf("hash mismatch:\n got %x\nwant %s", got, want)
	}
}

// 构造一个可解的 challenge 并验证 SolvePow 闭环。
func TestSolvePow(t *testing.T) {
	// 先用小 difficulty 构造:目标 = hash(salt_expireAt_42),应解出 nonce=42。
	salt := "abc"
	expire := int64(1700000000)
	nonce := int64(42)
	target := hex.EncodeToString(hashDeepSeekV1([]byte(salt + "_1700000000_42")))

	ch := &powChallenge{
		Algorithm:  "DeepSeekHashV1",
		Challenge:  target,
		Salt:       salt,
		Signature:  "sig",
		Difficulty: 1000000,
		ExpireAt:   expire,
	}
	header, solved, err := SolvePow(ch)
	if err != nil {
		t.Fatalf("SolvePow: %v", err)
	}
	if solved != nonce {
		t.Fatalf("solved nonce = %d, want 42", solved)
	}
	if header == "" {
		t.Fatal("empty header")
	}
}
