package metrics

import (
	"fmt"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	// RequestTotal 请求总数
	RequestTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llm_gateway_requests_total",
			Help: "Total number of requests",
		},
		[]string{"method", "path", "status", "model"},
	)

	// RequestDuration 请求耗时
	RequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "llm_gateway_request_duration_seconds",
			Help:    "Request duration in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "path", "model"},
	)

	// UpstreamLatency 上游延迟
	UpstreamLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "llm_gateway_upstream_latency_seconds",
			Help:    "Upstream request latency in seconds",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		},
		[]string{"provider", "model"},
	)

	// TokenUsage token 用量
	TokenUsage = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llm_gateway_token_usage_total",
			Help: "Total token usage",
		},
		[]string{"model", "type"}, // type: prompt | completion
	)

	// CircuitBreakerState 熔断器状态
	CircuitBreakerState = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "llm_gateway_circuit_breaker_state",
			Help: "Circuit breaker state (0=closed, 1=open, 2=half-open)",
		},
		[]string{"provider", "model"},
	)

	// RateLimitHits 限流命中数
	RateLimitHits = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "llm_gateway_rate_limit_hits_total",
			Help: "Total number of rate limit hits",
		},
	)
)

func init() {
	prometheus.MustRegister(
		RequestTotal,
		RequestDuration,
		UpstreamLatency,
		TokenUsage,
		CircuitBreakerState,
		RateLimitHits,
	)
}

// Handler Prometheus metrics handler
func Handler() gin.HandlerFunc {
	h := promhttp.Handler()
	return func(c *gin.Context) {
		h.ServeHTTP(c.Writer, c.Request)
	}
}

// RecordRequest 记录请求级别指标 (RequestTotal + RequestDuration)
func RecordRequest(method, path string, status int, model string, duration float64) {
	RequestTotal.WithLabelValues(method, path, fmt.Sprintf("%d", status), model).Inc()
	RequestDuration.WithLabelValues(method, path, model).Observe(duration)
}

// RecordTokenUsage 记录 token 用量指标
func RecordTokenUsage(model string, input, output int) {
	TokenUsage.WithLabelValues(model, "input").Add(float64(input))
	TokenUsage.WithLabelValues(model, "output").Add(float64(output))
}
