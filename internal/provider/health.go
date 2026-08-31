package provider

import "time"

// CredentialHealth 单个 provider 的凭证健康快照。
// 字段设计对齐可靠性计划 A3:让"MiniMax 还有 3 天过期"这类问题可查询、可告警。
type CredentialHealth struct {
	Name                string   `json:"name"`
	Status              string   `json:"status"` // ok | warn | critical | expired | empty | unmanaged
	Accounts            int      `json:"accounts"`
	MinRefreshExpiresAt *string  `json:"minRefreshExpiresAt,omitempty"` // 池内最早过期的长期凭据(需重抓的时限)
	MinRefreshDays      *float64 `json:"minRefreshExpiresInDays,omitempty"`
	Detail              string   `json:"detail,omitempty"`
}

// HealthReporter 是 provider 的可选接口:实现它即可被 /v1/health/credentials 采集。
// 有 exp 可解析凭据(.refresh 换发型)的通道实现它;会话级/cookie 级通道
// (DeepSeek/Grok/豆包/千问/Mimo 等)不实现,报告为 unmanaged。
type HealthReporter interface {
	CredentialHealth() CredentialHealth
}

// 凭证剩余天数分档(对齐 CREDENTIALS.md 的保活分级):
// 已过期 → expired;<3 天 → critical;<14 天 → warn;其余 → ok。
func statusFor(days float64) string {
	switch {
	case days < 0:
		return "expired"
	case days < 3:
		return "critical"
	case days < 14:
		return "warn"
	default:
		return "ok"
	}
}

// minExps 取一组过期时间中最早的;空集返回 ok=false。
func minExps(exps []time.Time) (time.Time, bool) {
	var min time.Time
	for _, e := range exps {
		if min.IsZero() || e.Before(min) {
			min = e
		}
	}
	return min, !min.IsZero()
}

// fillRefreshExpiry 把"最早过期时间 + 天数 + 分档"填入 h,返回是否填了。
func fillRefreshExpiry(h *CredentialHealth, exps []time.Time) bool {
	min, ok := minExps(exps)
	if !ok {
		return false
	}
	days := time.Until(min).Hours() / 24
	s := min.Format(time.RFC3339)
	h.MinRefreshExpiresAt = &s
	h.MinRefreshDays = &days
	h.Status = statusFor(days)
	return true
}

// CredentialHealthReport 汇总所有已注册 provider 的凭证健康。
// 未实现 HealthReporter 的通道报 unmanaged(其凭据无 exp 可解析,如会话级 token)。
func (r *Registry) CredentialHealthReport() []CredentialHealth {
	out := make([]CredentialHealth, 0, len(r.providers))
	for _, p := range r.providers {
		if hr, ok := p.(HealthReporter); ok {
			out = append(out, hr.CredentialHealth())
			continue
		}
		out = append(out, CredentialHealth{
			Name:     p.Name(),
			Status:   "unmanaged",
			Accounts: -1,
			Detail:   "该通道凭据无 exp 可解析(会话级/cookie 级),有效期见 docs/CREDENTIALS.md",
		})
	}
	return out
}
