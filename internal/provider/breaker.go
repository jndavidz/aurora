package provider

import (
	"sync"
	"time"
)

// ProviderBreaker Registry 级 provider 熔断器(E1/G2):
//   - 某 provider 连续失败达阈值后短暂摘除(Resolve 跳过),冷却到期自动恢复
//   - 目的:让 handler 在 Resolve 命中坏 provider 时能立刻 fallback 到 ChatGPT 兜底,
//     而不是每轮都去撞一个已知故障的上游(省 502 前的建连/超时等待)
//   - 与账号级冷却(accounts.Pool.ReportResult)互补:那是账号粒度,这是 provider 粒度
//
// 线程安全:chat_handler 并发调用,全操作走 mu。
// 恢复策略:冷却到期即放行一次(半开),成功则清零,失败则重新计时。
type ProviderBreaker struct {
	mu        sync.Mutex
	failures  map[string]int       // name -> 连续失败数
	deadUntil map[string]time.Time // name -> 摘除截止
	lastErr   map[string]string    // name -> 最近错误摘要(观测用)

	FailThreshold int           // 连续失败阈值(默认 3)
	Cooldown      time.Duration // 摘除时长(默认 60s)
}

func NewProviderBreaker() *ProviderBreaker {
	return &ProviderBreaker{
		failures:      make(map[string]int),
		deadUntil:     make(map[string]time.Time),
		lastErr:       make(map[string]string),
		FailThreshold: 3,
		Cooldown:      60 * time.Second,
	}
}

// Tripped 报告该 provider 是否处于熔断摘除状态。
// Resolve 前调用;true = 跳过此 provider。
func (b *ProviderBreaker) Tripped(name string) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if until, ok := b.deadUntil[name]; ok {
		if time.Now().Before(until) {
			return true
		}
		// 冷却到期:半开,放行
		delete(b.deadUntil, name)
	}
	return false
}

// RecordSuccess 记录一次成功:清零连续失败计数并解除摘除。
func (b *ProviderBreaker) RecordSuccess(name string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.failures, name)
	delete(b.deadUntil, name)
	delete(b.lastErr, name)
}

// RecordFailure 记录一次失败:连续计数达阈值则摘除 Cooldown 时长。
// 返回 true 表示本次触发了熔断(观测/告警用)。
func (b *ProviderBreaker) RecordFailure(name, errSummary string) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if errSummary != "" {
		b.lastErr[name] = errSummary
	}
	b.failures[name]++
	if b.failures[name] >= b.FailThreshold {
		b.deadUntil[name] = time.Now().Add(b.Cooldown)
		return true
	}
	return false
}

// Status 返回熔断状态快照(观测/健康端点用)。
func (b *ProviderBreaker) Status() map[string]BreakerState {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]BreakerState, len(b.failures))
	for name, n := range b.failures {
		st := BreakerState{ConsecutiveFails: n}
		if until, ok := b.deadUntil[name]; ok && time.Now().Before(until) {
			st.Tripped = true
			st.Until = until
		}
		st.LastError = b.lastErr[name]
		out[name] = st
	}
	return out
}

// BreakerState 单个 provider 的熔断状态。
type BreakerState struct {
	ConsecutiveFails int
	Tripped          bool
	Until            time.Time
	LastError        string
}
