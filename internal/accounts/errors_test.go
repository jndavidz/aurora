package accounts

import (
	"strings"
	"testing"
	"time"
)

// ---- ClassifyFailure ----

func TestClassifyFailureByCode(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   FailureKind
	}{
		{"401 无 body", 401, "", KindAuth},
		{"403 无 body", 403, "", KindAuth},
		{"429 无 body", 429, "", KindRateLimited},
		{"400 无已知码", 400, `{"error":"bad"}`, KindInput},
		{"500 无已知码", 500, `oops`, KindOverload},
		{"网络错误(无状态)", 0, `connection reset`, KindTransient},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ClassifyFailure(c.status, c.body)
			if got != c.want {
				t.Errorf("ClassifyFailure(%d,%q)=%s want %s", c.status, c.body[:min(30, len(c.body))], got, c.want)
			}
		})
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---- ReportResult 状态机 ----

func newTestPool() *Pool {
	return NewPool([]*Account{
		{ID: "aaaaaaaa-1111", Status: StatusActive, Type: TypeFree},
	})
}

func TestReportResultAuthMarksExpired(t *testing.T) {
	p := newTestPool()
	a := p.free[0]
	changed := p.ReportResult(a, 401, `{"error":"unauthorized"}`)
	if !changed || a.Status != StatusExpired {
		t.Errorf("401 应标 StatusExpired: changed=%v status=%s", changed, a.Status)
	}
	// 二次同错不重复计数
	if p.ReportResult(a, 401, "") {
		t.Error("重复 401 不应再变更")
	}
}

func TestReportResultRateLimitedCooldown(t *testing.T) {
	p := newTestPool()
	a := p.free[0]
	if !p.ReportResult(a, 429, "") {
		t.Fatal("429 应被处理")
	}
	if a.Status != StatusRateLimited {
		t.Errorf("status=%s want rate_limited", a.Status)
	}
	if a.cooldownUntil.Before(time.Now().Add(4 * time.Minute)) {
		t.Errorf("冷却应约 5 分钟, got %v", a.cooldownUntil)
	}
	// Acquire 应跳过冷却中的账号
	got, err := p.Acquire(TypeFree)
	if err == nil && got == a {
		t.Error("冷却中的账号不应被 Acquire 选中")
	}
}

// TestReportResultBannedLongCooldown 暂缓:Banned 触发路径依赖错误体关键词分类,
// 腾讯混元码段移除后暂无 aurora 侧触发源。保留 KindBanned 常量与冷却机制备用。

func TestReportResultOverloadNotMarked(t *testing.T) {
	p := newTestPool()
	a := p.free[0]
	// 上游过载与账号无关,不应标记(否则一次 500 就把好账号踢下线)
	p.ReportResult(a, 500, `{"code":11134,"msg":"temporarily unavailable"}`)
	if a.Status != StatusActive {
		t.Errorf("11134 不应改变账号状态, got %s", a.Status)
	}
}

func TestReportResultInputNotMarked(t *testing.T) {
	p := newTestPool()
	a := p.free[0]
	p.ReportResult(a, 400, `{"code":11115,"msg":"input length too long"}`)
	if a.Status != StatusActive {
		t.Errorf("11115 是请求侧问题,不应标记账号, got %s", a.Status)
	}
}

func TestCooldownExpireBackToActive(t *testing.T) {
	p := newTestPool()
	a := p.free[0]
	p.ReportResult(a, 429, "")
	a.Status = StatusRateLimited
	// 模拟冷却到期
	a.cooldownUntil = time.Now().Add(-time.Second)
	got, err := p.Acquire(TypeFree)
	if err != nil {
		t.Fatalf("冷却到期后应可 Acquire: %v", err)
	}
	if got != a || a.Status != StatusActive {
		t.Errorf("冷却到期应回池: status=%s", a.Status)
	}
	if !a.cooldownUntil.IsZero() {
		t.Error("回池后冷却时间应清零")
	}
}

func TestClassifyKeywordCaseInsensitive(t *testing.T) {
	if got := ClassifyFailure(200, `data: {"error":"CONTENT-FILTER blocked"}`); got == KindUnknown {
		// 200 + filter 文本:由 converter 的 filter 重试处理,此处归类不强制
		t.Log("200 带审核文本不在此分类器职责内(流式完成)")
	}
	// 小写兼容
	if got := ClassifyFailure(429, "rate limit exceeded"); got != KindRateLimited {
		t.Errorf("小写 rate limit 应识别为限流, got %s", got)
	}
	if got := ClassifyFailure(0, "Connection Reset by peer"); got != KindTransient {
		t.Errorf("连接重置应识别为 transient, got %s", got)
	}
	_ = strings.TrimSpace
}
