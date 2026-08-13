package deepseekweb

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strconv"
)

// DeepSeekHashV1 PoW 求解。
//
// 算法(依据 ai-wen/ds2api 测试向量与社区逆向):
//   - 输入 = salt + "_" + expire_at + "_" + nonce(十进制)
//   - hash = Keccak-f[1600] 的 23 轮变体(跳过第 0 轮),rate=136(SHA3-256 海绵),
//     输出 32 字节
//   - 目标:hash 的 hex == challenge 字段
//   - difficulty 是 nonce 搜索上界(实测 144000,平均 ~72000 次)
// 测试向量:"testsalt_1700000000_42" → d4a2ea58c89e40887c933484868380c6f803eaa8dc53a3b9df8e431b921a4f09

// powChallenge 对应 create_pow_challenge 响应里的 challenge 对象。
type powChallenge struct {
	Algorithm  string `json:"algorithm"`
	Challenge  string `json:"challenge"`
	Salt       string `json:"salt"`
	Signature  string `json:"signature"`
	Difficulty int64  `json:"difficulty"`
	ExpireAt   int64  `json:"expire_at"`
	ExpireAfter int64 `json:"expire_after,omitempty"`
	TargetPath string `json:"target_path"`
}

// SolvePow 求解 PoW,返回 x-ds-pow-response 头的值。
// 返回 (headerValue, 迭代次数, error)。
func SolvePow(ch *powChallenge) (string, int64, error) {
	prefix := ch.Salt + "_" + strconv.FormatInt(ch.ExpireAt, 10) + "_"
	limit := ch.Difficulty
	if limit <= 0 {
		limit = 144000
	}
	// 逐 nonce 计算,命中即返回。
	for nonce := int64(0); nonce <= limit; nonce++ {
		msg := prefix + strconv.FormatInt(nonce, 10)
		if hexEqual(hashDeepSeekV1([]byte(msg)), ch.Challenge) {
			payload := map[string]any{
				"algorithm":   "DeepSeekHashV1",
				"challenge":   ch.Challenge,
				"salt":        ch.Salt,
				"answer":      nonce,
				"signature":   ch.Signature,
				"target_path": ch.TargetPath,
			}
			b, _ := json.Marshal(payload)
			return base64.StdEncoding.EncodeToString(b), nonce, nil
		}
	}
	return "", 0, fmt.Errorf("pow not solved within difficulty=%d", limit)
}

func hexEqual(digest []byte, hexTarget string) bool {
	if len(hexTarget) != len(digest)*2 {
		return false
	}
	const hexDigits = "0123456789abcdef"
	for i, b := range digest {
		if hexDigits[b>>4] != hexTarget[i*2] || hexDigits[b&0x0f] != hexTarget[i*2+1] {
			return false
		}
	}
	return true
}

// ── Keccak-f[1600] 23 轮变体(跳过第 0 轮) ──

var keccakRC = [24]uint64{
	0x0000000000000001, 0x0000000000008082, 0x800000000000808A, 0x8000000080008000,
	0x000000000000808B, 0x0000000080000001, 0x8000000080008081, 0x8000000000008009,
	0x000000000000008A, 0x0000000000000088, 0x0000000080008009, 0x000000008000000A,
	0x000000008000808B, 0x800000000000008B, 0x8000000000008089, 0x8000000000008003,
	0x8000000000008002, 0x8000000000000080, 0x000000000000800A, 0x800000008000000A,
	0x8000000080008081, 0x8000000000008080, 0x0000000080000001, 0x8000000080008008,
}

// keccakF1600_23 执行 23 轮 Keccak-f(使用 RC[1..23],跳过第 0 轮)。
func keccakF1600_23(a *[25]uint64) {
	for round := 1; round < 24; round++ {
		// θ
		var c [5]uint64
		for x := 0; x < 5; x++ {
			c[x] = a[x] ^ a[x+5] ^ a[x+10] ^ a[x+15] ^ a[x+20]
		}
		var d [5]uint64
		for x := 0; x < 5; x++ {
			d[x] = c[(x+4)%5] ^ rotl64(c[(x+1)%5], 1)
		}
		for x := 0; x < 5; x++ {
			for y := 0; y < 5; y++ {
				a[x+5*y] ^= d[x]
			}
		}
		// ρ + π
		var b [25]uint64
		for x := 0; x < 5; x++ {
			for y := 0; y < 5; y++ {
				b[y+5*((2*x+3*y)%5)] = rotl64(a[x+5*y], rhoOffsets[x+5*y])
			}
		}
		// χ
		for y := 0; y < 5; y++ {
			for x := 0; x < 5; x++ {
				a[x+5*y] = b[x+5*y] ^ (^b[(x+1)%5+5*y] & b[(x+2)%5+5*y])
			}
		}
		// ι
		a[0] ^= keccakRC[round]
	}
}

var rhoOffsets = [25]int{
	0, 1, 62, 28, 27,
	36, 44, 6, 55, 20,
	3, 10, 43, 25, 39,
	41, 45, 15, 21, 8,
	18, 2, 61, 56, 14,
}

func rotl64(v uint64, n int) uint64 {
	n &= 63
	if n == 0 {
		return v
	}
	return (v << n) | (v >> (64 - n))
}

// hashDeepSeekV1 对输入做 23 轮 Keccak-256(SHA3-256 海绵,rate=136,pad 0x06)。
func hashDeepSeekV1(msg []byte) []byte {
	const rate = 136 // SHA3-256
	var st [25]uint64

	// 吸收
	for len(msg) >= rate {
		for i := 0; i < rate/8; i++ {
			st[i] ^= binary.LittleEndian.Uint64(msg[i*8:])
		}
		keccakF1600_23(&st)
		msg = msg[rate:]
	}
	// 末块 + 0x06 padding + 0x80
	var block [rate]byte
	copy(block[:], msg)
	block[len(msg)] = 0x06
	block[rate-1] |= 0x80
	for i := 0; i < rate/8; i++ {
		st[i] ^= binary.LittleEndian.Uint64(block[i*8:])
	}
	keccakF1600_23(&st)

	// 挤压 32 字节
	out := make([]byte, 32)
	for i := 0; i < 4; i++ {
		binary.LittleEndian.PutUint64(out[i*8:], st[i])
	}
	return out
}

// SolvePowForPath 便捷函数:给定 challenge 响应原始 JSON(biz_data 层),
// 求解并返回 x-ds-pow-response 头值。
func SolvePowForPath(bizData []byte) (string, error) {
	var wrapper struct {
		Challenge powChallenge `json:"challenge"`
	}
	if err := json.Unmarshal(bizData, &wrapper); err != nil {
		return "", fmt.Errorf("parse pow challenge: %w", err)
	}
	if wrapper.Challenge.Challenge == "" {
		return "", fmt.Errorf("empty challenge")
	}
	h, _, err := SolvePow(&wrapper.Challenge)
	return h, err
}
