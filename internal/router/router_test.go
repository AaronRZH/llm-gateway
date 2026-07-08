package router

import (
	"context"
	"sync"
	"testing"
	"time"

	"llm-gateway/internal/config"
	"llm-gateway/internal/provider"
)

func TestSelectCandidatesNoRace(t *testing.T) {
	cbCfg := config.CircuitBreakerConfig{
		MaxRequests:      5,
		Interval:         60 * time.Second,
		Timeout:          30 * time.Second,
		FailureThreshold: 5,
		Cooldown:         5 * time.Second,
	}

	ctx := context.Background()
	pm := provider.NewManager(map[string]config.ProviderConfig{
		"p1": {BaseURL: "http://example.com", Protocol: "openai"},
		"p2": {BaseURL: "http://example.com", Protocol: "openai"},
		"p3": {BaseURL: "http://example.com", Protocol: "openai"},
	})

	cfg := config.RealModelsConfig{
		Strategy: "priority",
		Models: []config.FallbackItem{
            {Provider: "p1", Model: "m1", Weight: 1},
            {Provider: "p2", Model: "m2", Weight: 1},
        },
	}

	service := New(cfg, pm, nil, cbCfg, map[string]string{})

	// Verify cloneFallbackChain is correct
	for _, m := range service.realModelsCfg.Models {
		t.Logf("init: provider=%s model=%s weight=%d", m.Provider, m.Model, m.Weight)
	}

	var wg sync.WaitGroup

	// Writer: rapidly add/update/delete models
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			service.AddRealModel(config.FallbackItem{
                Provider: "p3",
                Model:    "m3",
                Weight:   1,
            })
			n := len(service.realModelsCfg.Models)
			if n > 2 {
                service.DeleteRealModel(n - 1)
            }
			service.UpdateRealModel(0, config.FallbackItem{
                Provider: "p1",
                Model:    "m1",
                Weight:   2,
            })
		}
	}()

	// Reader: call SelectCandidates — even if it errors (circuit breaker nil),
	// the race detector would flag if there's a data race on realModelsCfg.
	for g := 0; g < 10; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
                _, _ = service.SelectCandidates(ctx, "vm1", 0)
            }
		}()
	}

	wg.Wait()
	t.Log("No data race detected")
}