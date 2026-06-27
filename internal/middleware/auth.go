package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"llm-gateway/internal/auth"
)

// Auth API Key 认证中间件
// 验证 Bearer Token 有效性（查 Redis + 本地种子 Key）
func Auth(authService *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing authorization header"})
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid authorization format"})
			return
		}

		apiKey := parts[1]
		if apiKey == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "empty api key"})
			return
		}

		// 验证 API Key 有效性（本地缓存 → 种子 Key → Redis）
		info, ok := authService.Validate(apiKey)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid api key"})
			return
		}

		// 将验证通过的 KeyInfo 存入上下文
		c.Set("api_key", apiKey)
		c.Set("api_key_info", info)
		c.Next()
	}
}
