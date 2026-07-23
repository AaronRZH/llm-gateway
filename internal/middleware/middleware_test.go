package middleware

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/config"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func doRequest(engine *gin.Engine, method, path, authHeader string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

// ==================== CORS ====================

func TestCORS_Get(t *testing.T) {
	engine := gin.New()
	engine.Use(CORS())
	engine.GET("/x", func(c *gin.Context) { c.String(200, "ok") })

	rec := doRequest(engine, "GET", "/x", "")
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("expected CORS origin header")
	}
}

func TestCORS_Options(t *testing.T) {
	engine := gin.New()
	engine.Use(CORS())
	engine.GET("/x", func(c *gin.Context) { c.String(200, "ok") })

	rec := doRequest(engine, "OPTIONS", "/x", "")
	if rec.Code != 204 {
		t.Fatalf("expected 204 for OPTIONS, got %d", rec.Code)
	}
}

// ==================== Auth ====================

func newAuthService() *auth.Service {
	return auth.New(nil, map[string]*auth.KeyInfo{
		"sk-valid": {Key: "sk-valid", Name: "user"},
	})
}

func TestAuth_ValidBearer(t *testing.T) {
	engine := gin.New()
	engine.Use(Auth(newAuthService()))
	engine.GET("/p", func(c *gin.Context) { c.String(200, "ok") })

	rec := doRequest(engine, "GET", "/p", "Bearer sk-valid")
	if rec.Code != 200 {
		t.Errorf("expected 200 for valid bearer, got %d", rec.Code)
	}
}

func TestAuth_XApiKey(t *testing.T) {
	engine := gin.New()
	engine.Use(Auth(newAuthService()))
	engine.GET("/p", func(c *gin.Context) { c.String(200, "ok") })

	req := httptest.NewRequest("GET", "/p", nil)
	req.Header.Set("x-api-key", "sk-valid")
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("expected 200 for valid x-api-key, got %d", rec.Code)
	}
}

func TestAuth_MissingHeader(t *testing.T) {
	engine := gin.New()
	engine.Use(Auth(newAuthService()))
	engine.GET("/p", func(c *gin.Context) { c.String(200, "ok") })

	rec := doRequest(engine, "GET", "/p", "")
	if rec.Code != 401 {
		t.Errorf("expected 401 for missing header, got %d", rec.Code)
	}
}

func TestAuth_InvalidKey(t *testing.T) {
	engine := gin.New()
	engine.Use(Auth(newAuthService()))
	engine.GET("/p", func(c *gin.Context) { c.String(200, "ok") })

	rec := doRequest(engine, "GET", "/p", "Bearer sk-wrong")
	if rec.Code != 401 {
		t.Errorf("expected 401 for invalid key, got %d", rec.Code)
	}
}

func TestAuth_InvalidFormat(t *testing.T) {
	engine := gin.New()
	engine.Use(Auth(newAuthService()))
	engine.GET("/p", func(c *gin.Context) { c.String(200, "ok") })

	// Authorization 不是 Bearer 格式
	rec := doRequest(engine, "GET", "/p", "Basic abcdef")
	if rec.Code != 401 {
		t.Errorf("expected 401 for bad auth format, got %d", rec.Code)
	}
}

func TestAuth_PublicExactPath(t *testing.T) {
	engine := gin.New()
	engine.Use(Auth(newAuthService(), "/health"))
	engine.GET("/health", func(c *gin.Context) { c.String(200, "ok") })

	rec := doRequest(engine, "GET", "/health", "")
	if rec.Code != 200 {
		t.Errorf("expected 200 for public exact path, got %d", rec.Code)
	}
}

func TestAuth_PublicPrefixPath(t *testing.T) {
	engine := gin.New()
	engine.Use(Auth(newAuthService(), "/v1/usage/*"))
	engine.GET("/v1/usage/abc", func(c *gin.Context) { c.String(200, "ok") })

	rec := doRequest(engine, "GET", "/v1/usage/abc", "")
	if rec.Code != 200 {
		t.Errorf("expected 200 for public prefix path, got %d", rec.Code)
	}
}

// ==================== AdminAuth ====================

func newAdminConfig() *config.Config {
	return &config.Config{
		Admin: config.AdminConfig{
			Password:   "secret",
			JWTSecret:  "mysecret",
			TokenExpiry: time.Hour,
		},
	}
}

func TestAdminAuth_NoHeader(t *testing.T) {
	engine := gin.New()
	engine.Use(AdminAuth(newAdminConfig()))
	engine.GET("/admin", func(c *gin.Context) { c.String(200, "ok") })

	rec := doRequest(engine, "GET", "/admin", "")
	if rec.Code != 401 {
		t.Errorf("expected 401 with no header, got %d", rec.Code)
	}
}

