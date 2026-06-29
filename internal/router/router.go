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
	breakers map[string]*gobreaker.CircuitBreaker

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
	Breaker      *gobreaker.CircuitBreaker // 断路器，用于保护实际请求；directRoute 时为 nil
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
	groups map[string]config.ModelGroup,
	pm *provider.Manager,
	mapSvc *mapper.Service,
	ts *token.Service,
	cbCfg config.CircuitBreakerConfig,
) *Service {
	s := &Service{
		groups:            groups,
		providerManager:   pm,
		mapper:            mapSvc,
		tokenService:      ts,
		breakers:          make(map[string]*gobreaker.CircuitBreaker),
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
			s.breakers[key] = breaker.New(key, breaker.Settings{
				MaxRequests:      uint32(cbCfg.MaxRequests),
				Interval:         cbCfg.Interval,
				Timeout:          cbCfg.Timeout,
				FailureThreshold: cbCfg.FailureThreshold,
				Cooldown:         cbCfg.Cooldown,
			})
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
	group, ok := s.groups[virtualModel]
	if !ok {
		// 未配置模型组，直接路由（单个候选，无断路器、无 fallback）
		target, err := s.directRoute(ctx, virtualModel)
		if err != nil {
			return nil, err
		}
		return &Selection{targets: []*Target{target}}, nil
	}

	chain := s.getOrderedChain(virtualModel, group)
	var candidates []*Target
	for _, item := range chain {
		target, err := s.tryProvider(ctx, virtualModel, item)
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
		return nil, fmt.Errorf("no available model in group")
	}
	return &Selection{targets: candidates}, nil
}

// getOrderedChain 根据策略返回排序后的 fallback chain
func (s *Service) getOrderedChain(groupName string, group config.ModelGroup) []config.FallbackItem {
	strategy := group.Strategy
	if strategy == "" {
		strategy = "priority"
	}

	switch strategy {
	case "round_robin":
		return s.roundRobinOrder(groupName, group)
	case "latency_optimized":
		return s.sortedByLatency(group.FallbackChain)
	case "cost_optimized":
		return s.sortedByCost(group.FallbackChain)
	default:
		return group.FallbackChain
	}
}

// roundRobinOrder 返回加权轮询排序后的 chain
func (s *Service) roundRobinOrder(groupName string, group config.ModelGroup) []config.FallbackItem {
	chain := group.FallbackChain

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
	idx := s.roundRobinCounter[groupName] % len(eligible)
	s.roundRobinCounter[groupName]++
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

// tryProvider 检查单个 provider 的断路器状态，返回可用的 Target（不做健康检查）
func (s *Service) tryProvider(ctx context.Context, groupName string, item config.FallbackItem) (*Target, error) {
	key := s.breakerKey(groupName, item.Provider, item.Model)
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
