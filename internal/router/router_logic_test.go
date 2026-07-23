package router

import (
	"context"
	"testing"
	"time"

	"github.com/sony/gobreaker"
	"llm-gateway/internal/config"
	"llm-gateway/internal/provider"
)

func newTestService(t *testing.T, models []config.FallbackItem) *Service {
	t.Helper()
	pm := provider.NewManager(map[string]config.ProviderConfig{
		"p1": {BaseURL: "http://p1", Protocol: "openai"},
		"p2": {BaseURL: "http://p2", Protocol: "anthropic"},
		"p3": {BaseURL: "http://p3", Protocol: "openai"},
	})
	cbCfg := config.CircuitBreakerConfig{
		MaxRequests:      5,
		Interval:         60 * time.Second,
		Timeout:          30 * time.Second,
		FailureThreshold: 5,
		Cooldown:         5 * time.Second,
	}
	return New(config.RealModelsConfig{Strategy: "priority", Models: models}, pm, nil, cbCfg, map[string]string{})
}

func TestCloneFallbackChain(t *testing.T) {
	src := []config.FallbackItem{{Provider: "p1", Model: "m1"}, {Provider: "p2", Model: "m2"}}
	dst := cloneFallbackChain(src)
	if &dst == &src {
		t.Error("expected distinct slice headers")
	}
	if len(dst) != len(src) {
		t.Fatalf("expected same length, got %d vs %d", len(dst), len(src))
	}
	if dst[0].Provider != src[0].Provider {
		t.Error("content mismatch")
	}
	// 修改 dst 不影响 src
	dst[0].Provider = "changed"
	if src[0].Provider != "p1" {
		t.Error("clone should not share backing array mutation")
	}
}

func TestPriorityOrder(t *testing.T) {
	s := newTestService(t, nil)
	chain := []config.FallbackItem{
		{Provider: "a", Priority: 0},
		{Provider: "b", Priority: 5},
		{Provider: "c", Priority: 2},
		{Provider: "d", Priority: 0},
	}
	got := s.priorityOrder(chain)
	// 期望按 priority 升序：a(0), d(0), c(2), b(5)，且同优先级稳定
	want := []string{"a", "d", "c", "b"}
	for i, item := range got {
		if item.Provider != want[i] {
			t.Errorf("priorityOrder[%d] = %s, want %s", i, item.Provider, want[i])
		}
	}
}

func TestPriorityOrder_AllZeroStable(t *testing.T) {
	s := newTestService(t, nil)
	chain := []config.FallbackItem{
		{Provider: "a"}, {Provider: "b"}, {Provider: "c"},
	}
	got := s.priorityOrder(chain)
	for i, item := range got {
		if item.Provider != chain[i].Provider {
			t.Errorf("expected stable order, got %s at %d", item.Provider, i)
		}
	}
}

func TestRoundRobinOrder(t *testing.T) {
	s := newTestService(t, nil)
	chain := []config.FallbackItem{
		{Provider: "a", Weight: 1},
		{Provider: "b", Weight: 1},
		{Provider: "c", Weight: 0}, // 权重 0 → 最后 fallback
	}
	got1 := s.roundRobinOrder(chain)
	// 第一轮：从 idx 0 开始 → a, b, 然后 c (权重0)
	if got1[0].Provider != "a" || got1[1].Provider != "b" || got1[2].Provider != "c" {
		t.Errorf("unexpected round robin order 1: %v", providersOf(got1))
	}
	got2 := s.roundRobinOrder(chain)
	// 第二轮：idx 1 → b, a, c
	if got2[0].Provider != "b" || got2[1].Provider != "a" || got2[2].Provider != "c" {
		t.Errorf("unexpected round robin order 2: %v", providersOf(got2))
	}
}

func TestRoundRobinOrder_NoEligible(t *testing.T) {
	s := newTestService(t, nil)
	chain := []config.FallbackItem{{Provider: "a", Weight: 0}, {Provider: "b", Weight: 0}}
	got := s.roundRobinOrder(chain)
	// 没有 eligible → 原样返回
	if len(got) != 2 || got[0].Provider != "a" {
		t.Errorf("expected original chain, got %v", providersOf(got))
	}
}

func TestSortedByLatency_NoData(t *testing.T) {
	s := newTestService(t, nil)
	chain := []config.FallbackItem{
		{Provider: "a", Model: "m", Weight: 1},
		{Provider: "b", Model: "m", Weight: 3},
		{Provider: "c", Model: "m", Weight: 2},
	}
	got := s.sortedByLatency(chain)
	// 无延迟数据 → 按权重降序：b(3), c(2), a(1)
	want := []string{"b", "c", "a"}
	for i, item := range got {
		if item.Provider != want[i] {
			t.Errorf("sortedByLatency[%d] = %s, want %s", i, item.Provider, want[i])
		}
	}
}

