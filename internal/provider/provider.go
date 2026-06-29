package provider

import (
	"context"
	"io"
	"net/http"

	"llm-gateway/internal/config"
)

// Tool 工具定义（OpenAI 格式）
type Tool struct {
	Type     string    `json:"type"`
	Function ToolFunc  `json:"function"`
}

// ToolFunc 工具函数定义
type ToolFunc struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters,omitempty"`
}

// Provider 上游 Provider 接口
type Provider interface {
	Chat(ctx context.Context, model string, messages []Message, tools []Tool) (*http.Response, error)
	StreamChat(ctx context.Context, model string, messages []Message, tools []Tool) (io.ReadCloser, error)
}

// Message 消息结构
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Manager Provider 管理器
type Manager struct {
	providers map[string]Provider
}

// NewManager 创建 Provider 管理器
func NewManager(cfg map[string]config.ProviderConfig) *Manager {
	m := &Manager{
		providers: make(map[string]Provider),
	}

	for name, pcfg := range cfg {
		switch name {
		case "openai":
			m.providers[name] = NewOpenAIProvider(pcfg)
		case "anthropic":
			m.providers[name] = NewAnthropicProvider(pcfg)
		case "deepseek":
			m.providers[name] = NewDeepSeekProvider(pcfg)
		default:
			// 通用 HTTP Provider
			m.providers[name] = NewGenericProvider(name, pcfg)
		}
	}

	return m
}

// Get 获取 Provider
func (m *Manager) Get(name string) (Provider, bool) {
	p, ok := m.providers[name]
	return p, ok
}

// baseProvider 基础 Provider 实现
type baseProvider struct {
	name       string
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

func newBaseProvider(cfg config.ProviderConfig) baseProvider {
	return baseProvider{
		baseURL: cfg.BaseURL,
		apiKey:  cfg.APIKey,
		httpClient: &http.Client{
			Timeout: cfg.Timeout,
		},
	}
}

