// Package sentinelvm 提供 so/turnstile 两份 sentinel VM 的共享底层
// (G3 合并,2026-09-05)。
//
// 合并边界(逐函数 diff 实证后划定):
//   - 本包只收【语义等价已证】的部分:寄存器存取 Base、Latin-1/base64/xor
//     纯函数、regKey(NaN 安全版,so/turnstile golden 双向验证)。
//   - asNumber(失败值 0 vs NaN)、jsToString(so 缺 NaN/Infinity 语义)、
//     jsJSONStringify(裸 Marshal vs JS 语义)、jsGetProp、buildWindow、
//     initRuntime 是【真实语义差异】,保留各自实现 —— 合并即改行为,
//     违背"合并不改行为"原则,不做。
package sentinelvm

import (
	"encoding/base64"
	"fmt"
	"math"
	"strconv"
)

// VmFunc 是 VM 原生函数的统一签名(两份 VM 的 opcode 处理器同型)。
type VmFunc = func(args ...any) (any, error)

// Base 是两份 VM 共享的寄存器底座(soSolver/turnstileSolver 嵌入)。
// 寄存器键经 RegKey 规范化(字符串/数值/其他类型分前缀),两份 VM 同构。
type Base struct {
	Regs map[string]any
}

// SetReg 写寄存器(键规范化)。
func (b *Base) SetReg(key, value any) { b.Regs[RegKey(key)] = value }

// GetReg 读寄存器。
func (b *Base) GetReg(key any) any { return b.Regs[RegKey(key)] }

// CopyAnySlice 深拷贝 []any(顶层)。
func CopyAnySlice(value []any) []any {
	if len(value) == 0 {
		return nil
	}
	out := make([]any, len(value))
	copy(out, value)
	return out
}

// CopyQueue 拷贝当前指令队列(queueKey 为队列所在寄存器)。
func (b *Base) CopyQueue(queueKey any) []any {
	queue, _ := b.GetReg(queueKey).([]any)
	return CopyAnySlice(queue)
}

// CallFn 调用寄存器中的原生函数(非 vmFunc 值原样返回)。
func (b *Base) CallFn(value any, args ...any) (any, error) {
	if fn, ok := value.(VmFunc); ok {
		return fn(args...)
	}
	return value, nil
}

// DerefArgs 把参数中的寄存器引用解引用为值。
func (b *Base) DerefArgs(args []any) []any {
	out := make([]any, 0, len(args))
	for _, arg := range args {
		out = append(out, b.GetReg(arg))
	}
	return out
}

// RegKey 寄存器键规范化(字符串 s: / 数值 n: / 其他 x:)。
// turnstile 版(NaN 安全:math.Trunc 判定),so 侧 float64(int64(v)) 对
// 超大 float 有实现定义行为,统一采用 NaN 安全版。
func RegKey(value any) string {
	switch v := value.(type) {
	case nil:
		return "nil"
	case string:
		return "s:" + v
	case int:
		return "n:" + strconv.Itoa(v)
	case int64:
		return "n:" + strconv.FormatInt(v, 10)
	case float64:
		if math.Trunc(v) == v {
			return "n:" + strconv.FormatInt(int64(v), 10)
		}
		return "n:" + strconv.FormatFloat(v, 'g', -1, 64)
	}
	return "x:" + fmt.Sprintf("%v", value)
}

// ─── Latin-1 / base64 / XOR 纯函数 ──────────────────────────────────────────

// Latin1StringToBytes 按 Latin-1 语义把字符串转字节(128-255 字符按单字节)。
func Latin1StringToBytes(value string) []byte {
	out := make([]byte, 0, len(value))
	for _, r := range value {
		out = append(out, byte(r))
	}
	return out
}

// BytesToLatin1Runes 字节按 Latin-1 语义转回 rune(与 so/turnstile 原实现逐字节相同)。
func BytesToLatin1Runes(value []byte) []rune {
	out := make([]rune, 0, len(value))
	for _, b := range value {
		out = append(out, rune(b))
	}
	return out
}

// Latin1Base64Encode 对齐浏览器 atob/btoa 的 Latin-1 语义。
func Latin1Base64Encode(value string) string {
	return base64.StdEncoding.EncodeToString(Latin1StringToBytes(value))
}

// Latin1Base64Decode base64 解码为 Latin-1 字符串。
func Latin1Base64Decode(value string) (string, error) {
	body, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	return string(BytesToLatin1Runes(body)), nil
}

// XORString 逐字节 XOR(key 循环),Latin-1 语义;XOR 对称可逆。
func XORString(data, key string) string {
	if key == "" {
		return data
	}
	dataBytes := Latin1StringToBytes(data)
	keyBytes := Latin1StringToBytes(key)
	out := make([]byte, len(dataBytes))
	for idx := range dataBytes {
		out[idx] = dataBytes[idx] ^ keyBytes[idx%len(keyBytes)]
	}
	return string(BytesToLatin1Runes(out))
}
