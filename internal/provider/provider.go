package provider

import (
	"bytes"
	"context"
	"encoding/json"
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

// Message 消息结构
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Provider 上游 Provider 接口 — 统一发送所有 HTTP 请求
type Provider struct {
	name       string
	baseURL    string
	apiKey     string
	endpoint   string // 可选：覆盖默认的 upstream 端点路径
	protocol   ClientProtocol // 上游协议类型（openai / anthropic）
	httpClient *http.Client

	converter *AnthropicConverter
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
		m.providers[name] = NewProvider(pcfg)
	}

	return m
}

// Get 获取 Provider
func (m *Manager) Get(name string) (Provider, bool) {
	p, ok := m.providers[name]
	return p, ok
}

// ==================== 构造函数 ====================

// newBaseProvider 构造基础 Provider 配置
func newBaseProvider(cfg config.ProviderConfig) (string, string, string, ClientProtocol, *http.Client) {
	proto := ProtocolOpenAI
	if cfg.Protocol == "anthropic" {
		proto = ProtocolAnthropic
	}
	return cfg.BaseURL, cfg.APIKey, cfg.Endpoint, proto, &http.Client{
		Timeout: cfg.Timeout,
	}
}

// NewProvider 创建统一 Provider
func NewProvider(cfg config.ProviderConfig) Provider {
	baseURL, apiKey, endpoint, proto, httpClient := newBaseProvider(cfg)
	return Provider{
		baseURL:    baseURL,
		apiKey:     apiKey,
		endpoint:   endpoint,
		protocol:   proto,
		httpClient: httpClient,
	}
}

// ==================== 内部方法 ====================

// getDefaultPath 返回该 provider 类型的默认端点路径
func (p *Provider) getDefaultPath() string {
	switch p.protocol {
	case ProtocolAnthropic:
		return "/messages"
	default:
		return "/chat/completions"
	}
}

// fullURL 构建完整的 upstream URL
// 如果配置了自定义 endpoint 则使用，否则使用 provider 类型的默认值
func (p *Provider) fullURL(suffix string) string {
	path := p.getDefaultPath()
	if p.endpoint != "" {
		path = p.endpoint
	}
	return p.baseURL + path + suffix
}

// SetName 设置 provider 名称
func (p *Provider) SetName(name string) {
	p.name = name
}

// GetName 获取 provider 名称
func (p *Provider) GetName() string {
	return p.name
}

// GetProtocol 返回上游使用的协议类型
func (p *Provider) GetProtocol() ClientProtocol {
	return p.protocol
}

// getConverter 获取格式转换器（延迟初始化）
func (p *Provider) getConverter() *AnthropicConverter {
	if p.converter == nil {
		p.converter = NewAnthropicConverter()
	}
	return p.converter
}

// ==================== HTTP 发送方法 ====================

