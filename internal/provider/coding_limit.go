package provider

import (
	"math/rand"
	"sync"
	"time"
)

// CodingLimiter 只对 coding 变体限频(agent 驱动的连发工具调用)。
//
// 策略(用户拍板):chat 不限频 —— 真人使用,天然有人类节奏,限制只会拖慢真人;
// coding 限频 —— agent 会连发工具调用,突发流量是风控/封号主因。
// 全局串行:同 provider 的所有 coding 请求排队,间隔 = base + rand(0..jitter),
// 随机抖动使节奏不像机器人。多账号也不会绕过(保守优先,安全第一)。
type CodingLimiter struct {
	mu      sync.Mutex
	base    time.Duration
	jitter  time.Duration
	lastReq time.Time
}

// NewCodingLimiter 构造限频器。base=基础间隔,jitter=随机抖动上限。
func NewCodingLimiter(base, jitter time.Duration) *CodingLimiter {
	return &CodingLimiter{base: base, jitter: jitter}
}

// Wait 阻塞到距上次 coding 请求 >= base + rand(0..jitter)。
func (l *CodingLimiter) Wait() {
	l.mu.Lock()
	defer l.mu.Unlock()
	target := l.base + time.Duration(rand.Int63n(int64(l.jitter)+1))
	if wait := target - time.Since(l.lastReq); wait > 0 {
		time.Sleep(wait)
	}
	l.lastReq = time.Now()
}
