package router

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/sony/gobreaker"
	"github.com/rs/zerolog/log"

	"llm-gateway/internal/config"
	"llm-gateway/internal/mapper"
	"llm-gateway/internal/provider"
	"llm-gateway/internal/token"
	"llm-gateway/pkg/breaker"
)

// Service 路由服务
type Service struct {
	mu              sync.RWMutex
	groups          map[string]config.ModelGroup
	providerManager *provider.Manager
	mapper          *mapper.Service
	tokenService    *token.Service
	breakers        map[string]*gobreaker.CircuitBreaker
	healthStatus    map[string]bool // provider/model -> healthy?

	// round_robin 策略状态
	roundRobinCounter map[string]int // group -> current index

	// latency_optimized 策略状态
	latencyTracker   map[string]float64     // "group:provider:model" -> 指数移动平均延迟(ms)
	latencyEMAAlpha  float64                // EMA 平滑系数 (0.1)
	lastRequestTimes map[string]time.Time   // 最后请求时间，用于间隔计算
	groupOrder       map[string][]int       // group -> 按策略排序后的 fallback_chain 索引列表
}

// Target 路由目标
type Target struct {
	Provider     provider.Provider
	ProviderName string
	Model        string
	Timeout      time.Duration
	Retry        int
}

// New 创建路由服务
func New(
	groups map[string]config.ModelGroup,
	pm *provider.Manager,
	mapSvc *mapper.Service,
	ts *token.Service,
) *Service {
	s := &Service{
		groups:          groups,
		providerManager: pm,
		mapper:          mapSvc,
		tokenService:    ts,
		breakers:        make(map[string]*gobreaker.CircuitBreaker),
		healthStatus:    make(map[string]bool),
		roundRobinCounter: make(map[string]int),
		latencyTracker:   make(map[string]float64),
		latencyEMAAlpha:  0.1,
		lastRequestTimes: make(map[string]time.Time),
		groupOrder:       make(map[string][]int),
	}

	// 初始化熔断器
	for groupName, group := range groups {
		for _, item := range group.FallbackChain {
			key := s.breakerKey(groupName, item.Provider, item.Model)
			s.breakers[key] = breaker.New(
				key,
				3,    // maxRequests
				5,    // failureThreshold
				30*time.Second, // cooldown
			)
		}
	}

	// 预计算每个 group 的策略排序
	for groupName, group := range groups {
		s.groupOrder[groupName] = s.orderIndices(group)
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
		for key := range s.lastRequestTimes {
			s.latencyTracker[key] = 0 // 超过 30s 没有新请求，重置
		}
		s.mu.Unlock()
	}
}

// Select 选择目标模型
func (s *Service) Select(ctx context.Context, virtualModel string, estimatedTokens int) (*Target, error) {
	group, ok := s.groups[virtualModel]
	if !ok {
		// 未配置模型组，尝试直接路由
		return s.directRoute(ctx, virtualModel)
	}

	return s.selectFromGroup(ctx, virtualModel, group, estimatedTokens)
}

// selectFromGroup 根据策略从模型组中选择目标
func (s *Service) selectFromGroup(ctx context.Context, groupName string, group config.ModelGroup, estimatedTokens int) (*Target, error) {
	strategy := group.Strategy
	if strategy == "" {
		strategy = "priority" // 默认策略
	}

	switch strategy {
	case "round_robin":
		return s.selectRoundRobin(ctx, groupName, group)
	case "latency_optimized":
		return s.selectLatencyOptimized(ctx, groupName, group)
	case "cost_optimized":
		return s.selectCostOptimized(ctx, groupName, group)
	default:
		return s.selectPriority(ctx, groupName, group)
	}
}

// selectPriority 按 fallback_chain 顺序，依次检查熔断器（默认策略）
func (s *Service) selectPriority(ctx context.Context, groupName string, group config.ModelGroup) (*Target, error) {
	return s.selectByFallbackChain(ctx, groupName, group, group.FallbackChain)
}

