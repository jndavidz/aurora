package provider

import (
	"testing"
	"time"
)

func TestBreakerTripsAfterThreshold(t *testing.T) {
	b := NewProviderBreaker()
	b.FailThreshold = 3
	b.Cooldown = 50 * time.Millisecond

	name := "test-provider"
	if b.Tripped(name) {
		t.Fatal("初始状态不应熔断")
	}
	b.RecordFailure(name, "502")
	b.RecordFailure(name, "502")
	if b.Tripped(name) {
		t.Fatal("2 次失败不应熔断(阈值 3)")
	}
	if tripped := b.RecordFailure(name, "502"); !tripped {
		t.Error("第 3 次失败应触发熔断")
	}
	if !b.Tripped(name) {
		t.Error("触发后应处于摘除状态")
	}
}

func TestBreakerCooldownRecovery(t *testing.T) {
	b := NewProviderBreaker()
	b.FailThreshold = 1
	b.Cooldown = 30 * time.Millisecond

	name := "cooldown-test"
	b.RecordFailure(name, "boom")
	if !b.Tripped(name) {
		t.Fatal("阈值 1 次即熔断")
	}
	time.Sleep(40 * time.Millisecond)
	if b.Tripped(name) {
		t.Error("冷却到期应放行(半开)")
	}
}

func TestBreakerSuccessResets(t *testing.T) {
	b := NewProviderBreaker()
	b.FailThreshold = 2

	name := "reset-test"
	b.RecordFailure(name, "err1")
	b.RecordSuccess(name)
	if b.Tripped(name) {
		t.Fatal("成功后不应熔断")
	}
	// 成功清零后,1 次失败不应触发(阈值 2)
	if tripped := b.RecordFailure(name, "err2"); tripped {
		t.Error("成功清零后 1 次失败不应熔断")
	}
}

func TestBreakerNilSafe(t *testing.T) {
	var b *ProviderBreaker
	if b.Tripped("x") {
		t.Error("nil breaker 不应熔断")
	}
	b.RecordSuccess("x")      // 不应 panic
	b.RecordFailure("x", "y") // 不应 panic
	if b.Status() != nil {
		t.Error("nil breaker Status 应为 nil")
	}
}

func TestBreakerStatusSnapshot(t *testing.T) {
	b := NewProviderBreaker()
	b.FailThreshold = 1
	b.Cooldown = time.Second

	b.RecordFailure("svc-a", "err detail A")
	b.RecordSuccess("svc-b")

	st := b.Status()
	if len(st) == 0 {
		t.Fatal("Status 应含条目")
	}
	a, ok := st["svc-a"]
	if !ok || !a.Tripped || a.LastError != "err detail A" {
		t.Errorf("svc-a 状态不对: %+v", a)
	}
	if _, ok := st["svc-b"]; ok {
		t.Error("svc-b 成功清零后不应出现在快照中")
	}
}
