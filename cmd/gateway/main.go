package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"llm-gateway/internal/config"
	"llm-gateway/internal/health"
	"llm-gateway/internal/mapper"
	"llm-gateway/internal/metrics"
	"llm-gateway/internal/middleware"
	"llm-gateway/internal/provider"
	"llm-gateway/internal/router"
	"llm-gateway/internal/stream"
	"llm-gateway/internal/token"
)

func main() {
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

	// 初始化各模块
	mapperService := mapper.New(cfg.ModelMapping)
	tokenService := token.New(cfg.Token)
	providerManager := provider.NewManager(cfg.Providers)
	routerService := router.New(cfg.ModelGroups, providerManager, mapperService, tokenService)
	streamHandler := stream.New(mapperService)

	// 创建 Gin 引擎
	if cfg.App.Env == "prod" {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.New()

	// 全局中间件
	r.Use(middleware.Logger())
	r.Use(middleware.Recovery())
	r.Use(middleware.CORS())
	r.Use(middleware.RateLimit(cfg.RateLimit))

	// 健康检查
	r.GET(cfg.Health.Path, health.Handler())

	// Prometheus 指标
	if cfg.Metrics.Enabled {
		r.GET(cfg.Metrics.Path, metrics.Handler())
	}

	// API 路由
	api := r.Group("/v1")
	{
		api.POST("/chat/completions", handleChatCompletion(mapperService, routerService, streamHandler, tokenService))
		api.POST("/completions", handleCompletion(mapperService, routerService, streamHandler, tokenService))
		api.GET("/models", handleListModels(mapperService))
	}

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
