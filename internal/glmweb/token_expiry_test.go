package glmweb

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

// mkTestJWT 构造仅含 exp 声明的测试 JWT(不验签,与 jwtutil 的解析前提一致)。
func mkTestJWT(t *testing.T, expUnix int64) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"exp": expUnix})
	if err != nil {
		t.Fatal(err)
	}
	return "hdr." + base64.RawURLEncoding.EncodeToString(raw) + ".sig"
}

// 回归:A1 —— access_token 过期/缺失/解析失败都必须视为"临近过期",
// 否则 ensureToken 拿废票打上游,502 到进程重启。
func TestAccessTokenNearExpiryAndClear(t *testing.T) {
	c := NewClient("", "", "refresh-token", "", "dev")
	if c.HasAccessToken() {
		t.Fatal("初始应无 access_token")
	}
	if !c.AccessTokenNearExpiry(10 * time.Minute) {
		t.Fatal("无票应视为临近过期")
	}

	c.accessToken = mkTestJWT(t, time.Now().Add(-time.Hour).Unix()) // 已过期
	if !c.AccessTokenNearExpiry(10 * time.Minute) {
		t.Fatal("过期票应视为临近过期")
	}

	c.accessToken = mkTestJWT(t, time.Now().Add(time.Hour).Unix()) // 1 小时后过期
	if c.AccessTokenNearExpiry(10 * time.Minute) {
		t.Fatal("距过期 1h > skew 10min,不应触发")
	}
	if !c.AccessTokenNearExpiry(2 * time.Hour) {
		t.Fatal("距过期 1h < skew 2h,应触发")
	}

	c.accessToken = "garbage" // 非 JWT
	if !c.AccessTokenNearExpiry(10 * time.Minute) {
		t.Fatal("exp 解析失败应视为临近过期")
	}

	c.ClearAccessToken()
	if c.HasAccessToken() {
		t.Fatal("ClearAccessToken 后应无票")
	}
	if !c.AccessTokenNearExpiry(10 * time.Minute) {
		t.Fatal("清票后应视为临近过期")
	}
}
