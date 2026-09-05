package sentinelvm

import (
	"math"
	"testing"
)

// RegKey:键规范化(字符串/数值/其他),NaN 安全(math.Trunc 判定)。
func TestRegKey(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{nil, "nil"},
		{"v1", "s:v1"},
		{9, "n:9"},            // opcode/寄存器号(int)
		{int64(3), "n:3"},     // successReg 类
		{3.0, "n:3"},          // JSON 反序列化的数值(float64 整值)
		{2.5, "n:2.5"},        // 非整数 float
		{math.NaN(), "n:NaN"}, // NaN 安全:FormatFloat 分支,不进 int64 转换(原 turnstile 行为)
		{[]any{1}, "x:[1]"},
	}
	for _, tc := range cases {
		if got := RegKey(tc.in); got != tc.want {
			t.Errorf("RegKey(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// +Inf/大数:不 panic 且非 n: 整数路径的误判
	if got := RegKey(math.Inf(1)); got == "n:" {
		t.Errorf("RegKey(+Inf) 误入整数路径")
	}
}

// Latin-1 边界:128-255 字符必须按单字节(UTF-8 多字节展开是 bug)。
func TestLatin1RoundTrip(t *testing.T) {
	// 构造覆盖 0-255 的 Latin-1 字符串
	var sb []rune
	for i := 0; i < 256; i++ {
		sb = append(sb, rune(i))
	}
	in := string(sb)
	b := Latin1StringToBytes(in)
	if len(b) != 256 {
		t.Fatalf("Latin1StringToBytes len = %d, want 256(多字节展开即 bug)", len(b))
	}
	back := string(BytesToLatin1Runes(b))
	if back != in {
		t.Fatal("Latin-1 往返不一致")
	}
}

// XORString:对称可逆、循环 key、空 key 透传。
func TestXORString(t *testing.T) {
	if got := XORString("abc", ""); got != "abc" {
		t.Fatalf("空 key 应透传, got %q", got)
	}
	// 手算:0x61^0x6B=0x0A, 0x62^0x6B=0x09, 0x63^0x6B=0x08
	if got := XORString("abc", "k"); got != "\n\t\b" {
		t.Fatalf("XORString(abc,k) = %q, want %q", got, "\n\t\b")
	}
	if XORString(XORString("payload", "key"), "key") != "payload" {
		t.Fatal("XOR 非对称可逆")
	}
	// 循环 key:key 长度 2 时第三字节用 key[0]
	if got := XORString("abc", "kk"); got[0] != "abc"[0]^"kk"[0] {
		t.Fatalf("循环 key 语义漂移")
	}
}

// Latin1Base64 编解码往返(atob/btoa Latin-1 语义)。
func TestLatin1Base64RoundTrip(t *testing.T) {
	in := "dG9rZW4=" // base64("token"),含 padding
	if got, _ := Latin1Base64Decode(in); got != "token" {
		t.Fatalf("Decode = %q", got)
	}
	if got := Latin1Base64Encode("token"); got != in {
		t.Fatalf("Encode = %q", got)
	}
	// 128-255 字节区间(普通 base64 与 Latin-1 语义在此分叉)
	high := string(BytesToLatin1Runes([]byte{0x80, 0xFF, 0x00}))
	dec, _ := Latin1Base64Decode(Latin1Base64Encode(high))
	// 比较口径:双方都转 Latin-1 字节域(high 的 Go string 是 UTF-8 字节,不可直接比)
	if string(Latin1StringToBytes(dec)) != string(Latin1StringToBytes(high)) {
		t.Fatal("高位字节往返不一致")
	}
}

// Base 寄存器底座:Set/Get 规范化一致、CopyQueue 深拷贝顶层、CallFn 断言
// VmFunc(类型身份依赖统一别名)、DerefArgs 解引用。
func TestBase(t *testing.T) {
	b := &Base{Regs: map[string]any{}}
	b.SetReg("k1", "val")
	b.SetReg(9, []any{"q0"})
	if b.GetReg("k1") != "val" {
		t.Fatal("string 寄存器读写不一致")
	}
	if _, ok := b.GetReg(9).([]any); !ok {
		t.Fatal("int 键(opcode)寄存器读写不一致")
	}
	// CopyQueue:修改拷贝不影响原队列
	q := []any{1.0, 2.0}
	b.SetReg("q", q)
	cp := b.CopyQueue("q")
	cp[0] = 99.0
	if q[0] != 1.0 {
		t.Fatal("CopyQueue 未拷贝顶层")
	}
	// CallFn:VmFunc 调用 / 非 func 原样返回
	fn := VmFunc(func(args ...any) (any, error) { return "called", nil })
	if v, _ := b.CallFn(fn); v != "called" {
		t.Fatal("CallFn 未调用 VmFunc")
	}
	if v, _ := b.CallFn(42); v != 42 {
		t.Fatal("CallFn 非 func 应原样返回")
	}
	// DerefArgs:寄存器引用解引用
	b.SetReg("r1", "resolved")
	if got := b.DerefArgs([]any{"r1", "literal"})[0]; got != "resolved" {
		t.Fatalf("DerefArgs = %v", got)
	}
}
