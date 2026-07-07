package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/config"
	"llm-gateway/internal/health"
	"llm-gateway/internal/mapper"
	"llm-gateway/internal/middleware"
	"llm-gateway/internal/provider"
	redisutil "llm-gateway/pkg/redis"
	"llm-gateway/internal/router"
	"llm-gateway/internal/storage"
	"llm-gateway/internal/stream"
	"llm-gateway/internal/token"
)

func main() {
	// 加载 .env 文件（优先配置文件所在目录的父目录，再回退到当前工作目录）
	configDir := filepath.Dir("configs/config.yaml")
	envFile := filepath.Join(configDir, "..", ".env")
	if _, err := os.Stat(envFile); err == nil {
		if err := godotenv.Load(envFile); err != nil {
			log.Warn().Err(err).Msg("failed to load .env file")
		}
	} else if err := godotenv.Load(); err != nil {
		log.Warn().Err(err).Msg("failed to load .env file, using system environment variables")
	}

	// 加载配置
	cfg, err := config.Load("configs/config.yaml")
	if err != nil {
		log.Fatal().Err(err).Msg("failed to load config")
	}

	// 设置日志
	setupLogger(cfg)

	log.Info().
		Str("app", cfg.App.Name).
		Str("version", cfg.App.Version).
		Str("env", cfg.App.Env).
		Msg("starting llm-gateway")

	// 构建 modelTiers map（虚拟模型名 -> tier）
	modelTiers := make(map[string]string, len(cfg.Models))
	for _, entry := range cfg.Models {
		if entry.Tier != "" {
			modelTiers[entry.Name] = entry.Tier
		}
	}

	// 初始化各模块
	mapperService := mapper.New(cfg.Models)
	tokenService := token.New(cfg.Token)
	providerManager := provider.NewManager(cfg.Providers)
	routerService := router.New(cfg.RealModels, providerManager, tokenService, cfg.CircuitBreaker, modelTiers)
	streamHandler := stream.New()

	// 初始化 Redis 客户端
	redisClient := redisutil.New(redisutil.Config{
		Addr:         cfg.Redis.Addr,
		Password:     cfg.Redis.Password,
		DB:           cfg.Redis.DB,
		PoolSize:     cfg.Redis.PoolSize,
		DialTimeout:  cfg.Redis.DialTimeout,
		ReadTimeout:  cfg.Redis.ReadTimeout,
		WriteTimeout: cfg.Redis.WriteTimeout,
	})

	// 初始化认证服务（加载种子 Key）
	seedKeys := buildSeedKeys(cfg.APIKeys)
	authService := auth.New(redisClient, seedKeys)

	// 初始化存储层（PostgreSQL → 文件降级）
	usageStorage := storage.NewPostgresStorage(cfg.Postgres)
	tokenService.SetStorage(usageStorage)

	// 创建 Gin 引擎
	if cfg.App.Env == "prod" {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.New()

	// 全局中间件（Auth 支持白名单路径）
	r.Use(middleware.Logger())
	r.Use(middleware.Recovery())
	r.Use(middleware.CORS())
	r.Use(middleware.RateLimit(cfg.RateLimit))
	r.Use(middleware.Auth(authService, cfg.AuthWhitelist...))

	// 注册公开路由
	r.GET(cfg.Health.Path, health.Handler())

	// API 路由
	api := r.Group("/v1")
	{
		api.POST("/chat/completions", handleChatCompletion(mapperService, routerService, streamHandler, tokenService))
		api.POST("/completions", handleCompletion(mapperService, routerService, streamHandler, tokenService))
		api.POST("/messages", handleAnthropicMessages(mapperService, routerService, streamHandler, tokenService))
		api.POST("/messages/count_tokens", handleCountTokens(mapperService, routerService, providerManager))
		api.GET("/models", handleListModels(mapperService))
	}

	// Token 用量统计查询路由（已删除 /usage，改用 /admin/usage/*）

	// 管理员路由（基础数据查询，无需密码）
	r.GET("/admin/usage", handleAdminUsage(tokenService))
	r.GET("/admin/usage/daily", handleAdminDailyUsage(tokenService))
	r.GET("/admin/usage/stats", handleAdminStats(tokenService))
	r.GET("/admin/calibration", handleAdminCalibration(tokenService))
	r.GET("/admin/breakers", handleAdminBreakers(routerService))
	r.GET("/admin/usage/by-real-model", handleAdminUsageByRealModel(tokenService))
		r.GET("/admin/usage/by-api-key", handleAdminUsageByAPIKey(authService, tokenService))

	// 管理后台 API（扩展管理功能，需 AdminAuth 中间件）
	adminAPI := r.Group("/admin")
		// 管理后台登录（不需要 AdminAuth 中间件）
		r.POST("/admin/login", handleAdminLogin(cfg))
	adminAPI.Use(middleware.AdminAuth(cfg))
	{
		adminAPI.GET("/api-keys", handleAdminAPIKeys(authService))
		adminAPI.POST("/api-keys", handleAdminCreateAPIKey(authService, cfg))
		adminAPI.DELETE("/api-keys/:key", handleAdminDeleteAPIKey(authService, cfg))
		adminAPI.GET("/api-keys/:key/usage", handleAdminAPIKeyUsage(authService, tokenService))
		adminAPI.GET("/providers", handleAdminProviders(routerService))
		adminAPI.GET("/providers/config", handleAdminProvidersConfig(cfg))
		adminAPI.POST("/providers", handleAdminAddProvider(cfg, providerManager))
		adminAPI.PUT("/providers/:name", handleAdminUpdateProvider(cfg, providerManager))
		adminAPI.DELETE("/providers/:name", handleAdminDeleteProvider(cfg, providerManager))
		adminAPI.GET("/models", handleAdminModels(mapperService))
		adminAPI.POST("/models", handleAdminAddModel(mapperService, cfg))
		adminAPI.DELETE("/models/:name", handleAdminDeleteModel(mapperService, cfg))
		adminAPI.GET("/real-models", handleAdminRealModels(routerService, cfg))
		adminAPI.POST("/real-models", handleAdminAddRealModel(cfg, routerService))
		adminAPI.PUT("/real-models/:index", handleAdminUpdateRealModel(cfg, routerService))
		adminAPI.DELETE("/real-models/:index", handleAdminDeleteRealModel(cfg, routerService))
		adminAPI.PATCH("/real-models/strategy", handleAdminUpdateStrategy(routerService, cfg))
		adminAPI.GET("/config", handleAdminConfig(cfg.App, mapperService))
	}

	// 管理后台静态文件服务（SPA 模式）
	r.StaticFS("/admin/assets", http.Dir("web/static"))
	r.GET("/admin", func(c *gin.Context) {
		c.File("web/static/index.html")
	})

	// 启动 HTTP 服务
	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.App.Port),
		Handler: r,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal().Err(err).Msg("server failed to start")
		}
	}()

	log.Info().Int("port", cfg.App.Port).Msg("server started")

	// 优雅关闭
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info().Msg("shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Error().Err(err).Msg("server forced to shutdown")
	}

	// 关闭 Redis 连接
	if redisClient != nil {
		if err := redisClient.Close(); err != nil {
			log.Error().Err(err).Msg("redis close failed")
		}
	}

	// 关闭存储连接
	if usageStorage != nil {
		if err := usageStorage.Close(); err != nil {
			log.Error().Err(err).Msg("usage storage close failed")
		}
	}

	log.Info().Msg("server exited")
}

func setupLogger(cfg *config.Config) {
	level, err := zerolog.ParseLevel(cfg.Log.Level)
	if err != nil {
		level = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(level)

	if cfg.Log.Format == "console" {
		log.Logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr}).With().Timestamp().Logger()
	} else {
		log.Logger = zerolog.New(os.Stderr).With().Timestamp().Logger()
	}
}

func buildSeedKeys(apiKeys []config.APIKeyConfig) map[string]*auth.KeyInfo {
	seedKeys := make(map[string]*auth.KeyInfo, len(apiKeys))
	for _, k := range apiKeys {
		seedKeys[k.Key] = &auth.KeyInfo{
			Key:   k.Key,
			Name:  k.Name,
		}
	}
	return seedKeys
}