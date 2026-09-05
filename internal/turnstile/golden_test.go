package turnstile

import (
	"encoding/json"
	"testing"

	"aurora/internal/sentinelvm"
)

// golden 测试(2026-09-05,G3 前置):与 so 包同构的 VM 语义快照。
// 直接构造 solver(绕过 solve() 的 requirements profile 解析),手动注册
// runtime 与 success 回调(对齐 solve() 内置行为)后跑微型字节码。
// 上游 sdk.js 轮换时,哪条 opcode 语义漂移由此精确定位。
//
// ⚠ 语义要点:操作类 opcode(op1/op5/op27...)的参数是【寄存器引用】,
// 字面值必须先 op2 入寄存器(与 so 包 golden 同轮澄清)。

// runGoldenQueue 直接驱动 VM 跑一段队列,返回 solver 供寄存器断言。
func runGoldenQueue(t *testing.T, queue []any, key string) *turnstileSolver {
	t.Helper()
	s := &turnstileSolver{
		Base:     sentinelvm.Base{Regs: map[string]any{}},
		maxSteps: 50000,
	}
	s.window = s.buildWindow()
	s.done = false
	s.initRuntime()
	// 对齐 solve():注册 success/error 终止回调(success 收值经 latin1Base64)
	s.SetReg(turnstileSuccessReg, vmFunc(func(args ...any) (any, error) {
		if !s.done {
			s.done = true
			var value any
			if len(args) > 0 {
				value = args[0]
			}
			s.resolved = sentinelvm.Latin1Base64Encode(s.jsToString(value))
		}
		return nil, nil
	}))
	s.SetReg(turnstileErrorReg, vmFunc(func(args ...any) (any, error) {
		if !s.done {
			s.done = true
		}
		return nil, nil
	}))
	s.SetReg(turnstileKeyReg, key)
	s.SetReg(turnstileQueueReg, queue)
	if err := s.runQueue(); err != nil {
		t.Fatalf("runQueue: %v", err)
	}
	return s
}

// 表驱动:锁寄存器终值(期望按当前语义独立手工推得)。
func TestTurnstileOpcodeGolden(t *testing.T) {
	cases := []struct {
		name  string
		queue []any
		reg   string
		want  any
	}{
		{
			name:  "op2 直接赋值",
			queue: []any{[]any{2.0, "v1", "hello"}},
			reg:   "v1", want: "hello",
		},
		{
			name:  "op8 寄存器拷贝",
			queue: []any{[]any{2.0, "v1", "a"}, []any{8.0, "v2", "v1"}},
			reg:   "v2", want: "a",
		},
		{
			name:  "op5 数字加法",
			queue: []any{[]any{2.0, "n1", 2.5}, []any{2.0, "n2", 3.0}, []any{5.0, "n1", "n2"}},
			reg:   "n1", want: 5.5,
		},
		{
			name:  "op5 字符串拼接",
			queue: []any{[]any{2.0, "s1", "ab"}, []any{2.0, "s2", "cd"}, []any{5.0, "s1", "s2"}},
			reg:   "s1", want: "abcd",
		},
		{
			name:  "op1 xor(abc ^ k)",
			queue: []any{[]any{2.0, "x1", "abc"}, []any{2.0, "k1", "k"}, []any{1.0, "x1", "k1"}},
			reg:   "x1", want: "\n\t\b",
		},
		{
			name:  "op19 base64 编码",
			queue: []any{[]any{2.0, "b2", "test"}, []any{19.0, "b2"}},
			reg:   "b2", want: "dGVzdA==",
		},
		{
			name:  "op27 数字减法(减数经寄存器)",
			queue: []any{[]any{2.0, "n3", 10.0}, []any{2.0, "n4", 4.0}, []any{27.0, "n3", "n4"}},
			reg:   "n3", want: 6.0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := runGoldenQueue(t, tc.queue, "golden-key")
			got := s.GetReg(tc.reg)
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(tc.want)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("reg %q = %s, want %s", tc.reg, gotJSON, wantJSON)
			}
		})
	}
}

// 端到端字节级快照:调用 success 回调(resolved = latin1Base64(jsToString(值)))。
func TestTurnstileSnapshotGolden(t *testing.T) {
	s := runGoldenQueue(t, []any{
		[]any{2.0, "v1", "golden"},
		[]any{7.0, 3.0, "v1"}, // 调 success(3)
	}, "golden-key")
	if got := s.resolved; got != "Z29sZGVu" {
		t.Fatalf("resolved = %q, want %q(VM 语义漂移?)", got, "Z29sZGVu")
	}
}
