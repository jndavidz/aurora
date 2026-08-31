package accounts

import (
	"time"

	"aurora/internal/jwtutil"
)

// AccountCredHealth 单个账号的凭证快照(ID 已脱敏,不含 token 本体)。
type AccountCredHealth struct {
	ID        string   `json:"id"` // 前 8 位
	Type      string   `json:"type"`
	Status    string   `json:"status"`
	ExpiresAt *string  `json:"accessExpiresAt,omitempty"`
	Days      *float64 `json:"accessExpiresInDays,omitempty"`
	Detail    string   `json:"detail,omitempty"` // exp 无法解析时的说明
}

// PoolHealth ChatGPT 兜底池的凭证健康汇总。
type PoolHealth struct {
	NoAuth             int                 `json:"noauth"`
	Free               int                 `json:"free"`
	PUID               int                 `json:"puid"`
	Temporary          int                 `json:"temporary"`
	Active             int                 `json:"active"`
	Expired            int                 `json:"expired"`
	MinAccessExpiresAt *string             `json:"minAccessExpiresAt,omitempty"`
	MinAccessDays      *float64            `json:"minAccessExpiresInDays,omitempty"`
	Accounts           []AccountCredHealth `json:"accounts"`
}

// CredentialHealth 汇总池内所有账号(三池 + 临时)的凭证健康。
// access_token 是 JWT,exp 可解析(实测 ~90 天);解析失败的账号(UUID 型无票)
// 单独标注,不影响其余账号的报告。
func (p *Pool) CredentialHealth() PoolHealth {
	h := PoolHealth{Accounts: []AccountCredHealth{}}
	var minExp time.Time

	add := func(acct *Account) {
		entry := AccountCredHealth{
			ID:     acct.ID,
			Type:   acct.Type.String(),
			Status: acct.Status.String(),
		}
		if len(entry.ID) > 8 {
			entry.ID = entry.ID[:8]
		}
		if exp, ok := jwtutil.Exp(acct.Token); ok {
			days := time.Until(exp).Hours() / 24
			s := exp.Format(time.RFC3339)
			entry.ExpiresAt = &s
			entry.Days = &days
			if minExp.IsZero() || exp.Before(minExp) {
				minExp = exp
			}
		} else {
			entry.Detail = "token 非 JWT 或无 exp(UUID 型/会话型)"
		}
		h.Accounts = append(h.Accounts, entry)
	}

	p.mu.Lock()
	for _, acct := range p.noauth {
		h.NoAuth++
		countActive(&h, acct.Status)
		add(acct)
	}
	for _, acct := range p.free {
		h.Free++
		countActive(&h, acct.Status)
		add(acct)
	}
	for _, acct := range p.puid {
		h.PUID++
		countActive(&h, acct.Status)
		add(acct)
	}
	p.mu.Unlock()

	p.tempMu.RLock()
	h.Temporary = len(p.temporary)
	for _, acct := range p.temporary {
		countActive(&h, acct.Status)
		add(acct)
	}
	p.tempMu.RUnlock()

	if !minExp.IsZero() {
		days := time.Until(minExp).Hours() / 24
		s := minExp.Format(time.RFC3339)
		h.MinAccessExpiresAt = &s
		h.MinAccessDays = &days
	}
	return h
}

func countActive(h *PoolHealth, s AccountStatus) {
	if s == StatusActive {
		h.Active++
	} else {
		h.Expired++
	}
}
