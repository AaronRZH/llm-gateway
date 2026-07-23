package ratelimit

import (
	"context"
	"testing"
)

func TestLocalLimiter_NewKeyAllowed(t *testing.T) {
	l := NewLocalLimiter(100, 1)
	if !l.Allow(context.Background(), "key1") {
		t.Error("expected new key to be allowed")
	}
}

func TestLocalLimiter_BurstExceeded(t *testing.T) {
	// 速率 0（不补充令牌），初始令牌 = burst = 2
	l := NewLocalLimiter(0, 2)
	if !l.Allow(context.Background(), "k") {
		t.Error("expected 1st allow")
	}
	if !l.Allow(context.Background(), "k") {
		t.Error("expected 2nd allow (burst)")
	}
	if l.Allow(context.Background(), "k") {
		t.Error("expected 3rd to be rejected (burst exhausted)")
	}
}

func TestLocalLimiter_IndependentKeys(t *testing.T) {
	l := NewLocalLimiter(0, 1)
	if !l.Allow(context.Background(), "a") {
		t.Error("expected a allowed")
	}
	// a 已用完其 burst，但 b 是独立桶
	if !l.Allow(context.Background(), "b") {
		t.Error("expected b allowed independently")
	}
}

func TestLocalLimiter_AllowDoesNotPanicWithNilContext(t *testing.T) {
	l := NewLocalLimiter(10, 5)
	// rate.Limiter.Allow 接受 context；传 nil 不应对实现产生问题
	_ = l.Allow(nil, "x")
}
