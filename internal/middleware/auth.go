package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"llm-gateway/internal/auth"
)

// Auth API Key 认证中间件
// 支持白名单路径（如 /health、/metrics）跳过认证
func Auth(authService *auth.Service, publicPaths ...string) gin.HandlerFunc {
	publicPathSet := make(map[string]struct{}, len(publicPaths))
	for _, p := range publicPaths {
		publicPathSet[p] = struct{}{}
	}
	return func(c *gin.Context) {
		if _, skip := publicPathSet[c.Request.URL.Path]; skip {
			c.Next()
			return
		}

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
