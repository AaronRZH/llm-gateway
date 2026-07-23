package breaker

import (
	"testing"

	"github.com/sony/gobreaker"
)

func TestStateName(t *testing.T) {
	cases := []struct {
		state gobreaker.State
		want  string
	}{
		{gobreaker.StateClosed, "closed"},
		{gobreaker.StateOpen, "open"},
		{gobreaker.StateHalfOpen, "half-open"},
		{gobreaker.State(999), "unknown"},
	}
	for _, c := range cases {
		if got := stateName(c.state); got != c.want {
			t.Errorf("stateName(%v) = %q, want %q", c.state, got, c.want)
		}
	}
}

func TestNew(t *testing.T) {
	cb := New("test", Settings{
		MaxRequests:      5,
		Interval:         0,
		Timeout:          0,
		FailureThreshold: 3,
		Cooldown:         0,
	})
	if cb == nil {
		t.Fatal("expected non-nil circuit breaker")
	}
	if cb.Name() != "test" {
		t.Errorf("expected name test, got %q", cb.Name())
	}
	// 初始状态应为 closed
	if cb.State() != gobreaker.StateClosed {
		t.Errorf("expected closed, got %v", cb.State())
	}
}
