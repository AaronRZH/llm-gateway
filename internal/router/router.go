package router

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/sony/gobreaker"

	"llm-gateway/internal/config"
	"llm-gateway/internal/provider"
	"llm-gateway/internal/token"
	"llm-gateway/pkg/breaker"
)

// Service 路由服务
type Service struct {
	mu              sync.RWMutex
	realModelsCfg   config.RealModelsConfig
	providerManager *provider.Manager
	tokenService    *token.Service
	breakers        map[string]*gobreaker.CircuitBreaker
	modelTiers      map[string]string // virtual model name -> tier

	// round_robin 策略状态
	roundRobinCounter int

	// latency_optimized 策略状态
	latencyTracker   map[string]float64   // "provider:model" -> 指数移动平均延迟(ms)
	latencyEMAAlpha  float64              // EMA 平滑系数 (0.1)
	lastRequestTimes map[string]time.Time // 最后请求时间，用于间隔计算
}

// Target 路由目标
type Target struct {
	Provider     provider.Provider
	ProviderName string
	Model        string
	Timeout      time.Duration
	Retry        int
	Breaker      *gobreaker.CircuitBreaker
}

// Selection 路由候选列表，handler 通过 Next() 遍历以在请求失败时重试下一个候选
type Selection struct {
	targets  []*Target
	position int
}

// Next 返回下一个候选 Target，如果没有更多候选则返回 nil
func (sel *Selection) Next() *Target {
	if sel.position >= len(sel.targets) {
		return nil
	}
	t := sel.targets[sel.position]
	sel.position++
	return t
}

// New 创建路由服务
func New(
	realModelsCfg config.RealModelsConfig,
	pm *provider.Manager,
	ts *token.Service,
	cbCfg config.CircuitBreakerConfig,
	modelTiers map[string]string,
) *Service {
	s := &Service{
		realModelsCfg:    realModelsCfg,
		providerManager:  pm,
		tokenService:     ts,
		breakers:         make(map[string]*gobreaker.CircuitBreaker),
		modelTiers:       modelTiers,
		latencyTracker:   make(map[string]float64),
		latencyEMAAlpha:  0.1,
		lastRequestTimes: make(map[string]time.Time),
	}

	// 初始化熔断器
	for _, item := range realModelsCfg.Models {
		key := s.breakerKey(item.Provider, item.Model)
		s.breakers[key] = breaker.New(key, breaker.Settings{
			MaxRequests:      uint32(cbCfg.MaxRequests),
			Interval:         cbCfg.Interval,
			Timeout:          cbCfg.Timeout,
			FailureThreshold: cbCfg.FailureThreshold,
			Cooldown:         cbCfg.Cooldown,
		})
	}

	// 启动延迟追踪后台任务
	go s.latencyWorker()

	return s
}

// latencyWorker 后台定时更新延迟统计
func (s *Service) latencyWorker() {
	ticker := time.NewTicker(30 * time.Second)
	for range ticker.C {
		s.mu.Lock()
		now := time.Now()
		for key, lastReq := range s.lastRequestTimes {
			if now.Sub(lastReq) > 30*time.Second {
				// 超过 30s 无请求，清理避免内存泄漏
				delete(s.lastRequestTimes, key)
				delete(s.latencyTracker, key)
			} else {
				s.latencyTracker[key] = 0 // 无新延迟数据时重置
			}
		}
		s.mu.Unlock()
	}
}

// Select 选择目标模型（保持 API 兼容，返回第一个可用候选）
func (s *Service) Select(ctx context.Context, virtualModel string, estimatedTokens int) (*Target, error) {
	sel, err := s.SelectCandidates(ctx, virtualModel, estimatedTokens)
	if err != nil {
		return nil, err
	}
	return sel.Next(), nil
}

// SelectCandidates 返回路由候选列表，handler 通过 Next() 遍历以在请求失败时重试下一个候选
func (s *Service) SelectCandidates(ctx context.Context, virtualModel string, estimatedTokens int) (*Selection, error) {
	chain := s.getOrderedChain(s.realModelsCfg.Strategy, s.realModelsCfg.Models)

	// 按 virtualModel 的 tier 过滤 fallback chain
	targetTier := s.resolveModelTier(virtualModel)
	if targetTier != "" {
		chain = s.filterByTier(chain, targetTier)
	}

	var candidates []*Target
	for _, item := range chain {
		target, err := s.tryProvider(ctx, item)
		if err != nil {
			log.Debug().
				Str("provider", item.Provider).
				Str("model", item.Model).
				Str("reason", err.Error()).
				Msg("model unavailable, skipping")
			continue
		}
		candidates = append(candidates, target)
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no available model")
	}
	return &Selection{targets: candidates}, nil
}

