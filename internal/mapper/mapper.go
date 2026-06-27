package mapper

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"llm-gateway/internal/config"
)

// Service 模型名映射服务
type Service struct {
	mu              sync.RWMutex
	virtualToReal   map[string]MappedModel  // 虚拟名 -> 真实模型
	realToVirtual   map[string]string       // 真实模型 -> 虚拟名
	aliasToVirtual  map[string]string       // 别名 -> 虚拟名
}

// MappedModel 映射后的模型信息
type MappedModel struct {
	RealModel string
	Provider  string
}

// New 创建映射服务
func New(cfg config.ModelMappingConfig) *Service {
	s := &Service{
		virtualToReal:  make(map[string]MappedModel),
		realToVirtual:  make(map[string]string),
		aliasToVirtual: make(map[string]string),
	}

	for virtual, model := range cfg.VirtualToReal {
		parts := strings.SplitN(model.Real, "/", 2)
		if len(parts) != 2 {
			continue
		}

		mapped := MappedModel{
			RealModel: parts[1],
			Provider:  parts[0],
		}

		s.virtualToReal[virtual] = mapped
		s.realToVirtual[model.Real] = virtual

		// 注册别名
		for _, alias := range model.Aliases {
			s.aliasToVirtual[alias] = virtual
		}
	}

	return s
}

// Resolve 将虚拟模型名解析为真实模型
func (s *Service) Resolve(virtualName string) (MappedModel, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// 直接匹配
	if m, ok := s.virtualToReal[virtualName]; ok {
		return m, nil
	}

	// 别名匹配
	if virtual, ok := s.aliasToVirtual[virtualName]; ok {
		if m, ok := s.virtualToReal[virtual]; ok {
			return m, nil
		}
	}

	return MappedModel{}, fmt.Errorf("model not found: %s", virtualName)
}

// GetVirtualName 根据真实模型名获取虚拟名
func (s *Service) GetVirtualName(realModel string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.realToVirtual[realModel]
}

// ListVirtualModels 列出所有对外暴露的模型
func (s *Service) ListVirtualModels() []map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var models []map[string]interface{}
	for virtual := range s.virtualToReal {
		models = append(models, map[string]interface{}{
			"id":       virtual,
			"object":   "model",
			"created":  time.Now().Unix(),
			"owned_by": "llm-gateway",
		})
	}
	return models
}

// RewriteResponse 重写响应中的模型名字段
func (s *Service) RewriteResponse(body []byte, virtualName string) []byte {
	// 简单字符串替换（生产环境建议用 json 解析）
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