func TestAdminAuth_OptionsPasses(t *testing.T) {
	engine := gin.New()
	engine.Use(AdminAuth(newAdminConfig()))
	engine.GET("/admin", func(c *gin.Context) { c.String(200, "ok") })

	// OPTIONS 请求应被中间件放行（不返回 401），后续交由路由处理
	rec := doRequest(engine, "OPTIONS", "/admin", "")
	if rec.Code == 401 {
		t.Errorf("expected OPTIONS not to be blocked by admin auth, got %d", rec.Code)
	}
}

func TestAdminAuth_ValidJWT(t *testing.T) {
	engine := gin.New()
	engine.Use(AdminAuth(newAdminConfig()))
	engine.GET("/admin", func(c *gin.Context) { c.String(200, "ok") })

	token, _, err := GenerateAdminToken("mysecret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	rec := doRequest(engine, "GET", "/admin", "Bearer "+token)
	if rec.Code != 200 {
		t.Errorf("expected 200 for valid JWT, got %d", rec.Code)
	}
}

func TestAdminAuth_InvalidJWT(t *testing.T) {
	engine := gin.New()
	engine.Use(AdminAuth(newAdminConfig()))
	engine.GET("/admin", func(c *gin.Context) { c.String(200, "ok") })

	token, _, _ := GenerateAdminToken("wrongsecret", time.Hour)
	rec := doRequest(engine, "GET", "/admin", "Bearer "+token)
	if rec.Code != 401 {
		t.Errorf("expected 401 for invalid JWT, got %d", rec.Code)
	}
}

func TestAdminAuth_BasicAuth(t *testing.T) {
	engine := gin.New()
	engine.Use(AdminAuth(newAdminConfig()))
	engine.GET("/admin", func(c *gin.Context) { c.String(200, "ok") })

	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:secret"))
	rec := doRequest(engine, "GET", "/admin", basic)
	if rec.Code != 200 {
		t.Errorf("expected 200 for valid basic auth, got %d", rec.Code)
	}
}

func TestAdminAuth_BasicAuthInvalid(t *testing.T) {
	engine := gin.New()
	engine.Use(AdminAuth(newAdminConfig()))
	engine.GET("/admin", func(c *gin.Context) { c.String(200, "ok") })

	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:wrong"))
	rec := doRequest(engine, "GET", "/admin", basic)
	if rec.Code != 401 {
		t.Errorf("expected 401 for invalid basic auth, got %d", rec.Code)
	}
}

func TestGenerateAdminToken(t *testing.T) {
	token, exp, err := GenerateAdminToken("s", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if token == "" {
		t.Error("expected non-empty token")
	}
	if exp <= time.Now().Unix() {
		t.Error("expected expiry in the future")
	}
}

// ==================== RateLimit ====================

func TestRateLimit_Disabled(t *testing.T) {
	engine := gin.New()
	engine.Use(RateLimit(config.RateLimitConfig{Enabled: false}))
	engine.GET("/x", func(c *gin.Context) { c.String(200, "ok") })

	rec := doRequest(engine, "GET", "/x", "")
	if rec.Code != 200 {
		t.Errorf("expected 200 when disabled, got %d", rec.Code)
	}
}

func TestRateLimit_EnabledBurst(t *testing.T) {
	engine := gin.New()
	// rps=0（不补充令牌），burst=1 → 第一条通过，第二条被限流
	engine.Use(RateLimit(config.RateLimitConfig{Enabled: true, RequestsPerSecond: 0, Burst: 1}))
	engine.GET("/x", func(c *gin.Context) { c.String(200, "ok") })

	rec1 := doRequest(engine, "GET", "/x", "")
	if rec1.Code != 200 {
		t.Errorf("expected 200 for first request, got %d", rec1.Code)
	}
	rec2 := doRequest(engine, "GET", "/x", "")
	if rec2.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 for second request, got %d", rec2.Code)
	}
}

// ==================== Recovery ====================

func TestRecovery(t *testing.T) {
	engine := gin.New()
	engine.Use(Recovery())
	engine.GET("/panic", func(c *gin.Context) { panic("boom") })

	rec := doRequest(engine, "GET", "/panic", "")
	if rec.Code != 500 {
		t.Errorf("expected 500 after panic, got %d", rec.Code)
	}
}

// ==================== Logger ====================

func TestLogger(t *testing.T) {
	engine := gin.New()
	engine.Use(Logger())
	engine.GET("/x", func(c *gin.Context) { c.String(200, "ok") })

	rec := doRequest(engine, "GET", "/x", "")
	if rec.Code != 200 {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}
