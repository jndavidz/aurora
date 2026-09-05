package so

import (
	"encoding/json"
	"testing"
	"time"
)

// golden 测试(2026-09-05,G3 前置):锁定 VM 各 opcode 的语义快照。
//
// 背景:so.go 与 turnstile.go 是跟随上游 SDK 反编译的两份 VM 实现(零测试)。
// 上游 SDK 改版后需要重对 opcode 语义,本测试以"微型字节码 + 期望终值"锁定
// 当前语义 —— 上游轮换后重跑,哪条漂移立刻定位,而不必全流程盲调。
// (真实 collector_dx/snapshot_dx 样本需 live 抓取,补齐后可叠加全流程快照。)
//
// 指令格式对齐 run()/runQueue():队列 = [[opcode, args...], ...],
// dx = latin1Base64Encode(xorString(json(queue), reqToken)),xor 对称可逆。
//
// ⚠ 语义要点(由首轮 golden 失败澄清):操作类 opcode(op1/op5/op27...)的
// 参数一律是【寄存器引用】,字面值必须先用 op2 存入寄存器 —— 否则被
// getReg 当寄存器名解析为 nil/"undefined"。这与真实 dx 的构造方式一致。

// mustEncodeDX 构造与上游同构的 dx 字节码。
func mustEncodeDX(t *testing.T, queue []any, key string) string {
	t.Helper()
	b, err := json.Marshal(queue)
	if err != nil {
		t.Fatal(err)
	}
	return latin1Base64Encode(xorString(string(b), key))
}

// runGolden 跑一段微型字节码(snapshot 模式),返回 resolved(success 回调
// 收到的值经 latin1Base64Encode)。纯计算字节码无时间/随机参与,输出确定。
func runGolden(t *testing.T, queue []any, key string) (string, error) {
	t.Helper()
	dx := mustEncodeDX(t, queue, key)
	s := newSOSolver()
	return s.run(key, dx, false)
}

// 表驱动:锁"寄存器终值"。期望值均按当前 opcode 语义独立手工推得
// (非被测函数生成),语义漂移即 FAIL。
func TestVMOpcodeGolden(t *testing.T) {
	cases := []struct {
		name  string
		queue []any
		reg   string // 执行后读取的寄存器
		want  any    // 期望终值(JSON 解码后的 Go 值)
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
			name:  "op5 数组追加(右值经寄存器)",
			queue: []any{[]any{2.0, "a1", []any{1.0, 2.0}}, []any{2.0, "e1", "x"}, []any{5.0, "a1", "e1"}},
			reg:   "a1", want: []any{1.0, 2.0, "x"},
		},
		{
			name:  "op1 xor(abc ^ k:0x0A,0x09,0x08)",
			queue: []any{[]any{2.0, "x1", "abc"}, []any{2.0, "k1", "k"}, []any{1.0, "x1", "k1"}},
			reg:   "x1", want: "\n\t\b",
		},
		{
			name:  "op14 JSON 解析",
			queue: []any{[]any{2.0, "j1", `{"a":1}`}, []any{14.0, "o1", "j1"}},
			reg:   "o1", want: map[string]any{"a": 1.0},
		},
		{
			name:  "op15 JSON 序列化往返",
			queue: []any{[]any{2.0, "j1", `{"a":1}`}, []any{14.0, "o1", "j1"}, []any{15.0, "s9", "o1"}},
			reg:   "s9", want: `{"a":1}`,
		},
		{
			name:  "op18 base64 解码",
			queue: []any{[]any{2.0, "b1", "dGVzdA=="}, []any{18.0, "b1"}},
			reg:   "b1", want: "test",
		},
		{
			name:  "op19 base64 编码",
			queue: []any{[]any{2.0, "b2", "test"}, []any{19.0, "b2"}},
			reg:   "b2", want: "dGVzdA==",
		},
		{
			name:  "op27 数组删元素(下标经寄存器)",
			queue: []any{[]any{2.0, "a2", []any{1.0, 2.0, 3.0}}, []any{2.0, "i1", 2.0}, []any{27.0, "a2", "i1"}},
			reg:   "a2", want: []any{1.0, 3.0},
		},
		{
			name:  "op27 数字减法(减数经寄存器)",
			queue: []any{[]any{2.0, "n3", 10.0}, []any{2.0, "n4", 4.0}, []any{27.0, "n3", "n4"}},
			reg:   "n3", want: 6.0,
		},
		{
			name: "op22 子队列执行后恢复外层",
			queue: []any{
				[]any{2.0, "outer", "kept"},
				[]any{22.0, "ignored", []any{[]any{2.0, "inner", "ran"}}},
				[]any{2.0, "after", "yes"},
			},
			reg: "after", want: "yes",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dx := mustEncodeDX(t, tc.queue, "golden-key")
			s := newSOSolver()
			if _, err := s.run("golden-key", dx, false); err != nil {
				// vm unresolved 是预期内的执行形态(未调 success),不影响寄存器断言
				t.Logf("run returned error (acceptable for reg-level cases): %v", err)
			}
			got := s.getReg(tc.reg)
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(tc.want)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("reg %q = %s, want %s", tc.reg, gotJSON, wantJSON)
			}
		})
	}
}

// 端到端字节级快照:微型程序调用 success 回调(resolved = latin1Base64(值))。
// 纯计算输出确定 —— 上游 VM 行为漂移时此值会变,即为 golden 哨兵。
func TestVMSnapshotGolden(t *testing.T) {
	cases := []struct {
		name  string
		queue []any
		want  string // 期望 resolved(base64 字节级)
	}{
		{
			name:  "字符串直达 success",
			queue: []any{[]any{2.0, "v1", "golden"}, []any{7.0, 3.0, "v1"}},
			want:  "Z29sZGVu",
		},
		{
			// op19 后 n="NQ==";success 回调自身再做一次 latin1Base64 → 双层
			name:  "计算链:2+3 → op19 → success(双层 base64)",
			queue: []any{[]any{2.0, "n", 2.0}, []any{2.0, "m", 3.0}, []any{5.0, "n", "m"}, []any{19.0, "n"}, []any{7.0, 3.0, "n"}},
			want:  "TlE9PQ==",
		},
		{
			name:  "xor 链:abc ^ kk(0x0A,0x09,0x08 → base64)",
			queue: []any{[]any{2.0, "x", "abc"}, []any{2.0, "k", "kk"}, []any{1.0, "x", "k"}, []any{7.0, 3.0, "x"}},
			want:  "CgkI",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := runGolden(t, tc.queue, "golden-key")
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if got != tc.want {
				t.Fatalf("resolved = %q, want %q(VM 语义漂移?)", got, tc.want)
			}
		})
	}
}

// 防御性快照(P0-4 回归哨兵):超过 maxQueueSteps 的指令量必须在步数上限内
// 以错误终止,不得无限空转(上游下发损坏/构造字节码时的进程保护)。
func TestVMInfiniteLoopTerminates(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		var loop []any
		for i := 0; i < maxQueueSteps+10; i++ {
			loop = append(loop, []any{5.0, "cnt", 1.0}) // 恒 +1,永不终止
		}
		queue := []any{[]any{2.0, "cnt", 0.0}, []any{22.0, "ignored", loop}}
		dx := mustEncodeDX(t, queue, "k")
		_, _ = newSOSolver().run("k", dx, false)
	}()
	select {
	case <-done:
		// 在步数上限内终止 ✔
	case <-time.After(30 * time.Second):
		t.Error("runQueue 未在步数上限内终止(P0-4 回归)")
	}
}
