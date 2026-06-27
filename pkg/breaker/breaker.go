package breaker

import (
	"fmt"
	"time"

	"github.com/sony/gobreaker"
)

// New 创建熔断器
func New(name string, maxRequests uint32, failureThreshold int, cooldown time.Duration) *gobreaker.CircuitBreaker {
	return gobreaker.NewCircuitBreaker(gobreaker.Settings{
		Name:        name,
		MaxRequests: maxRequests,
		Interval:    10 * time.Second,
		Timeout:     5 * time.Second,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			// 失败次数达到阈值即熔断
			return counts.TotalFailures >= uint32(failureThreshold)
		},
		OnStateChange: func(name string, from gobreaker.State, to gobreaker.State) {
			fmt.Printf("[Breaker] %s: %s -> %s", name, stateName(from), stateName(to))
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