func TestSortedByLatency_WithData(t *testing.T) {
	s := newTestService(t, nil)
	s.recordLatency("p1", "m1", 100)
	s.recordLatency("p2", "m2", 10)
	chain := []config.FallbackItem{
		{Provider: "p1", Model: "m1"},
		{Provider: "p2", Model: "m2"},
	}
	got := s.sortedByLatency(chain)
	// p2 (10ms) 应排在 p1 (100ms) 之前
	if got[0].Provider != "p2" || got[1].Provider != "p1" {
		t.Errorf("expected p2 before p1, got %v", providersOf(got))
	}
}

func TestSortedByCost(t *testing.T) {
	s := newTestService(t, nil)
	chain := []config.FallbackItem{
		{Provider: "a", Model: "m", Cost: 0},
		{Provider: "b", Model: "m", Cost: 3},
		{Provider: "c", Model: "m", Cost: 1},
	}
	got := s.sortedByCost(chain)
	// 成本为 0 视为无数据，按权重降序；b(3), c(1), a(0 weight 0)
	// 实际：c(1), b(3) 按成本升序；a cost0 且 weight0 排最后
	if got[0].Provider != "c" || got[1].Provider != "b" {
		t.Errorf("expected c, b first by cost, got %v", providersOf(got))
	}
	if got[len(got)-1].Provider != "a" {
		t.Errorf("expected a (cost 0) last, got %v", providersOf(got))
	}
}

func TestFilterDisabled(t *testing.T) {
	s := newTestService(t, nil)
	chain := []config.FallbackItem{
		{Provider: "a"},
		{Provider: "b", Disabled: true},
		{Provider: "c"},
	}
	got := s.filterDisabled(chain)
	if len(got) != 2 {
		t.Fatalf("expected 2 after filter, got %d", len(got))
	}
	for _, item := range got {
		if item.Disabled {
			t.Error("disabled item should be filtered out")
		}
	}
}

func TestFilterByTier(t *testing.T) {
	s := newTestService(t, nil)
	chain := []config.FallbackItem{
		{Provider: "a", Tier: "premium"},
		{Provider: "b", Tier: ""},       // 通用 fallback
		{Provider: "c", Tier: "standard"}, // 不匹配
	}
	got := s.filterByTier(chain, "premium")
	if len(got) != 2 {
		t.Fatalf("expected 2 (premium + empty), got %d", len(got))
	}
	for _, item := range got {
		if item.Tier != "" && item.Tier != "premium" {
			t.Errorf("unexpected tier %s", item.Tier)
		}
	}
}

func TestResolveModelTier(t *testing.T) {
	s := newTestService(t, nil)
	// 新建服务 tier 为空，但可通过 SyncModelTiers 注入
	s.SyncModelTiers(map[string]string{"vm1": "premium"})
	if tier := s.resolveModelTier("vm1"); tier != "premium" {
		t.Errorf("expected premium, got %q", tier)
	}
	if tier := s.resolveModelTier("missing"); tier != "" {
		t.Errorf("expected empty, got %q", tier)
	}
}

func TestGetOrderedChain(t *testing.T) {
	s := newTestService(t, nil)
	chain := []config.FallbackItem{{Provider: "a", Priority: 2}, {Provider: "b", Priority: 1}}
	// 空策略 → 默认 priority
	if got := s.getOrderedChain("", chain); got[0].Provider != "b" {
		t.Errorf("empty strategy should default to priority, got %s", got[0].Provider)
	}
	// 未知策略 → 原样返回
	if got := s.getOrderedChain("unknown", chain); got[0].Provider != "a" {
		t.Errorf("unknown strategy should return unchanged, got %s", got[0].Provider)
	}
}

func TestRecordLatency_EMA(t *testing.T) {
	s := newTestService(t, nil)
	// 首次记录
	s.RecordLatency("p1", "m1", 100)
	if v := s.latencyTracker[s.breakerKey("p1", "m1")]; v != 100 {
		t.Errorf("expected 100, got %v", v)
	}
	// EMA 平滑：alpha=0.1 → 0.1*50 + 0.9*100 = 95
	s.RecordLatency("p1", "m1", 50)
	if v := s.latencyTracker[s.breakerKey("p1", "m1")]; v != 95 {
		t.Errorf("expected EMA 95, got %v", v)
	}
}

func TestSetStrategyAndGet(t *testing.T) {
	s := newTestService(t, nil)
	s.SetStrategy("cost_optimized")
	if got := s.GetStrategy(); got != "cost_optimized" {
		t.Errorf("expected cost_optimized, got %q", got)
	}
}

