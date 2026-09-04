package accounts

import (
	"net/http"
	"strings"
	"time"
)

// E1(可靠性计划/整合路线):错误分类状态机 —— 把上游返回的 HTTP 状态码/错误体
// 归类为账号生命周期事件,启用闲置的 StatusRateLimited / StatusBanned。
//
// 设计约束:
//   - 只依据状态码与错误体关键词,不做请求重试(重试是调用方的事)
//   - 熔断(冷却)在内存中做,进程重启即清零 —— 冷却窗口短(分钟级),持久化无意义
//   - 不改变既有 StatusExpired 的续期路径(healthy check / ensureToken 自愈)

// FailureKind 上游错误的归类。
type FailureKind string

const (
	// KindAuth 凭证失效(401/403):清票重换发可自愈
	KindAuth FailureKind = "auth"
	// KindRateLimited 限流(429 / code 6000):账号进冷却,不换发
	KindRateLimited FailureKind = "rate_limited"
	// KindBanned 风控封禁(11128 渠道拦截 / 40300 / 明确封禁文案):账号长冷却
	KindBanned FailureKind = "banned"
	// KindOverload 上游过载(500/502/503/11134):与账号无关,不应标记账号
	KindOverload FailureKind = "overload"
	// KindInput 请求本身有问题(400/11115 input too long):换账号也没用
	KindInput FailureKind = "input"
	// KindTransient 网络类(超时/连接重置):轻处理
	KindTransient FailureKind = "transient"
	// KindUnknown 无法归类:保守按 Transient 处理但不标记
	KindUnknown FailureKind = "unknown"
)

// cooldownByKind 各类失败的冷却时长。零值 = 不冷却(只记计数)。
var cooldownByKind = map[FailureKind]time.Duration{
	KindRateLimited: 5 * time.Minute,
	KindBanned:      30 * time.Minute,
}

// ClassifyFailure 从 HTTP 状态码与响应体片段归类失败。
// errBody 可为空;关键词命中优先于状态码(腾讯的 400 也可能是风控)。
func ClassifyFailure(statusCode int, errBody string) FailureKind {
	body := strings.ToLower(errBody)
	has := func(ss ...string) bool {
		for _, s := range ss {
			if strings.Contains(body, s) {
				return true
			}
		}
		return false
	}

	// 腾讯系业务错误码(错误体 JSON 的 "code" 字段)
	switch {
	case has("11128", "unapproved channel"):
		return KindBanned // 渠道风控
	case has("11115", "input length too long"):
		return KindInput
	case has("11134", "temporarily unavailable"):
		return KindOverload
	case has(`"code":6000`, "超出频率限制", "频率限制"):
		return KindRateLimited
	}

	switch statusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return KindAuth
	case http.StatusTooManyRequests:
		return KindRateLimited
	case http.StatusBadRequest:
		// 400 但无已知错误码:请求侧问题,换账号无用
		return KindInput
	case http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, 529:
		return KindOverload
	case 0:
		return KindTransient // 网络层错误(无 HTTP 状态)
	default:
		return KindUnknown
	}
}

// ReportResult 是 Pool 对一次调用结果的统一入口(E1):
//   - 按 ClassifyFailure 归类并设置账号状态
//   - RateLimited/Banned 进冷却(cooldownUntil),Acquire 跳过冷却中的账号
//   - Auth 类仅过期(交还既有续期自愈),不冷却
//   - Overload/Input/Transient 不改账号状态(与账号无关,标记了反而误伤)
//
// 返回值:本次是否改变了账号状态。
func (p *Pool) ReportResult(acct *Account, statusCode int, errBody string) bool {
	if acct == nil {
		return false
	}
	kind := ClassifyFailure(statusCode, errBody)

	p.mu.Lock()
	defer p.mu.Unlock()

	switch kind {
	case KindAuth:
		if acct.Status != StatusExpired {
			acct.Status = StatusExpired
			acct.FailedCalls++
			return true
		}
		return false
	case KindRateLimited, KindBanned:
		cd := cooldownByKind[kind]
		until := time.Now().Add(cd)
		// 已在更长的冷却期内,不缩短
		if acct.cooldownUntil.After(until) {
			return false
		}
		acct.Status = StatusRateLimited
		if kind == KindBanned {
			acct.Status = StatusBanned
		}
		acct.cooldownUntil = until
		acct.FailedCalls++
		return true
	default:
		// Overload/Input/Transient:不标记账号
		return false
	}
}

// inCooldown 报告账号是否处于冷却期(Acquire 跳过用)。
func (a *Account) inCooldown() bool {
	return !a.cooldownUntil.IsZero() && time.Now().Before(a.cooldownUntil)
}
