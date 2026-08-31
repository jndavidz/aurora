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

// 回归:A3 —— 池内 exp 解析(健康端点的数据源):非法条目跳过,
// 池空但直传单 token 时解析直传值。
func TestRefreshTokenExps(t *testing.T) {
	c := NewClient("", "", "", "", "dev")
	if got := c.RefreshTokenExps(); len(got) != 0 {
		t.Fatalf("空池应返回空集, got %d", len(got))
	}

	far := time.Now().Add(90 * 24 * time.Hour).Unix()
	near := time.Now().Add(24 * time.Hour).Unix()
	c.tokens = []string{
		mkTestJWT(t, far),
		"garbage", // 解析失败跳过
		mkTestJWT(t, near),
	}
	exps := c.RefreshTokenExps()
	if len(exps) != 2 {
		t.Fatalf("应解析出 2 条(跳过 garbage), got %d", len(exps))
	}

	// 池空但直传了单 refresh_token:回退解析直传值
	c2 := NewClient("", "", mkTestJWT(t, time.Now().Add(48*time.Hour).Unix()), "", "dev")
	if got := c2.RefreshTokenExps(); len(got) != 1 {
		t.Fatalf("直传 token 应回退解析, got %d", len(got))
	}
}
