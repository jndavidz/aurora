// Package glmweb 实现智谱清言(chatglm.cn)网页接口逆向客户端。
//
// 协议要点(2026-08-13 CDP 抓包 + 主 JS 逆向确认):
//   - 认证:chatglm_refresh_token(cookie)→ POST /chatglm/user-api/user/refresh
//     换发 access_token(JWT,~2h);completion 用 Authorization: Bearer <JWT>
//   - 签名:所有 /chatglm/ 请求带 X-Timestamp/X-Nonce/X-Sign,
//     X-Sign = MD5(混淆timestamp + "-" + nonce + "-" + 固定密钥)
//   - completion:POST /chatglm/backend-api/assistant/stream(SSE,parts 全量重发)
package glmweb

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"
)

const (
	// signKey 是 X-Sign 的固定密钥(从主 JS 逆向提取,实测校验通过)。
	signKey = "8a1317a7468aa3ad86e997d08f3f31cb"
)

// SignHeader 是一组请求签名头。
type SignHeader struct {
	Timestamp string
	Nonce     string
	Sign      string
}

// NewSign 生成签名头。
//
// 算法(与网页 JS 一致):
//   - timestamp:Date.now() 的混淆值 —— 中间位(倒数第二位)替换成
//     (各位数字之和 - 倒数第二位) % 10 的校验值
//   - nonce:UUID v4 去掉连字符
//   - sign:MD5(timestamp + "-" + nonce + "-" + signKey)
func NewSign() SignHeader {
	ts := obfuscatedTimestamp(time.Now().UnixMilli())
	nonce := uuidNoDash()
	sign := md5hex(ts + "-" + nonce + "-" + signKey)
	return SignHeader{Timestamp: ts, Nonce: nonce, Sign: sign}
}

// obfuscatedTimestamp 实现网页 JS 的 timestamp 混淆:
// A=Date.now() 字符串,e=A.length,i=各位数字,t=sum(i)-i[e-2],
// 结果 = A[0:e-2] + (t%10) + A[e-1:e]。
func obfuscatedTimestamp(ms int64) string {
	A := strconv.FormatInt(ms, 10)
	e := len(A)
	sum := 0
	digits := make([]int, e)
	for i := 0; i < e; i++ {
		digits[i] = int(A[i] - '0')
		sum += digits[i]
	}
	t := sum - digits[e-2]
	return A[0:e-2] + strconv.Itoa(t%10) + A[e-1:e]
}

// uuidNoDash 生成 32 位无连字符的 UUID。
func uuidNoDash() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b[:])
}

func md5hex(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

var _ = fmt.Sprintf