// buildRequest 构建 HTTP 请求（OpenAI 格式 body）
func (p *Provider) buildRequest(ctx context.Context, method string, url string, model string, messages []Message, tools []Tool, stream bool) (*http.Request, error) {
	reqBody := map[string]interface{}{
		"model":    model,
		"messages": messages,
	}

	if len(tools) > 0 {
		reqBody["tools"] = tools
	}

	if stream {
		reqBody["stream"] = true
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")

	// 根据上游协议设置认证头
	if p.protocol == ProtocolAnthropic {
		req.Header.Set("x-api-key", p.apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	return req, nil
}

// buildAnthropicRequest 构建 HTTP 请求（Anthropic 格式 body）
func (p *Provider) buildAnthropicRequest(ctx context.Context, url string, model string, messages []map[string]interface{}, tools []map[string]interface{}, system interface{}, extraParams map[string]interface{}, stream bool) (*http.Request, error) {
	reqBody := map[string]interface{}{
		"model":      model,
		"messages":   messages,
		"max_tokens": 4096,
	}

	if system != nil {
		reqBody["system"] = system
	}

	// 从 extraParams 复制额外参数（temperature, top_p, stop_sequences, tools 等）
	for k, v := range extraParams {
		if v != nil {
			reqBody[k] = v
		}
	}

	if stream {
		reqBody["stream"] = true
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", p.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	return req, nil
}

// Chat 非流式请求 — 使用 OpenAI 格式发送
func (p *Provider) Chat(ctx context.Context, model string, messages []Message, tools []Tool) (*http.Response, error) {
	req, err := p.buildRequest(ctx, "POST", p.fullURL(""), model, messages, tools, false)
	if err != nil {
		return nil, err
	}

	return p.httpClient.Do(req)
}
// StreamChat 流式请求 — 使用 OpenAI 格式发送
func (p *Provider) StreamChat(ctx context.Context, model string, messages []Message, tools []Tool) (io.ReadCloser, error) {
	req, err := p.buildRequest(ctx, "POST", p.fullURL(""), model, messages, tools, true)
	if err != nil {
		return nil, err
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	return resp.Body, nil
}

// ChatWithProtocol 带协议信息的 Chat
func (p *Provider) ChatWithProtocol(ctx context.Context, model string, messages []Message, tools []Tool, clientProtocol ClientProtocol) (*http.Response, error) {
	if p.protocol == ProtocolAnthropic && clientProtocol == ProtocolOpenAI {
		// 需要将 OpenAI 消息转换为 Anthropic 格式后发送
		anthropicMsgs := p.getConverter().ConvertMessagesToAnthropic(messages)
		convertedTools := p.getConverter().ConvertTools(tools)
		return p.doSendAnthropic(ctx, model, anthropicMsgs, convertedTools)
	}
	return p.Chat(ctx, model, messages, tools)
}

// StreamChatWithProtocol 带协议信息的 StreamChat
func (p *Provider) StreamChatWithProtocol(ctx context.Context, model string, messages []Message, tools []Tool, clientProtocol ClientProtocol) (io.ReadCloser, error) {
	if p.protocol == ProtocolAnthropic && clientProtocol == ProtocolOpenAI {
		// 需要将 OpenAI 消息转换为 Anthropic 格式后发送
		anthropicMsgs := p.getConverter().ConvertMessagesToAnthropic(messages)
		convertedTools := p.getConverter().ConvertTools(tools)
		return p.doSendAnthropicStream(ctx, model, anthropicMsgs, convertedTools)
	}
	return p.StreamChat(ctx, model, messages, tools)
}

// doSendAnthropic 发送 Anthropic 格式消息（使用 OpenAI 格式请求体 + Anthropic 头）
func (p *Provider) doSendAnthropic(ctx context.Context, model string, messages []map[string]interface{}, tools []map[string]interface{}) (*http.Response, error) {
	req, err := p.buildAnthropicRequest(ctx, p.fullURL(""), model, messages, tools, nil, nil, false)
	if err != nil {
		return nil, err
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

// doSendAnthropicStream 发送 Anthropic 格式消息（流式）
func (p *Provider) doSendAnthropicStream(ctx context.Context, model string, messages []map[string]interface{}, tools []map[string]interface{}) (io.ReadCloser, error) {
	req, err := p.buildAnthropicRequest(ctx, p.fullURL(""), model, messages, tools, nil, nil, true)
	if err != nil {
		return nil, err
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	return resp.Body, nil
}

// CountTokens 调用上游的 /count_tokens 端点
func (p *Provider) CountTokens(ctx context.Context, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", p.fullURL("")+"/count_tokens", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", p.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	// 错误状态码也原样返回，由 handler 转发给客户端
	return resp, nil
}

// SendDirect 用已存在的 Anthropic 格式消息直接发送请求到上游，不做格式转换。
// 适用于 Anthropic 格式客户端 → Anthropic 上游的场景（Case 4）。
func (p *Provider) SendDirect(
	ctx context.Context,
	model string,
	messages []map[string]interface{},
	system interface{},
	extraParams map[string]interface{},
	stream bool,
) (*http.Response, error) {
	// 规范化每条消息的 content，确保 content block 格式正确
	normalizedMessages := make([]map[string]interface{}, len(messages))
	converter := p.getConverter()
	for i, msg := range messages {
		normalized := make(map[string]interface{}, len(msg))
		for k, v := range msg {
			if k == "content" {
				normalized[k] = converter.ContentToBlocks(v)
			} else {
				normalized[k] = v
			}
		}
		normalizedMessages[i] = normalized
	}

	req, err := p.buildAnthropicRequest(ctx, p.fullURL(""), model, normalizedMessages, nil, system, extraParams, stream)
	if err != nil {
		return nil, err
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	// 上游返回错误状态码时，不直接 return，而是将 response 传给调用方处理
	// 这样调用方可以读取 body 并转发给客户端，而不是丢失错误详情
	return resp, nil
}

// ==================== 公开 Converter 方法（供 handler 调用） ====================

// ConvertAnthropicMessagesToOpenAI 将 Anthropic 格式消息和工具转换为 OpenAI 格式
func (p *Provider) ConvertAnthropicMessagesToOpenAI(
	messages []map[string]interface{},
	system interface{},
	tools []map[string]interface{},
) ([]Message, []Tool) {
	return p.getConverter().ConvertAnthropicMessagesToOpenAI(messages, system, tools)
}

// ConvertOpenAIToAnthropicResponse 将 OpenAI 非流式响应体转换为 Anthropic 格式
func (p *Provider) ConvertOpenAIToAnthropicResponse(body []byte, virtualModel string, inputTokens int) ([]byte, error) {
	return p.getConverter().ConvertOpenAIToAnthropicResponse(body, virtualModel, inputTokens)
}

// ConvertResponse 将后端响应转为 Anthropic 格式
func (p *Provider) ConvertResponse(resp *http.Response) ([]byte, error) {
	return p.getConverter().ConvertResponse(resp)
}

// ConvertResponseWithModel 将后端响应转为 Anthropic 格式，使用指定的虚拟模型名
func (p *Provider) ConvertResponseWithModel(resp *http.Response, virtualModel string) ([]byte, error) {
	return p.getConverter().ConvertResponseWithModel(resp, virtualModel)
}