func TestAddRealModel(t *testing.T) {
	s := newTestService(t, nil)
	s.AddRealModel(config.FallbackItem{Provider: "p1", Model: "m9"})
	if s.getBreaker(s.breakerKey("p1", "m9")) == nil {
		t.Error("expected breaker created for new real model")
	}
	// 重复添加应创建对应 breaker（幂等安全）
	s.AddRealModel(config.FallbackItem{Provider: "p1", Model: "m9"})
}

func TestUpdateRealModel(t *testing.T) {
	s := newTestService(t, []config.FallbackItem{{Provider: "p1", Model: "m1"}})
	s.UpdateRealModel(0, config.FallbackItem{Provider: "p2", Model: "m2"})
	if s.realModelsCfg.Models[0].Provider != "p2" {
		t.Errorf("expected p2 after update, got %s", s.realModelsCfg.Models[0].Provider)
	}
	// 越界更新应安全无操作
	s.UpdateRealModel(99, config.FallbackItem{Provider: "x"})
}

func TestDeleteRealModel(t *testing.T) {
	s := newTestService(t, []config.FallbackItem{
		{Provider: "p1", Model: "m1"},
		{Provider: "p2", Model: "m2"},
	})
	s.DeleteRealModel(0)
	if len(s.realModelsCfg.Models) != 1 || s.realModelsCfg.Models[0].Provider != "p2" {
		t.Errorf("expected only p2 left, got %v", s.realModelsCfg.Models)
	}
	// 越界删除应安全
	s.DeleteRealModel(99)
}

func TestDeleteRealModelsByProvider(t *testing.T) {
	s := newTestService(t, []config.FallbackItem{
		{Provider: "p1", Model: "m1"},
		{Provider: "p1", Model: "m2"},
		{Provider: "p2", Model: "m3"},
	})
	s.DeleteRealModelsByProvider("p1")
	for _, m := range s.realModelsCfg.Models {
		if m.Provider == "p1" {
			t.Error("expected all p1 models removed")
		}
	}
}

func TestBreakerStates(t *testing.T) {
	s := newTestService(t, []config.FallbackItem{{Provider: "p1", Model: "m1"}})
	states := s.BreakerStates()
	if len(states) == 0 {
		t.Fatal("expected breaker states")
	}
	for _, v := range states {
		if v != "closed" {
			t.Errorf("expected closed, got %q", v)
		}
	}
}

func TestStateString(t *testing.T) {
	if stateString(gobreaker.StateClosed) != "closed" {
		t.Error("expected closed")
	}
	if stateString(gobreaker.StateOpen) != "open" {
		t.Error("expected open")
	}
	if stateString(gobreaker.StateHalfOpen) != "half-open" {
		t.Error("expected half-open")
	}
	if stateString(gobreaker.State(999)) != "unknown" {
		t.Error("expected unknown")
	}
}

func TestBreakerKey(t *testing.T) {
	s := newTestService(t, nil)
	if got := s.breakerKey("p1", "m1"); got != "p1:m1" {
		t.Errorf("expected p1:m1, got %q", got)
	}
}

func TestGetBreaker(t *testing.T) {
	s := newTestService(t, []config.FallbackItem{{Provider: "p1", Model: "m1"}})
	if s.getBreaker(s.breakerKey("p1", "m1")) == nil {
		t.Error("expected existing breaker")
	}
	if s.getBreaker("missing:key") != nil {
		t.Error("expected nil for missing breaker")
	}
}

func TestSelection_Next(t *testing.T) {
	sel := &Selection{targets: []*Target{{ProviderName: "a"}, {ProviderName: "b"}}}
	if sel.Next() == nil {
		t.Fatal("expected first target")
	}
	if sel.Next() == nil {
		t.Fatal("expected second target")
	}
	if sel.Next() != nil {
		t.Error("expected nil after exhaustion")
	}
}

func TestSelectCandidates_EndToEnd(t *testing.T) {
	s := newTestService(t, []config.FallbackItem{{Provider: "p1", Model: "m1", Weight: 1}})
	sel, err := s.SelectCandidates(context.Background(), "vm1", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tgt := sel.Next()
	if tgt == nil {
		t.Fatal("expected a candidate")
	}
	if tgt.ProviderName != "p1" {
		t.Errorf("expected p1, got %s", tgt.ProviderName)
	}
	if tgt.Breaker == nil {
		t.Error("expected breaker set on target")
	}
}

func TestSelectCandidates_NoAvailable(t *testing.T) {
	// 引用不存在的 provider → 没有候选
	s := newTestService(t, []config.FallbackItem{{Provider: "ghost", Model: "m1"}})
	_, err := s.SelectCandidates(context.Background(), "vm1", 0)
	if err == nil {
		t.Error("expected 'no available model' error")
	}
}

func providersOf(items []config.FallbackItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Provider
	}
	return out
}
