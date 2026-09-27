package session

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeUpstream 是 acquire/release seam 的内存 fake:模拟上游 session 的
// 创建/关闭,记录调用轨迹供断言,无网络。
type fakeUpstream struct {
	mu       sync.Mutex
	created  int
	deleted  int
	sessions map[string]string // sessionID → token
}

func newFakeUpstream() *fakeUpstream {
	return &fakeUpstream{sessions: map[string]string{}}
}

func (f *fakeUpstream) CreateSession(token string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created++
	id := fmt.Sprintf("s%d", f.created)
	f.sessions[id] = token
	return id, nil
}

// currentDeleted 线程安全读取删除计数(测试主 goroutine vs cleanupLoop)。
func (f *fakeUpstream) currentDeleted() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deleted
}

func (f *fakeUpstream) DeleteSession(token, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted++
	delete(f.sessions, sessionID)
	return nil
}

// Cycle 2:同 clientKey 不同 model 不串池(缓存键 = clientKey + "|" + model)。
// 用两次 acquire 走不同 model,断言各自新开 session(fake 上游 created == 2)。
func TestAcquireDifferentModelsNoPoolSharing(t *testing.T) {
	up := newFakeUpstream()
	pool := NewPool(up, PoolConfig{})

	l1, err := pool.Acquire("u1", "deepseek", "tok")
	l2, err2 := pool.Acquire("u1", "deepseek-expert", "tok")
	if err != nil || err2 != nil {
		t.Fatalf("Acquire: %v / %v", err, err2)
	}

	if l1.SessionID == l2.SessionID {
		t.Fatalf("不同 model 应各自新开 session,got same %q", l1.SessionID)
	}
	if up.created != 2 {
		t.Fatalf("created = %d, want 2", up.created)
	}
}

// Cycle 3:同 key 同 model 连续 acquire 命中池 → 续轮(不新开、不删旧)。
func TestAcquireHitReusesSession(t *testing.T) {
	up := newFakeUpstream()
	pool := NewPool(up, PoolConfig{CleanupInterval: -1})

	l1, err := pool.Acquire("u1", "deepseek", "tok")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	l1.ParentMessageID = "resp-1" // 模拟首轮完成
	pool.Release(l1, "resp-1")

	l2, err := pool.Acquire("u1", "deepseek", "tok")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if l2.SessionID != l1.SessionID {
		t.Fatalf("同 key 同 model 第二轮应复用 session: l1=%q l2=%q", l1.SessionID, l2.SessionID)
	}
	if l2.ParentMessageID != "resp-1" {
		t.Fatalf("续轮 parent_message_id = %q, want resp-1", l2.ParentMessageID)
	}
	if up.created != 1 || up.deleted != 0 {
		t.Fatalf("created=%d deleted=%d, want 1/0", up.created, up.deleted)
	}
}

// Cycle 4a:TTL 惰性过期——空闲超时的条目在下一次 acquire 时被踢出,
// 视为新开(spec: TTL 过期即接受记忆断档,不重放历史)。
func TestAcquireTTLExpiredNewSession(t *testing.T) {
	up := newFakeUpstream()
	pool := NewPool(up, PoolConfig{CleanupInterval: -1, TTL: 10 * time.Millisecond})

	l1, err := pool.Acquire("u1", "deepseek", "tok")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	pool.Release(l1, "resp-1")
	time.Sleep(15 * time.Millisecond)

	l2, err := pool.Acquire("u1", "deepseek", "tok")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if l2.SessionID == l1.SessionID {
		t.Fatal("TTL 过期后应新开 session,却复用了旧 session")
	}
	if l2.ParentMessageID != "" {
		t.Fatalf("新开会话 parent 应为空, got %q", l2.ParentMessageID)
	}
	if up.created != 2 {
		t.Fatalf("created = %d, want 2", up.created)
	}
	if up.deleted != 1 {
		t.Fatalf("TTL 淘汰应顺手删上游 session: deleted = %d, want 1", up.deleted)
	}
}

// Cycle 4b:LRU 淘汰——容量满时最久未用条目被踢出,刚用过的保留。
func TestAcquireLRUEviction(t *testing.T) {
	up := newFakeUpstream()
	pool := NewPool(up, PoolConfig{Capacity: 2, CleanupInterval: -1})

	a, err := pool.Acquire("u1", "m", "tok")
	pool.Release(a, "r-a")
	b, err2 := pool.Acquire("u2", "m", "tok")
	pool.Release(b, "r-b")
	time.Sleep(2 * time.Millisecond)          // 保证 lastUsed 可比
	c, err3 := pool.Acquire("u3", "m", "tok") // 池满,u1 被淘汰
	if err != nil || err2 != nil || err3 != nil {
		t.Fatalf("Acquire: %v / %v / %v", err, err2, err3)
	}
	_ = c

	// u1 的条目已被淘汰 → 再次 acquire u1 应新开
	a2, err4 := pool.Acquire("u1", "m", "tok")
	if err4 != nil {
		t.Fatalf("Acquire: %v", err4)
	}
	if a2.SessionID == a.SessionID {
		t.Fatal("被 LRU 淘汰的条目不应被复用")
	}
	// LRU 语义自洽:acquire u1 时池又满,u2 成为最旧被淘汰 → b2 也新开。
	// created 计数 = 初始 3 + u1 重开 1 + u2 重开 1 = 5。
	b2, err5 := pool.Acquire("u2", "m", "tok")
	if err5 != nil {
		t.Fatalf("Acquire: %v", err5)
	}
	if b2.SessionID == b.SessionID {
		t.Fatal("u2 应已被 LRU 淘汰,不应复用")
	}
	if up.created != 5 {
		t.Fatalf("created = %d, want 5", up.created)
	}
	// 删除计数:evict u1(1) → evict u2(2) → u1 占坑后被 u2 挤出(3)。
	// 三次都是真实淘汰,deleted=3 是 LRU 链式挤出的正确结果。
	if up.deleted != 3 {
		t.Fatalf("LRU 链式淘汰应删上游: deleted = %d, want 3", up.deleted)
	}
}

