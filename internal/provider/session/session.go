// Package session 提供「上游会话复用」的策略 module。
//
// aurora 定位是网页反代:不维护对话记忆、不校验客户端历史。本 module 只负责
// 一个问题——同一客户端的连续请求何时复用已建立的上游 session、何时新开。
// 池化、TTL、信令、失效降级的全部 implementation 吸收在 acquire/release
// 小接口之后,provider 侧通过可选能力(SessionAware)接入,不感知内部细节。
//
// spec: .scratch/upstream-session-reuse/spec.md
package session

import (
	"fmt"
	"log"
	"sync"
	"time"
)

// newSignalPrefix 是 chat completions user 字段中「强制新会话」的信令前缀。
// 剥除后剩余部分作 clientKey;仅前缀(空 key)视为无 key 但信令仍生效。
const newSignalPrefix = "#new#"

// ResolveClientKey 按固定优先级解析会话键与显式新会话信令:
//  1. X-Session-Key 请求头(信令 = 前缀 #new#)
//  2. X-Session-Action 请求头(信令 = 值 "new")
//  3. chat completions user 字段(信令 = 前缀 #new#)
//  4. 皆空 → 空串(调用方不进池)
//
// 返回 (clientKey, isNew):isNew 为 true 表示客户端显式要求新开会话,
// 池内同 key 旧 entry 应作废。多通道信令叠加为 OR(任一生效)。
func ResolveClientKey(keyHeader, actionHeader, userField string) (string, bool) {
	// 通道 1:X-Session-Key 头(信令 = 前缀 #new#)
	key, isNew := "", false
	if keyHeader != "" {
		key, isNew = stripNewSignal(keyHeader)
	}
	// 通道 2:X-Session-Action: new 头
	if actionHeader == "new" {
		isNew = true
	}
	// 通道 3:user 字段;头已给 key 时,信令仍可由 user 通道叠加
	if userField != "" {
		uKey, uNew := stripNewSignal(userField)
		if key == "" {
			key = uKey
		}
		isNew = isNew || uNew
	}
	return key, isNew
}

// stripNewSignal 剥除 #new# 前缀,返回 (剩余内容, 是否带信令)。
func stripNewSignal(s string) (string, bool) {
	if len(s) >= len(newSignalPrefix) && s[:len(newSignalPrefix)] == newSignalPrefix {
		return s[len(newSignalPrefix):], true
	}
	return s, false
}

// PoolConfig 是池的可调参数;零值即生产默认(上限 16、TTL 30min)。
type PoolConfig struct {
	// Capacity 池上限,超出按 LRU 淘汰。0 = 默认 16。
	Capacity int
	// TTL 条目空闲超过此时长视为过期。0 = 默认 30min。
	TTL time.Duration
	// CleanupInterval 后台清理循环的扫描间隔。0 = 默认 10min;
	// 负值禁用后台循环(测试用,过期判定走惰性检查)。
	CleanupInterval time.Duration
}

// Lease 是一次续轮租约:上游 session 的句柄 + 续轮所需的父消息锚点。
type Lease struct {
	SessionID       string
	ParentMessageID string // 上轮 response_message_id;新开时为空
	token           string // 建立时绑定的 token
	key             string // 缓存键
}

// upstream 是 session 通道的最小抽象(DeepSeek adapter 实现;GLM/Grok/Minimax
// 协议同型,二期跟进)。fake 实现用于无网络测试。
type upstream interface {
	CreateSession(token string) (string, error)
	DeleteSession(token, sessionID string) error
}

// Pool 是会话池 deep module:acquire 吸收命中判断/TTL/信令/淘汰全部细节,
// release 归还租约并把本轮 response_message_id 写回作下轮 parent。
// 线程安全(参照 handler 层 SessionManager 的 mu+map 模式)。
type Pool struct {
	mu      sync.Mutex
	up      upstream
	cfg     PoolConfig
	entries map[string]*poolEntry
}

// poolEntry 是池内一个活 session。lastUsed 驱动惰性 TTL 与 LRU。
type poolEntry struct {
	sid      string
	token    string
	parentID string // 上轮 response_message_id
	lastUsed time.Time
}

// 生产默认值(spec 拍板)。
const (
	defaultCapacity = 16
	defaultTTL      = 30 * time.Minute
)

// NewPool 构造会话池。up 为该上游的 session 通道实现。
func NewPool(up upstream, cfg PoolConfig) *Pool {
	if cfg.Capacity <= 0 {
		cfg.Capacity = defaultCapacity
	}
	if cfg.TTL <= 0 {
		cfg.TTL = defaultTTL
	}
	p := &Pool{up: up, cfg: cfg, entries: map[string]*poolEntry{}}
	if cfg.CleanupInterval > 0 {
		go p.cleanupLoop()
	}
	return p
}

// cacheKey 最终缓存键:clientKey + 模型 id。不同 model 不串池。
func cacheKey(clientKey, modelID string) string {
	return clientKey + "|" + modelID
}