// selectByFallbackChain 通用 fallback 逻辑（按指定顺序）
func (s *Service) selectByFallbackChain(ctx context.Context, groupName string, group config.ModelGroup, orderedChain []config.FallbackItem) (*Target, error) {
	for _, item := range orderedChain {
		target, err := s.tryProvider(ctx, groupName, item)
		if err == nil {
			return target, nil
		}
		log.Debug().
			Str("provider", item.Provider).
			Str("model", item.Model).
			Str("reason", err.Error()).
			Msg("model unavailable, trying next")
	}
	return nil, fmt.Errorf("no available model in group")
}

// selectRoundRobin 加权轮询策略
func (s *Service) selectRoundRobin(ctx context.Context, groupName string, group config.ModelGroup) (*Target, error) {
	chain := group.FallbackChain

	// 收集权重 > 0 的 provider
	var eligible []int
	for i, item := range chain {
		if item.Weight > 0 {
			eligible = append(eligible, i)
		}
	}
	if len(eligible) == 0 {
		// 没有权重，退化为 priority
		return s.selectByFallbackChain(ctx, groupName, group, chain)
	}

	s.mu.Lock()
	idx := s.roundRobinCounter[groupName] % len(eligible)
	s.roundRobinCounter[groupName]++
	s.mu.Unlock()

	// 从选中位置开始，尝试最多 len(chain) 次（覆盖所有 provider）
	for i := 0; i < len(chain); i++ {
		pos := (idx + i) % len(chain)
		item := chain[pos]

		// 只有权重 > 0 的才参与轮询，否则跳过（但可以作为 fallback）
		if item.Weight <= 0 {
			// 尝试作为 fallback
			target, err := s.tryProvider(ctx, groupName, item)
			if err == nil {
				return target, nil
			}
			continue
		}

		target, err := s.tryProvider(ctx, groupName, item)
		if err == nil {
			return target, nil
		}
		log.Debug().
			Str("provider", item.Provider).
			Str("model", item.Model).
			Msg("round_robin: model unavailable, trying next")
	}

	return nil, fmt.Errorf("no available model in group (round_robin)")
}

// selectLatencyOptimized 延迟优先策略
func (s *Service) selectLatencyOptimized(ctx context.Context, groupName string, group config.ModelGroup) (*Target, error) {
	chain := group.FallbackChain

	// 按最近延迟排序（延迟低的在前），相同延迟按权重排序
	sorted := s.sortedByLatency(chain)

	return s.selectByFallbackChain(ctx, groupName, group, sorted)
}

