package provider

import (
	"testing"
	"time"
)

// TestCodingLimiter 验证:首次不等待,后续请求间隔 >= base;抖动为 0 时精确可测。
func TestCodingLimiter(t *testing.T) {
	base := 300 * time.Millisecond
	l := NewCodingLimiter(base, 0)

	start := time.Now()
	l.Wait()
	if first := time.Since(start); first > 100*time.Millisecond {
		t.Errorf("first Wait should not block, took %v", first)
	}

	l.Wait()
	if elapsed := time.Since(start); elapsed < base {
		t.Errorf("second Wait should wait >= base, elapsed %v < %v", elapsed, base)
	}

	// 抖动上限:多次采样应落在 [base, base+jitter]
	jitter := 100 * time.Millisecond
	lj := NewCodingLimiter(base, jitter)
	lj.Wait()
	for i := 0; i < 20; i++ {
		s := time.Now()
		lj.Wait()
		elapsed := time.Since(s)
		if elapsed < base || elapsed > base+jitter+20*time.Millisecond {
			t.Fatalf("jittered wait out of range: %v (want [%v,%v])", elapsed, base, base+jitter)
		}
	}
}