// Acquire 取一条续轮租约。命中池内活 entry(未过 TTL)则续轮
// (parent = 上轮 response_message_id);未命中/TTL 过期则新开。
// 上游建会话失败返回 error(调用方转 502),不 panic。
func (p *Pool) Acquire(clientKey, modelID string) (*Lease, error) {
	ck := cacheKey(clientKey, modelID)
	p.mu.Lock()
	var victim *poolEntry
	if e, ok := p.entries[ck]; ok {
		if time.Since(e.lastUsed) <= p.cfg.TTL {
			e.lastUsed = time.Now()
			p.mu.Unlock()
			return &Lease{SessionID: e.sid, ParentMessageID: e.parentID, token: e.token, key: ck}, nil
		}
		// TTL 过期:摘出旧条目(条目已不在池内,无人能再取到)。
		victim = e
		delete(p.entries, ck)
	}
	p.mu.Unlock()
	if victim != nil {
		p.deleteUpstream(victim) // 用户拍板:淘汰顺手删,不悬挂;锁外执行
	}
	l, err := p.newLease(clientKey, modelID, "")
	if err != nil {
		return nil, err
	}
	// 新开即占坑:容量满时先按 LRU 淘汰(含可能的本键 TTL 过期残留)。
	// 淘汰挂在 acquire 而非 release:未 release 的在途租约不占池位,
	// 同一 key 并发新开时后者 release 覆盖前者(最后写入胜)。
	p.mu.Lock()
	evicted := p.evictIfNeeded(ck)
	p.entries[ck] = &poolEntry{sid: l.SessionID, token: l.token, lastUsed: time.Now()}
	p.mu.Unlock()
	if evicted != nil {
		p.deleteUpstream(evicted)
	}
	return l, nil
}

// AcquireNew 显式新会话:作废池内同 key 旧 entry 后强制新开。
// 信令解析统一走 ResolveClientKey,本层只负责作废语义。
func (p *Pool) AcquireNew(clientKey, modelID string) (*Lease, error) {
	ck := cacheKey(clientKey, modelID)
	p.mu.Lock()
	victim := p.removeEntryLocked(ck)
	p.mu.Unlock()
	if victim != nil {
		p.deleteUpstream(victim)
	}
	return p.Acquire(clientKey, modelID)
}

// Discard 丢弃失败租约:条目不入池,上游 session 顺手删(ticket 03 的失败
// 降级依赖此语义:失败续轮不留下已失效的条目)。删除失败仅记日志。
func (p *Pool) Discard(l *Lease) {
	if l == nil {
		return
	}
	p.mu.Lock()
	delete(p.entries, l.key)
	p.mu.Unlock()
	p.deleteUpstream(&poolEntry{sid: l.SessionID, token: l.token})
}

// release 归还租约:池化条目供下轮续轮,并把本轮 response_message_id
// 写回作下轮 parent。新会话引导路径的租约同样归还(本轮成功即入池)。
// 容量满时 LRU 淘汰的旧条目同样摘出后锁外删上游。
func (p *Pool) Release(l *Lease, responseMessageID string) {
	if l == nil {
		return
	}
	p.mu.Lock()
	evicted := p.evictIfNeeded(l.key)
	p.entries[l.key] = &poolEntry{sid: l.SessionID, token: l.token, parentID: responseMessageID, lastUsed: time.Now()}
	p.mu.Unlock()
	if evicted != nil {
		p.deleteUpstream(evicted)
	}
}

// evictIfNeeded 容量满时按 LRU 淘汰最旧条目,返回被摘出的条目(可能为
// nil,调用方锁外删上游)。不触上游、不阻塞在网络 I/O 上。调用方需持锁。
func (p *Pool) evictIfNeeded(key string) *poolEntry {
	if _, exists := p.entries[key]; exists {
		return nil
	}
	if len(p.entries) < p.cfg.Capacity {
		return nil
	}
	var oldestKey string
	var oldest time.Time
	first := true
	for k, e := range p.entries {
		if first || e.lastUsed.Before(oldest) {
			oldestKey, oldest, first = k, e.lastUsed, false
		}
	}
	e := p.entries[oldestKey]
	delete(p.entries, oldestKey)
	return e
}

// removeEntryLocked 摘出指定条目(调用方锁外删上游)。需持锁。
func (p *Pool) removeEntryLocked(key string) *poolEntry {
	e, ok := p.entries[key]
	if ok {
		delete(p.entries, key)
	}
	return e
}

// deleteUpstream 调上游删除 session。失败仅记日志(条目已作废,
// 下轮降级新开;不因删除失败阻塞主路径)。
func (p *Pool) deleteUpstream(e *poolEntry) {
	if e == nil {
		return
	}
	if err := p.up.DeleteSession(e.token, e.sid); err != nil {
		log.Printf("[session-pool] 删除上游 session %s 失败: %v", e.sid, err)
	}
}

// newLease 新开上游 session 并构造租约。token 为建立时绑定的凭证;
// 失败透传 error(ticket 03 的降级重试建立在它之上)。
func (p *Pool) newLease(clientKey, modelID, token string) (*Lease, error) {
	sid, err := p.up.CreateSession(token)
	if err != nil {
		return nil, fmt.Errorf("session create: %w", err)
	}
	return &Lease{SessionID: sid, token: token, key: cacheKey(clientKey, modelID)}, nil
}

// cleanupLoop 后台扫描过期条目(参照 handler 层 SessionManager 模式)。
// 锁内只摘条目,锁外删上游——网络 I/O 不持池锁。
func (p *Pool) cleanupLoop() {
	ticker := time.NewTicker(p.cfg.CleanupInterval)
	defer ticker.Stop()
	for range ticker.C {
		p.mu.Lock()
		now := time.Now()
		var victims []*poolEntry
		for k, e := range p.entries {
			if now.Sub(e.lastUsed) > p.cfg.TTL {
				victims = append(victims, e)
				delete(p.entries, k)
			}
		}
		n := len(victims)
		p.mu.Unlock()
		for _, e := range victims {
			p.deleteUpstream(e)
		}
		if n > 0 {
			log.Printf("[session-pool] 清理过期 session %d 个", n)
		}
	}
}
