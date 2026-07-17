package middleware

import (
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"llm-gateway/internal/config"
)

// AdminAuth 管理后台认证中间件（支持 JWT Bearer）
func AdminAuth(cfg *config.Config) gin.HandlerFunc {
	password := cfg.Admin.Password
	jwtSecret := cfg.Admin.JWTSecret

	// Basic Auth 预计算（兼容保留）
	var plainPass string
	decoded, err := base64.StdEncoding.DecodeString(password)
	if err == nil && len(decoded) >= 4 {
		plainPass = string(decoded)
	} else {
		plainPass = password
	}
	expectedBasic := "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:"+plainPass))

	return func(c *gin.Context) {
		if c.Request.Method == "OPTIONS" {
			c.Next()
			return
		}

		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.Header("WWW-Authenticate", `Bearer realm="Admin"`)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}

		// 1. 尝试 JWT Bearer token
		if strings.HasPrefix(authHeader, "Bearer ") {
			tokenStr := strings.TrimSpace(authHeader[7:])
			token, err := jwt.Parse(tokenStr, func(token *jwt.Token) (interface{}, error) {
				if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
					return nil, jwt.ErrSignatureInvalid
				}
				return []byte(jwtSecret), nil
			})
			if err == nil && token.Valid {
				if claims, ok := token.Claims.(jwt.MapClaims); ok {
					if claims["sub"] == "admin" {
						c.Next()
						return
					}
				}
			}
			c.Header("WWW-Authenticate", `Bearer realm="Admin", error="invalid_token"`)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			return
		}

		// 2. 兼容 Basic Auth（仅当配置了密码时）
		if password != "" && strings.HasPrefix(authHeader, "Basic ") {
			authStr := strings.TrimSpace(authHeader[6:])
			if authStr == expectedBasic {
				c.Next()
				return
			}
			decoded, err := base64.StdEncoding.DecodeString(authStr)
			if err == nil {
				parts := strings.SplitN(string(decoded), ":", 2)
				if len(parts) == 2 && parts[0] == "admin" && parts[1] == plainPass {
					c.Next()
					return
				}
			}
		}

		c.Header("WWW-Authenticate", `Bearer realm="Admin"`)
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid authorization format"})
	}
}

// GenerateAdminToken 生成管理后台 JWT token
func GenerateAdminToken(secret string, expiry time.Duration) (string, int64, error) {
	now := time.Now()
	exp := now.Add(expiry)
	claims := jwt.MapClaims{
		"sub": "admin",
		"iat": now.Unix(),
		"exp": exp.Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenStr, err := token.SignedString([]byte(secret))
	if err != nil {
		return "", 0, err
	}
	return tokenStr, exp.Unix(), nil
}
