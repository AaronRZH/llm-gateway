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

// ClientProtocol 客户端使用的协议类型
type ClientProtocol string

const (
	ProtocolOpenAI    ClientProtocol = "openai"
	ProtocolAnthropic ClientProtocol = "anthropic"
)

// Provider 上游 Provider 接口
type Provider interface {
	Chat(ctx context.Context, model string, messages []Message, tools []Tool) (*http.Response, error)
	StreamChat(ctx context.Context, model string, messages []Message, tools []Tool) (io.ReadCloser, error)
	// ChatWithProtocol 带协议信息的 Chat，实现协议感知的格式转换
	ChatWithProtocol(ctx context.Context, model string, messages []Message, tools []Tool, clientProtocol ClientProtocol) (*http.Response, error)
	// StreamChatWithProtocol 带协议信息的 StreamChat，实现协议感知的格式转换
	StreamChatWithProtocol(ctx context.Context, model string, messages []Message, tools []Tool, clientProtocol ClientProtocol) (io.ReadCloser, error)
	// GetProtocol 返回上游使用的协议类型
	GetProtocol() ClientProtocol
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
	endpoint   string // 可选：覆盖默认的 upstream 端点路径
	protocol   ClientProtocol // 上游协议类型（openai / anthropic）
	httpClient *http.Client
}

func newBaseProvider(cfg config.ProviderConfig) baseProvider {
	proto := ProtocolOpenAI
	if cfg.Protocol == "anthropic" {
		proto = ProtocolAnthropic
	}
	return baseProvider{
		baseURL:  cfg.BaseURL,
		apiKey:   cfg.APIKey,
		endpoint: cfg.Endpoint,
		protocol: proto,
		httpClient: &http.Client{
			Timeout: cfg.Timeout,
		},
	}
}

// getDefaultPath 返回该 provider 类型的默认端点路径
func (p *baseProvider) getDefaultPath() string {
	switch p.name {
	case "anthropic":
		return "/v1/messages"
	default:
		return "/v1/chat/completions"
	}
}

// fullURL 构建完整的 upstream URL
// 如果配置了自定义 endpoint 则使用，否则使用 provider 类型的默认值
func (p *baseProvider) fullURL(suffix string) string {
	path := p.getDefaultPath()
	if p.endpoint != "" {
		path = p.endpoint
	}
	return p.baseURL + path + suffix
}

// GetProtocol 返回上游使用的协议类型
func (p *baseProvider) GetProtocol() ClientProtocol {
	return p.protocol
}

