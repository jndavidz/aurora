// Package jwtutil 提供不验签的 JWT 声明读取(仅用于本地"换发时机"判断)。
//
// 与 kimiweb.parseClaims 同思路:客户端只需读 exp 决定何时用 refresh_token
// 重换发 access_token,既不需要也无法验证上游签名。
// 放独立小包而非塞进 glmweb/kimiweb,避免两家各抄一份 exp 解析(审计结论:重复即分叉)。
package jwtutil

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Exp 返回 JWT 的 exp 声明(UTC)。token 非法、payload 不可解码或缺 exp 时 ok=false。
// exp 兼容数字与数字字符串两种编码(RFC 7519 允许 NumericDate 为字符串的变体实现)。
func Exp(token string) (time.Time, bool) {
	dot1 := strings.IndexByte(token, '.')
	if dot1 < 0 {
		return time.Time{}, false
	}
	rest := token[dot1+1:]
	dot2 := strings.IndexByte(rest, '.')
	if dot2 < 0 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(rest[:dot2])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp json.Number `json:"exp"` // json.Number 同时吃下数字与字符串形态
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, false
	}
	if claims.Exp == "" {
		return time.Time{}, false
	}
	sec, err := strconv.ParseInt(claims.Exp.String(), 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(sec, 0).UTC(), true
}
