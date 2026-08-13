package glmweb

import (
	"crypto/md5"
	"encoding/hex"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// TestObfuscatedTimestamp 验证混淆算法:中间位(倒数第二位)被替换为
// (各位数字之和 - 倒数第二位) % 10。
func TestObfuscatedTimestamp(t *testing.T) {
	cases := []struct {
		ms int64
	}{
		{time.Now().UnixMilli()},
		{1755000000000}, // 固定值,便于手工复算
		{1700000000000},
		{1000000000000},
	}
	for _, c := range cases {
		got := obfuscatedTimestamp(c.ms)
		if len(got) != 13 {
			t.Fatalf("obfuscatedTimestamp(%d) = %q, want len 13", c.ms, got)
		}
		// 除倒数第二位外其余位应与原时间戳一致
		orig := strconv.FormatInt(c.ms, 10)
		if got[:11] != orig[:11] || got[12:] != orig[12:] {
			t.Fatalf("obfuscatedTimestamp(%d) = %q, orig %q: only middle digit should change", c.ms, got, orig)
		}
		// 校验位 = (digit sum - second-last digit) % 10
		sum := 0
		for _, ch := range orig {
			sum += int(ch - '0')
		}
		want := (sum - int(orig[11]-'0')) % 10
		if got[11] != byte('0'+want) {
			t.Fatalf("obfuscatedTimestamp(%d) = %q, middle digit want %d", c.ms, got, want)
		}
	}
}

// TestSignFormat 验证签名长度与格式(MD5 hex, 32 字符)。
func TestSignFormat(t *testing.T) {
	sg := NewSign()
	if len(sg.Timestamp) != 13 {
		t.Fatalf("timestamp = %q, want len 13", sg.Timestamp)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(sg.Nonce) {
		t.Fatalf("nonce = %q, want 32 hex chars", sg.Nonce)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(sg.Sign) {
		t.Fatalf("sign = %q, want 32 hex chars", sg.Sign)
	}
	// 签名 = MD5(ts-nonce-key)
	h := md5.Sum([]byte(sg.Timestamp + "-" + sg.Nonce + "-" + signKey))
	if want := hex.EncodeToString(h[:]); sg.Sign != want {
		t.Fatalf("sign = %q, want %q", sg.Sign, want)
	}
	// 两次调用 nonce 不同
	sg2 := NewSign()
	if sg.Nonce == sg2.Nonce {
		t.Fatal("two NewSign calls produced same nonce")
	}
}
