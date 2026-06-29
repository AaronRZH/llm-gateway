package breaker

import (
	"fmt"
	"time"

	"github.com/sony/gobreaker"
)

// Settings 熔断器配置参数
type Settings struct {
	MaxRequests      uint32
	Interval         time.Duration
	Timeout          time.Duration
	FailureThreshold int
	Cooldown         time.Duration
}

// New 创建熔断器
func New(name string, s Settings) *gobreaker.CircuitBreaker {
	return gobreaker.NewCircuitBreaker(gobreaker.Settings{
		Name:        name,
		MaxRequests: s.MaxRequests,
		Interval:    s.Interval,
		Timeout:     s.Timeout,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			return counts.TotalFailures >= uint32(s.FailureThreshold)
		},
		OnStateChange: func(name string, from gobreaker.State, to gobreaker.State) {
			fmt.Printf("[Breaker] %s: %s -> %s\n", name, stateName(from), stateName(to))
		},
	})
}

func stateName(s gobreaker.State) string {
	switch s {
	case gobreaker.StateClosed:
		return "closed"
	case gobreaker.StateOpen:
		return "open"
	case gobreaker.StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}
