package middleware

import (
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"

	"llm-gateway/internal/config"
)

// RateLimit 限流中间件
func RateLimit(cfg config.RateLimitConfig) gin.HandlerFunc {
	if !cfg.Enabled {
		return func(c *gin.Context) { c.Next() }
	}

	// 按 API Key 限流（简单实现，生产环境建议用 Redis 分布式限流）
	limiters := &sync.Map{}

	return func(c *gin.Context) {
		apiKey := c.GetHeader("Authorization")
		if apiKey == "" {
			apiKey = c.ClientIP()
		}

		val, _ := limiters.LoadOrStore(apiKey, rate.NewLimiter(rate.Limit(cfg.RequestsPerSecond), cfg.Burst))
		limiter := val.(*rate.Limiter)

		if !limiter.Allow() {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": "rate limit exceeded",
			})
			return
		}

		c.Next()
	}
}
