package mapper

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// Service 模型名 allowlist 服务
type Service struct {
	mu     sync.RWMutex
	models map[string]bool
}

// New 创建 allowlist 服务
func New(models []string) *Service {
	s := &Service{
		models: make(map[string]bool, len(models)),
	}
	for _, m := range models {
		s.models[m] = true
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

// ListVirtualModels 列出所有对外暴露的模型
func (s *Service) ListVirtualModels() []map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var models []map[string]interface{}
	for name := range s.models {
		models = append(models, map[string]interface{}{
			"id":       name,
			"object":   "model",
			"created":  time.Now().Unix(),
			"owned_by": "llm-gateway",
		})
	}
	return models
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
