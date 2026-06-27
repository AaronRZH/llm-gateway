package middleware

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog/log"
)

// Logger zerolog 中间件
func Logger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		raw := c.Request.URL.RawQuery

		c.Next()

		latency := time.Since(start)
		status := c.Writer.Status()

		logger := log.Info()
		if status >= 400 {
			logger = log.Warn()
		}
		if status >= 500 {
			logger = log.Error()
		}

		if raw != "" {
			path = path + "?" + raw
		}

		logger.
			Str("method", c.Request.Method).
			Str("path", path).
			Int("status", status).
			Dur("latency", latency).
			Str("client_ip", c.ClientIP()).
			Str("request_id", c.GetString("request_id")).
			Msg("request")
	}
}