// resolveModelTier 获取虚拟模型对应的 tier
func (s *Service) resolveModelTier(virtualModel string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.modelTiers[virtualModel]
}

// filterByTier 按 tier 过滤 fallback chain
// 保留：tier 匹配的 item，以及 tier 为空的通用 fallback item
func (s *Service) filterByTier(chain []config.FallbackItem, targetTier string) []config.FallbackItem {
	var filtered []config.FallbackItem
	for _, item := range chain {
		if item.Tier == "" || item.Tier == targetTier {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

// getOrderedChain 根据策略返回排序后的 fallback chain
func (s *Service) getOrderedChain(strategy string, chain []config.FallbackItem) []config.FallbackItem {
	if strategy == "" {
		strategy = "priority"
	}

	switch strategy {
	case "round_robin":
		return s.roundRobinOrder(chain)
	case "latency_optimized":
		return s.sortedByLatency(chain)
	case "cost_optimized":
		return s.sortedByCost(chain)
	default:
		return chain
	}
}

// roundRobinOrder 返回加权轮询排序后的 chain
func (s *Service) roundRobinOrder(chain []config.FallbackItem) []config.FallbackItem {
	var eligible []int
	for i, item := range chain {
		if item.Weight > 0 {
			eligible = append(eligible, i)
		}
	}
	if len(eligible) == 0 {
		return chain
	}

	s.mu.Lock()
	idx := s.roundRobinCounter % len(eligible)
	s.roundRobinCounter++
	s.mu.Unlock()

	// 从选中位置开始，重排 chain
	ordered := make([]config.FallbackItem, 0, len(chain))
	// 先添加权重>0的，从 idx 开始
	for i := 0; i < len(eligible); i++ {
		pos := eligible[(idx+i)%len(eligible)]
		ordered = append(ordered, chain[pos])
	}
	// 再追加权重=0的作为最后的 fallback
	for i, item := range chain {
		if item.Weight <= 0 {
			ordered = append(ordered, chain[i])
		}
	}
	return ordered
}

// sortedByLatency 按延迟从低到高排序，延迟相同时按权重从高到低
func (s *Service) sortedByLatency(chain []config.FallbackItem) []config.FallbackItem {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// 深拷贝避免修改原始配置
	sorted := make([]config.FallbackItem, len(chain))
	copy(sorted, chain)

	sort.SliceStable(sorted, func(i, j int) bool {
		keyI := s.breakerKey(sorted[i].Provider, sorted[i].Model)
		keyJ := s.breakerKey(sorted[j].Provider, sorted[j].Model)

		latencyI := s.latencyTracker[keyI]
		latencyJ := s.latencyTracker[keyJ]

		if latencyI == 0 && latencyJ == 0 {
			// 都没有延迟数据，按权重高的在前
			return sorted[i].Weight > sorted[j].Weight
		}
		if latencyI == 0 {
			return false // 无数据的排在后面
		}
		if latencyJ == 0 {
			return true // 有数据的排前面
		}
		return latencyI < latencyJ
	})

	return sorted
}

// sortedByCost 按成本从低到高排序，成本相同时按权重从高到低
func (s *Service) sortedByCost(chain []config.FallbackItem) []config.FallbackItem {
	sorted := make([]config.FallbackItem, len(chain))
	copy(sorted, chain)

	sort.SliceStable(sorted, func(i, j int) bool {
		costI := sorted[i].Cost
		costJ := sorted[j].Cost

		if costI == 0 && costJ == 0 {
			return sorted[i].Weight > sorted[j].Weight
		}
		if costI == 0 {
			return false
		}
		if costJ == 0 {
			return true
		}
		return costI < costJ
	})

	return sorted
}

// tryProvider 检查单个 provider 的断路器状态，返回可用的 Target
func (s *Service) tryProvider(ctx context.Context, item config.FallbackItem) (*Target, error) {
	key := s.breakerKey(item.Provider, item.Model)
	cb := s.breakers[key]
	if cb == nil {
		return nil, fmt.Errorf("circuit breaker not found")
	}

	// 只检查断路器状态，不发送健康检查请求。
	// 断路器为 open 时跳过此 provider，closed/half-open 都放行。
	// 实际请求由 handler 通过 target.Breaker.Execute() 包裹，由 gobreaker 自动计数和状态转换。
	if cb.State() == gobreaker.StateOpen {
		return nil, fmt.Errorf("circuit breaker open")
	}

	// 获取 Provider
	p, ok := s.providerManager.Get(item.Provider)
	if !ok {
		return nil, fmt.Errorf("provider not found: %s", item.Provider)
	}

	target := &Target{
		Provider:     p,
		ProviderName: item.Provider,
		Model:        item.Model,
		Timeout:      item.Timeout,
		Retry:        item.Retry,
		Breaker:      cb,
	}

	// 记录请求开始时间（在 handler 返回后计算延迟）
	latencyKey := s.breakerKey(item.Provider, item.Model)
	s.mu.Lock()
	s.lastRequestTimes[latencyKey] = time.Now()
	s.mu.Unlock()

	return target, nil
}

// recordLatency 记录请求延迟到 tracker
func (s *Service) recordLatency(providerName, model string, latencyMs float64) {
	key := s.breakerKey(providerName, model)
	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.latencyTracker[key]
	if current == 0 {
		// 首次记录
		s.latencyTracker[key] = latencyMs
	} else {
		// EMA 平滑
		alpha := s.latencyEMAAlpha
		s.latencyTracker[key] = alpha*latencyMs + (1-alpha)*current
	}
}

// RecordLatency 供 handler 调用，记录请求延迟
func (s *Service) RecordLatency(providerName, model string, latencyMs float64) {
	s.recordLatency(providerName, model, latencyMs)
}

// SyncModelTiers 同步虚拟模型 → tier 映射（models 后台变更后调用）
func (s *Service) SyncModelTiers(tiers map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.modelTiers = tiers
}

// SetStrategy 动态更新路由策略
func (s *Service) SetStrategy(strategy string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.realModelsCfg.Strategy = strategy
	log.Info().Str("strategy", strategy).Msg("routing strategy updated")
}

// AddRealModel 动态添加一条 real_model 路由配置
func (s *Service) AddRealModel(item config.FallbackItem) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.realModelsCfg.Models = append(s.realModelsCfg.Models, item)
	// 初始化熔断器
	key := s.breakerKey(item.Provider, item.Model)
	if _, exists := s.breakers[key]; !exists {
		s.breakers[key] = breaker.New(key, breaker.Settings{
			MaxRequests:      5,
			Interval:         60 * time.Second,
			Timeout:          30 * time.Second,
			FailureThreshold: 5,
			Cooldown:         5 * time.Second,
		})
	}
	log.Info().Str("provider", item.Provider).Str("model", item.Model).Msg("real model added to router")
}

// UpdateRealModel 动态更新指定索引的 real_model 路由配置
func (s *Service) UpdateRealModel(index int, item config.FallbackItem) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if index < 0 || index >= len(s.realModelsCfg.Models) {
		log.Warn().Int("index", index).Msg("UpdateRealModel index out of range")
		return
	}
	s.realModelsCfg.Models[index] = item
	// 熔断器 key 可能变了，移除旧 key 的熔断器并创建新的
	// 但最简单的方式是确保新 key 有熔断器
	key := s.breakerKey(item.Provider, item.Model)
	if _, exists := s.breakers[key]; !exists {
		s.breakers[key] = breaker.New(key, breaker.Settings{
			MaxRequests:      5,
			Interval:         60 * time.Second,
			Timeout:          30 * time.Second,
			FailureThreshold: 5,
			Cooldown:         5 * time.Second,
		})
	}
	log.Info().Int("index", index).Str("provider", item.Provider).Str("model", item.Model).Msg("real model updated in router")
}

// DeleteRealModel 动态删除指定索引的 real_model 路由配置
func (s *Service) DeleteRealModel(index int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if index < 0 || index >= len(s.realModelsCfg.Models) {
		log.Warn().Int("index", index).Msg("DeleteRealModel index out of range")
		return
	}
	removed := s.realModelsCfg.Models[index]
	s.realModelsCfg.Models = append(s.realModelsCfg.Models[:index], s.realModelsCfg.Models[index+1:]...)
	// 注意：不删除熔断器，避免正在进行的请求受影响；熔断器会自然过期或被覆盖
	log.Info().Int("index", index).Str("provider", removed.Provider).Str("model", removed.Model).Msg("real model deleted from router")
}

// GetStrategy 获取当前路由策略
func (s *Service) GetStrategy() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.realModelsCfg.Strategy
}

// BreakerStates 返回所有熔断器的当前状态（用于管理端展示）
func (s *Service) BreakerStates() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	states := make(map[string]string, len(s.breakers))
	for key, cb := range s.breakers {
		states[key] = stateString(cb.State())
	}
	return states
}

func stateString(s gobreaker.State) string {
	switch s {
	case gobreaker.StateClosed:
		return "closed"
	case gobreaker.StateOpen:
		return "open"
	case gobreaker.StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

func (s *Service) breakerKey(provider, model string) string {
	return fmt.Sprintf("%s:%s", provider, model)
}
