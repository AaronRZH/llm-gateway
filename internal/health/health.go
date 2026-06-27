package health

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Handler 健康检查 handler
func Handler() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status":  "ok",
			"service": "llm-gateway",
		})
	}
}
