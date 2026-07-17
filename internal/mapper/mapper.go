package mapper

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"llm-gateway/internal/config"
)

// Service 模型名 allowlist 服务
type Service struct {
	mu         sync.RWMutex
	models     map[string]bool   // allowlist
	modelTiers map[string]string // name -> tier
}

// New 创建 allowlist 服务，接受 ModelEntry 列表
func New(entries []config.ModelEntry) *Service {
	s := &Service{
		models:     make(map[string]bool, len(entries)),
		modelTiers: make(map[string]string, len(entries)),
	}
	for _, e := range entries {
		s.models[e.Name] = true
		if e.Tier != "" {
			s.modelTiers[e.Name] = e.Tier
		}
	}
	return s
}

// Validate 检查模型名是否在 allowlist 中
func (s *Service) Validate(name string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.models[name] {
		return nil
	}
	return fmt.Errorf("model not found: %s", name)
}

// GetTier 获取虚拟模型的 tier，未配置时返回空字符串
func (s *Service) GetTier(name string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.modelTiers[name]
}

// ListVirtualModels 列出所有对外暴露的模型
func (s *Service) ListVirtualModels() []map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var models []map[string]interface{}
	for name := range s.models {
		model := map[string]interface{}{
			"id":       name,
			"object":   "model",
			"created":  time.Now().Unix(),
			"owned_by": "llm-gateway",
		}
		if tier, ok := s.modelTiers[name]; ok {
			model["tier"] = tier
		}
		models = append(models, model)
	}
	return models
}

// AddModel 新增虚拟模型（动态添加，不持久化到配置文件）
func (s *Service) AddModel(name, tier string) bool {
	if name == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.models[name] {
		return false // 已存在
	}
	s.models[name] = true
	if tier != "" {
		s.modelTiers[name] = tier
	}
	return true
}

// DeleteModel 删除虚拟模型（仅动态删除，不影响配置文件）
func (s *Service) DeleteModel(name string) bool {
	if name == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.models[name] {
		return false // 不存在
	}
	delete(s.models, name)
	delete(s.modelTiers, name)
	return true
}

// RewriteResponse 重写响应中的模型名字段
func (s *Service) RewriteResponse(body []byte, virtualName string) []byte {
	var resp map[string]interface{}
	if err := json.Unmarshal(body, &resp); err != nil {
		return body
	}

	if _, ok := resp["model"]; ok {
		resp["model"] = virtualName
	}

	newBody, _ := json.Marshal(resp)
	return newBody
}
