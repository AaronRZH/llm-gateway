package ratelimit

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/time/rate"
)

// Limiter 限流器接口
type Limiter interface {
	Allow(ctx context.Context, key string) bool
}

// LocalLimiter 本地限流器（单机）
type LocalLimiter struct {
	limiters map[string]*rate.Limiter
	rate     rate.Limit
	burst    int
}

// NewLocalLimiter 创建本地限流器
func NewLocalLimiter(rps int, burst int) *LocalLimiter {
	return &LocalLimiter{
		limiters: make(map[string]*rate.Limiter),
		rate:     rate.Limit(rps),
		burst:    burst,
	}
}

// Allow 判断是否允许通过
func (l *LocalLimiter) Allow(ctx context.Context, key string) bool {
	limiter, ok := l.limiters[key]
	if !ok {
		limiter = rate.NewLimiter(l.rate, l.burst)
		l.limiters[key] = limiter
	}
	return limiter.Allow()
}

// RedisLimiter Redis 分布式限流器（滑动窗口）
type RedisLimiter struct {
	client *redis.Client
	window time.Duration
	limit  int
}

// NewRedisLimiter 创建 Redis 限流器
func NewRedisLimiter(client *redis.Client, window time.Duration, limit int) *RedisLimiter {
	return &RedisLimiter{
		client: client,
		window: window,
		limit:  limit,
	}
}

// Allow 使用 Redis 滑动窗口限流
func (l *RedisLimiter) Allow(ctx context.Context, key string) bool {
	now := time.Now().Unix()
	windowStart := now - int64(l.window.Seconds())

	pipe := l.client.Pipeline()

	// 移除窗口外的记录
	pipe.ZRemRangeByScore(ctx, key, "0", strconv.FormatInt(windowStart, 10))

	// 获取当前窗口内的请求数
	countCmd := pipe.ZCard(ctx, key)

	// 添加当前请求
	pipe.ZAdd(ctx, key, redis.Z{Score: float64(now), Member: now})

	// 设置过期时间
	pipe.Expire(ctx, key, l.window)

	_, err := pipe.Exec(ctx)
	if err != nil {
		return false // Redis 出错时保守处理，拒绝请求
	}

	count := countCmd.Val()
	return count < int64(l.limit)
}
