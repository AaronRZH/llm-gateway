package auth

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"
)

const (
	// Redis key 前缀
	apikeyPrefix = "gateway:apikeys:"
	// 本地缓存 TTL
	localCacheTTL = 30 * time.Second
)

// KeyInfo API Key 关联的元信息
type KeyInfo struct {
	Key       string    `json:"key"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type cacheEntry struct {
	info      *KeyInfo
	expiresAt time.Time
}

// Service API Key 验证服务
type Service struct {
	rdb      *redis.Client
	seedKeys map[string]*KeyInfo      // 从配置加载的初始 Key
	cache    map[string]*cacheEntry   // 本地 LRU 缓存
	mu       sync.RWMutex
}

// New 创建 API Key 验证服务
// seedKeys: 从配置文件预加载的种子 Key，用于本地快速验证
func New(rdb *redis.Client, seedKeys map[string]*KeyInfo) *Service {
	if seedKeys == nil {
		seedKeys = make(map[string]*KeyInfo)
	}
	svc := &Service{
		rdb:      rdb,
		seedKeys: seedKeys,
		cache:    make(map[string]*cacheEntry),
	}
	// 将种子 Key 写入 Redis（幂等）
	if rdb != nil && len(seedKeys) > 0 {
		go svc.syncSeedKeysToRedis()
	}
	return svc
}

// FindKeyByName 根据 Key 名称查找 API Key（遍历种子 Key）
func (s *Service) FindKeyByName(name string) (*KeyInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, info := range s.seedKeys {
		if info.Name == name {
			return info, true
		}
	}
	return nil, false
}

// ListSeedKeys 返回所有种子 Key（管理后台用）
func (s *Service) ListSeedKeys() []*KeyInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]*KeyInfo, 0, len(s.seedKeys))
	for _, info := range s.seedKeys {
		// 拷贝一份避免外部修改
		infoCopy := *info
		keys = append(keys, &infoCopy)
	}
	return keys
}

// CreateSeedKey 新增一个种子 Key
func (s *Service) CreateSeedKey(key string, name string) bool {
	if key == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.seedKeys[key]; ok {
		return false // 已存在
	}
	info := &KeyInfo{
		Key:       key,
		Name:      name,
		CreatedAt: time.Now(),
	}
	s.seedKeys[key] = info
	// 同步到 Redis
	if s.rdb != nil {
		ctx := context.Background()
		pipe := s.rdb.Pipeline()
		pipe.HSet(ctx, apikeyPrefix+key, "name", name)
		pipe.HSet(ctx, apikeyPrefix+key, "created_at", info.CreatedAt.Format(time.RFC3339))
		pipe.Expire(ctx, apikeyPrefix+key, 0)
		if _, err := pipe.Exec(ctx); err != nil {
			log.Warn().Err(err).Str("key_prefix", key[:min(8, len(key))]+"...").Msg("create key sync to redis failed")
		}
	}
	return true
}

// DeleteSeedKey 删除一个种子 Key
func (s *Service) DeleteSeedKey(key string) bool {
	if key == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.seedKeys[key]; !ok {
		return false // 不存在
	}
	delete(s.seedKeys, key)
	// 从本地缓存中清除
	for k := range s.cache {
		if strings.HasPrefix(k, key) {
			delete(s.cache, k)
		}
	}
	// 从 Redis 中删除
	if s.rdb != nil {
		ctx := context.Background()
		_ = s.rdb.Del(ctx, apikeyPrefix+key)
	}
	return true
}

// Validate 验证 API Key 是否有效
// 返回 KeyInfo 和是否有效
func (s *Service) Validate(key string) (*KeyInfo, bool) {
	if key == "" {
		return nil, false
	}

	// 1. 查本地缓存
	if info := s.checkCache(key); info != nil {
		return info, true
	}

	// 2. 查种子 Key（配置中预加载的）
	if info, ok := s.seedKeys[key]; ok {
		s.setCache(key, info)
		return info, true
	}

	// 3. 查 Redis
	if s.rdb != nil {
		info, ok := s.checkRedis(key)
		if ok {
			s.setCache(key, info)
			return info, true
		}
	}

	return nil, false
}

// checkCache 检查本地缓存
func (s *Service) checkCache(key string) *KeyInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.cache[key]
	if !ok {
		return nil
	}
	if time.Now().After(entry.expiresAt) {
		return nil
	}
	return entry.info
}

// setCache 写入本地缓存
func (s *Service) setCache(key string, info *KeyInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 简单清理过期条目（避免 map 无限增长）
	if len(s.cache) > 10000 {
		now := time.Now()
		for k, v := range s.cache {
			if now.After(v.expiresAt) {
				delete(s.cache, k)
			}
		}
	}

	s.cache[key] = &cacheEntry{
		info:      info,
		expiresAt: time.Now().Add(localCacheTTL),
	}
}

// checkRedis 从 Redis 查询 API Key
func (s *Service) checkRedis(key string) (*KeyInfo, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	exists, err := s.rdb.Exists(ctx, apikeyPrefix+key).Result()
	if err != nil {
		log.Warn().Err(err).Str("key_prefix", key[:min(8, len(key))]+"...").Msg("redis check api key failed")
		return nil, false
	}
	if exists == 0 {
		return nil, false
	}

	// 可选：获取 Key 元信息
	name, _ := s.rdb.HGet(ctx, apikeyPrefix+key, "name").Result()
	createdAtStr, _ := s.rdb.HGet(ctx, apikeyPrefix+key, "created_at").Result()

	createdAt := time.Now()
	if t, err := time.Parse(time.RFC3339, createdAtStr); err == nil {
		createdAt = t
	}

	return &KeyInfo{
		Key:       key,
		Name:      name,
		CreatedAt: createdAt,
	}, true
}

// syncSeedKeysToRedis 将种子 Key 同步到 Redis
func (s *Service) syncSeedKeysToRedis() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for k, v := range s.seedKeys {
		pipe := s.rdb.Pipeline()
		pipe.HSet(ctx, apikeyPrefix+k, "name", v.Name)
		pipe.HSet(ctx, apikeyPrefix+k, "created_at", v.CreatedAt.Format(time.RFC3339))
		pipe.Expire(ctx, apikeyPrefix+k, 0) // 永不过期
		if _, err := pipe.Exec(ctx); err != nil {
			log.Warn().Err(err).Str("key_prefix", k[:min(8, len(k))]+"...").Msg("sync seed key to redis failed")
		}
	}

	log.Info().Int("count", len(s.seedKeys)).Msg("seed api keys synced to redis")
}
