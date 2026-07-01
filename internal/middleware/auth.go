package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"llm-gateway/internal/auth"
)

// Auth API Key 认证中间件
// 支持白名单路径（如 /health、/metrics）跳过认证
// 以 /* 结尾的路径作为前缀匹配（如 /v1/usage/* 匹配 /v1/usage/xxx）
func Auth(authService *auth.Service, publicPaths ...string) gin.HandlerFunc {
	exactSet := make(map[string]struct{}, len(publicPaths))
	prefixes := make([]string, 0, len(publicPaths))
	for _, p := range publicPaths {
		if len(p) > 2 && p[len(p)-2:] == "/*" {
			prefixes = append(prefixes, p[:len(p)-2])
		} else {
			exactSet[p] = struct{}{}
		}
	}
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		if _, skip := exactSet[path]; skip {
			c.Next()
			return
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(path, prefix) {
				c.Next()
				return
			}
		}

		// 支持 Authorization: Bearer <key>（OpenAI 格式）和 x-api-key: <key>（Anthropic 格式）
		authHeader := c.GetHeader("Authorization")
		apiKey := ""
		if authHeader != "" {
			parts := strings.SplitN(authHeader, " ", 2)
			if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid authorization format"})
				return
			}
			apiKey = parts[1]
		} else {
			apiKey = c.GetHeader("x-api-key")
		}

		if apiKey == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing authorization header"})
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