// Cycle 5:显式信令 → 池内同 key 旧 entry 作废并顺手删上游 session
// (用户拍板「淘汰时顺手删」,信令作废属主动淘汰,三路径策略统一)。
func TestAcquireNewSignalInvalidates(t *testing.T) {
	up := newFakeUpstream()
	pool := NewPool(up, PoolConfig{CleanupInterval: -1})

	l1, err := pool.Acquire("u1", "deepseek", "tok")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	pool.Release(l1, "resp-1")

	l2, err := pool.AcquireNew("u1", "deepseek", "tok")
	if l2.SessionID == l1.SessionID {
		t.Fatal("显式新会话信令应强制新开,却复用了旧 session")
	}
	if l2.ParentMessageID != "" {
		t.Fatalf("新会话 parent 应为空, got %q", l2.ParentMessageID)
	}
	if up.created != 2 {
		t.Fatalf("created = %d, want 2", up.created)
	}
	if up.deleted != 1 {
		t.Fatalf("信令作废旧 entry 应顺手删上游 session: deleted = %d, want 1", up.deleted)
	}

	// 信令后的下一轮(无信令)正常复用新 session
	l3, err2 := pool.Acquire("u1", "deepseek", "tok")
	if err2 != nil {
		t.Fatalf("Acquire: %v", err2)
	}
	if l3.SessionID != l2.SessionID {
		t.Fatalf("信令后的下一轮应复用新 session: got %q want %q", l3.SessionID, l2.SessionID)
	}
}

// 并发冒烟:多 goroutine 同 key acquire/release 不死锁、不 panic、
// 池内条目数不超上限。
func TestPoolConcurrentAccess(t *testing.T) {
	up := newFakeUpstream()
	pool := NewPool(up, PoolConfig{Capacity: 4, CleanupInterval: -1})

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			key := fmt.Sprintf("u%d", n%6)
			l, err := pool.Acquire(key, "m", "tok")
			if err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			if n%3 == 0 {
				if _, err := pool.AcquireNew(key, "m", "tok"); err != nil {
					t.Errorf("AcquireNew: %v", err)
					return
				}
			}
			pool.Release(l, fmt.Sprintf("r-%d", n))
		}(i)
	}
	wg.Wait()

	pool.mu.Lock()
	size := len(pool.entries)
	pool.mu.Unlock()
	if size > 4 {
		t.Fatalf("池内条目 %d 超上限 4", size)
	}
}

// 后台清理循环也应顺手删上游 session(短间隔驱动,计时留余量)。
func TestCleanupLoopDeletesUpstream(t *testing.T) {
	up := newFakeUpstream()
	pool := NewPool(up, PoolConfig{CleanupInterval: 20 * time.Millisecond, TTL: 10 * time.Millisecond})

	l, err := pool.Acquire("u1", "m", "tok")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	pool.Release(l, "r-1")

	deadline := time.Now().Add(2 * time.Second)
	for up.currentDeleted() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if d := up.currentDeleted(); d != 1 {
		t.Fatalf("后台清理应删上游 session: deleted = %d, want 1", d)
	}
}

// Cycle 6(2026-09-27 live 验证暴露):租约建立必须用调用方本轮携带的 token。
// 修复前 newLease 硬编码空 token,带 X-Session-Key 的请求 100% 401/502。
func TestAcquirePassesTokenToUpstream(t *testing.T) {
	up := newFakeUpstream()
	pool := NewPool(up, PoolConfig{CleanupInterval: -1})

	l, err := pool.Acquire("u1", "deepseek", "tok-A")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if got := up.sessions[l.SessionID]; got != "tok-A" {
		t.Fatalf("上游建会话 token = %q, want tok-A", got)
	}
	if l.token != "tok-A" {
		t.Fatalf("租约绑定 token = %q, want tok-A", l.token)
	}
}

// Cycle 6b:token 轮换(spec「session 与建立时的 token 绑定」)→ 池内旧 entry
// 对持有新 token 的请求视为失效,降级新开(不拿旧 session 配新 token 发)。
func TestAcquireTokenRotationInvalidatesEntry(t *testing.T) {
	up := newFakeUpstream()
	pool := NewPool(up, PoolConfig{CleanupInterval: -1})

	l1, err := pool.Acquire("u1", "deepseek", "tok-A")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	pool.Release(l1, "resp-1")

	l2, err := pool.Acquire("u1", "deepseek", "tok-B")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if l2.SessionID == l1.SessionID {
		t.Fatal("token 轮换后旧 entry 应失效,不应复用")
	}
	if l2.ParentMessageID != "" {
		t.Fatalf("失效重开的 parent 应为空, got %q", l2.ParentMessageID)
	}
	if up.deleted != 1 {
		t.Fatalf("失效 entry 应顺手删上游 session: deleted = %d, want 1", up.deleted)
	}
	// 同 token 的后续请求正常复用新 entry。
	l3, err := pool.Acquire("u1", "deepseek", "tok-B")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if l3.SessionID != l2.SessionID {
		t.Fatalf("同 token 应复用新 session: got %q want %q", l3.SessionID, l2.SessionID)
	}
}