// sortedByLatency 按延迟从低到高排序，延迟相同时按权重从高到低
func (s *Service) sortedByLatency(chain []config.FallbackItem) []config.FallbackItem {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// 深拷贝避免修改原始配置
	sorted := make([]config.FallbackItem, len(chain))
	copy(sorted, chain)

	sort.SliceStable(sorted, func(i, j int) bool {
		keyI := s.breakerKey("", sorted[i].Provider, sorted[i].Model)
		keyJ := s.breakerKey("", sorted[j].Provider, sorted[j].Model)

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

// selectCostOptimized 成本优先策略
func (s *Service) selectCostOptimized(ctx context.Context, groupName string, group config.ModelGroup) (*Target, error) {
	chain := group.FallbackChain

	// 按成本从低到高排序，成本相同的按权重排序
	sorted := make([]config.FallbackItem, len(chain))
	copy(sorted, chain)

	sort.SliceStable(sorted, func(i, j int) bool {
		costI := sorted[i].Cost
		costJ := sorted[j].Cost

		if costI == 0 && costJ == 0 {
			// 都没有成本数据，按权重
			return sorted[i].Weight > sorted[j].Weight
		}
		if costI == 0 {
			return false // 无成本数据的排后面
		}
		if costJ == 0 {
			return true // 有成本数据的排前面
		}
		return costI < costJ
	})

	return s.selectByFallbackChain(ctx, groupName, group, sorted)
}

// tryProvider 尝试单个 provider，包含熔断器检查和健康检查
func (s *Service) tryProvider(ctx context.Context, groupName string, item config.FallbackItem) (*Target, error) {
	key := s.breakerKey(groupName, item.Provider, item.Model)
	cb := s.breakers[key]
	if cb == nil {
		return nil, fmt.Errorf("circuit breaker not found")
	}

	// 检查熔断器状态（发送一个轻量级探测请求）
	_, err := cb.Execute(func() (interface{}, error) {
		return nil, s.healthCheck(ctx, item.Provider, item.Model)
	})

	if err != nil {
		return nil, err
	}

	// 获取 Provider
	p, ok := s.providerManager.Get(item.Provider)
	if !ok {
		return nil, fmt.Errorf("provider not found: %s", item.Provider)
	}

	// 记录本次请求的延迟到 latency tracker
	target := &Target{
		Provider:     p,
		ProviderName: item.Provider,
		Model:        item.Model,
		Timeout:      item.Timeout,
		Retry:        item.Retry,
	}

	// 记录请求开始时间（在返回后计算延迟）
	latencyKey := s.breakerKey(groupName, item.Provider, item.Model)
	s.mu.Lock()
	s.lastRequestTimes[latencyKey] = time.Now()
	s.mu.Unlock()

	return target, nil
}

// recordLatency 记录请求延迟到 tracker
func (s *Service) recordLatency(groupName, providerName, model string, latencyMs float64) {
	key := s.breakerKey(groupName, providerName, model)
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

// directRoute 直接路由（未配置模型组时，通过模型映射选择 Provider）
func (s *Service) directRoute(ctx context.Context, virtualModel string) (*Target, error) {
	mapped, err := s.mapper.Resolve(virtualModel)
	if err != nil {
		return nil, fmt.Errorf("direct route not implemented for: %s", virtualModel)
	}

	p, ok := s.providerManager.Get(mapped.Provider)
	if !ok {
		return nil, fmt.Errorf("provider not found: %s", mapped.Provider)
	}

	return &Target{
		Provider:     p,
		ProviderName: mapped.Provider,
		Model:        mapped.RealModel,
	}, nil
}

// healthCheck 健康检查
func (s *Service) healthCheck(ctx context.Context, providerName, model string) error {
	p, ok := s.providerManager.Get(providerName)
	if !ok {
		return fmt.Errorf("provider not found: %s", providerName)
	}
	// 发送一个轻量级探测请求
	return p.HealthCheck(ctx, model)
}

// RecordLatency 供 handler 调用，记录请求延迟
func (s *Service) RecordLatency(groupName, providerName, model string, latencyMs float64) {
	s.recordLatency(groupName, providerName, model, latencyMs)
}

func (s *Service) breakerKey(group, provider, model string) string {
	if group != "" {
		return fmt.Sprintf("%s:%s:%s", group, provider, model)
	}
	return fmt.Sprintf("%s:%s", provider, model)
}

func (s *Service) orderIndices(group config.ModelGroup) []int {
	strategy := group.Strategy
	switch strategy {
	case "round_robin":
		var eligible []int
		for i, item := range group.FallbackChain {
			if item.Weight > 0 {
				eligible = append(eligible, i)
			}
		}
		if len(eligible) == 0 {
			indices := make([]int, len(group.FallbackChain))
			for i := range indices {
				indices[i] = i
			}
			return indices
		}
		return eligible
	case "latency_optimized", "cost_optimized":
		// 返回所有索引，具体排序在运行时动态计算
		indices := make([]int, len(group.FallbackChain))
		for i := range indices {
			indices[i] = i
		}
		return indices
	default:
		indices := make([]int, len(group.FallbackChain))
		for i := range indices {
			indices[i] = i
		}
		return indices
	}
}
