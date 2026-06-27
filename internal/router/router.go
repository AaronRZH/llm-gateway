package router

import (
	"context"
	"fmt"
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

	return s
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

// selectFromGroup 从模型组中选择
func (s *Service) selectFromGroup(ctx context.Context, groupName string, group config.ModelGroup, estimatedTokens int) (*Target, error) {
	for _, item := range group.FallbackChain {
		key := s.breakerKey(groupName, item.Provider, item.Model)
		cb := s.breakers[key]
		if cb == nil {
			continue
		}

		// 检查熔断器状态
		_, err := cb.Execute(func() (interface{}, error) {
			return nil, s.healthCheck(ctx, item.Provider, item.Model)
		})

		if err == nil {
			p, ok := s.providerManager.Get(item.Provider)
			if !ok {
				continue
			}
			return &Target{
				Provider:     p,
				ProviderName: item.Provider,
				Model:        item.Model,
				Timeout:      item.Timeout,
				Retry:        item.Retry,
			}, nil
		}

		log.Warn().
			Str("provider", item.Provider).
			Str("model", item.Model).
			Str("reason", err.Error()).
			Msg("model unavailable, trying next")
	}

	return nil, fmt.Errorf("no available model in group")
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

func (s *Service) breakerKey(group, provider, model string) string {
	if group != "" {
		return fmt.Sprintf("%s:%s:%s", group, provider, model)
	}
	return fmt.Sprintf("%s:%s", provider, model)
}
