package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"llm-gateway/internal/config"
)

// defaultResponseHeaderTimeout 首字节超时：超过该时间未收到上游响应头即判定失败，
// 触发 fallback，避免被"连上但不返回"的慢 provider 长时间阻塞。
const defaultResponseHeaderTimeout = 15 * time.Second

// UpstreamHTTPError 表示上游返回非 2xx HTTP 状态码的错误。
type UpstreamHTTPError struct {
	StatusCode int
	Provider   string
	Body       []byte
}

func (e *UpstreamHTTPError) Error() string {
	return fmt.Sprintf("upstream %s returned HTTP %d", e.Provider, e.StatusCode)
}

// Tool 工具定义（OpenAI 格式）
type Tool struct {
	Type     string   `json:"type"`
	Function ToolFunc `json:"function"`
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

// Message 消息结构（OpenAI 格式，支持 tool_calls）
type Message struct {
	Role       string                   `json:"role"`
	Content    string                   `json:"content,omitempty"`
	ToolCalls  []map[string]interface{} `json:"tool_calls,omitempty"`
	ToolCallID string                   `json:"tool_call_id,omitempty"`
}

// Provider 上游 Provider 接口 — 统一发送所有 HTTP 请求
type Provider struct {
	name       string
	baseURL    string
	apiKey     string
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
		p := NewProvider(pcfg)
		p.SetName(name)
		m.providers[name] = p
	}

	return m
}

// Get 获取 Provider
func (m *Manager) Get(name string) (Provider, bool) {
	p, ok := m.providers[name]
	return p, ok
}

// UpdateProvider 更新或新增 Provider 配置（运行时生效）
func (m *Manager) UpdateProvider(name string, cfg config.ProviderConfig) {
	p := NewProvider(cfg)
	p.SetName(name)
	m.providers[name] = p
}

// DeleteProvider 删除 Provider（运行时生效）
func (m *Manager) DeleteProvider(name string) {
	delete(m.providers, name)
}

// ==================== 构造函数 ====================

// newBaseProvider 构造基础 Provider 配置
func newBaseProvider(cfg config.ProviderConfig) (string, string, ClientProtocol, *http.Client) {
	proto := ProtocolOpenAI
	if cfg.Protocol == "anthropic" {
		proto = ProtocolAnthropic
	}
	// 首字节超时：超过该时间未收到响应头即失败并触发 fallback，避免长时间阻塞。
	// 优先使用 Provider 配置的 response_header_timeout，未配置则沿用全局默认。
	headerTimeout := defaultResponseHeaderTimeout
	if cfg.ResponseHeaderTimeout > 0 {
		headerTimeout = cfg.ResponseHeaderTimeout
	}
	transport := &http.Transport{
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 100,
		MaxConnsPerHost:     100,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		// 首字节超时：超过该时间未收到响应头即失败并触发 fallback，避免长时间阻塞
		ResponseHeaderTimeout: headerTimeout,
	}

	return cfg.BaseURL, cfg.APIKey, proto, &http.Client{
		Timeout:   cfg.Timeout,
		Transport: transport,
	}
}

// NewProvider 创建统一 Provider
func NewProvider(cfg config.ProviderConfig) Provider {
	baseURL, apiKey, proto, httpClient := newBaseProvider(cfg)
	return Provider{
		baseURL:    baseURL,
		apiKey:     apiKey,
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
func (p *Provider) fullURL(suffix string) string {
	return p.baseURL + p.getDefaultPath() + suffix
}

// SetName 设置 provider 名称
func (p *Provider) SetName(name string) {
	p.name = name
}

// SetAPIKey 更新 Provider 的 API Key（用于运行时更新，立即生效）
func (p *Provider) SetAPIKey(key string) {
	p.apiKey = key
}

// GetName 获取 provider 名称
func (p *Provider) GetName() string {
	return p.name
}

// GetProtocol 返回上游使用的协议类型
func (p *Provider) GetProtocol() ClientProtocol {
	return p.protocol
}

// FullURL 返回完整的 upstream URL（不含 suffix）
func (p *Provider) FullURL() string {
	return p.fullURL("")
}

// getConverter 获取格式转换器（延迟初始化）
func (p *Provider) getConverter() *AnthropicConverter {
	if p.converter == nil {
		p.converter = NewAnthropicConverter()
	}
	return p.converter
}

// checkHTTPResponse 检查 HTTP 响应状态码，对 4xx/5xx 返回 UpstreamHTTPError。
func (p *Provider) checkHTTPResponse(resp *http.Response) (*http.Response, error) {
	if resp.StatusCode < 400 {
		return resp, nil
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return nil, &UpstreamHTTPError{
		StatusCode: resp.StatusCode,
		Provider:   p.name,
		Body:       body,
	}
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
		// OpenAI 流式默认不返回 usage，需显式开启；Anthropic 流式默认自带 usage，无需设置
		// buildRequest 只用于 OpenAI 格式请求体，故此处安全注入
		reqBody["stream_options"] = map[string]interface{}{
			"include_usage": true,
		}
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
func (p *Provider) buildAnthropicRequest(ctx context.Context, url string, model string, messages []map[string]interface{}, tools []map[string]interface{}, system interface{}, extraParams map[string]interface{}, stream bool, maxTokens int) (*http.Request, error) {
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	reqBody := map[string]interface{}{
		"model":      model,
		"messages":   messages,
		"max_tokens": maxTokens,
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

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	return p.checkHTTPResponse(resp)
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
	_, err = p.checkHTTPResponse(resp)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// ChatWithProtocol 带协议信息的 Chat
func (p *Provider) ChatWithProtocol(ctx context.Context, model string, messages []Message, tools []Tool, clientProtocol ClientProtocol, maxTokens int) (*http.Response, error) {
	if p.protocol == ProtocolAnthropic && clientProtocol == ProtocolOpenAI {
		// 需要将 OpenAI 消息转换为 Anthropic 格式后发送
		anthropicMsgs := p.getConverter().ConvertMessagesToAnthropic(messages)
		convertedTools := p.getConverter().ConvertTools(tools)
		return p.doSendAnthropic(ctx, model, anthropicMsgs, convertedTools, maxTokens)
	}
	return p.Chat(ctx, model, messages, tools)
}

// StreamChatWithProtocol 带协议信息的 StreamChat
func (p *Provider) StreamChatWithProtocol(ctx context.Context, model string, messages []Message, tools []Tool, clientProtocol ClientProtocol, maxTokens int) (io.ReadCloser, error) {
	if p.protocol == ProtocolAnthropic && clientProtocol == ProtocolOpenAI {
		// 需要将 OpenAI 消息转换为 Anthropic 格式后发送
		anthropicMsgs := p.getConverter().ConvertMessagesToAnthropic(messages)
		convertedTools := p.getConverter().ConvertTools(tools)
		return p.doSendAnthropicStream(ctx, model, anthropicMsgs, convertedTools, maxTokens)
	}
	return p.StreamChat(ctx, model, messages, tools)
}

// doSendAnthropic 发送 Anthropic 格式消息（使用 OpenAI 格式请求体 + Anthropic 头）
func (p *Provider) doSendAnthropic(ctx context.Context, model string, messages []map[string]interface{}, tools []map[string]interface{}, maxTokens int) (*http.Response, error) {
	req, err := p.buildAnthropicRequest(ctx, p.fullURL(""), model, messages, tools, nil, nil, false, maxTokens)
	if err != nil {
		return nil, err
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	_, err = p.checkHTTPResponse(resp)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// doSendAnthropicStream 发送 Anthropic 格式消息（流式）
func (p *Provider) doSendAnthropicStream(ctx context.Context, model string, messages []map[string]interface{}, tools []map[string]interface{}, maxTokens int) (io.ReadCloser, error) {
	req, err := p.buildAnthropicRequest(ctx, p.fullURL(""), model, messages, tools, nil, nil, true, maxTokens)
	if err != nil {
		return nil, err
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	_, err = p.checkHTTPResponse(resp)
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
	return p.checkHTTPResponse(resp)
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

	req, err := p.buildAnthropicRequest(ctx, p.fullURL(""), model, normalizedMessages, nil, system, extraParams, stream, 0)
	if err != nil {
		return nil, err
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	// 上游返回错误状态码时，将 response body 封装到 UpstreamHTTPError 中返回。
	// 这样断路器可以统计失败次数，同时调用方（protocol.Resolve / handler）
	// 能从 UpstreamHTTPError 中提取 body 并转发给客户端，而不是丢失错误详情。
	return p.checkHTTPResponse(resp)
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

// ConvertAnthropicToOpenAIResponse 将 Anthropic 非流式响应体转换为 OpenAI 格式
func (p *Provider) ConvertAnthropicToOpenAIResponse(body []byte, virtualModel string) ([]byte, error) {
	return p.getConverter().ConvertAnthropicToOpenAIResponse(body, virtualModel)
}

// ConvertResponse 将后端响应转为 Anthropic 格式
func (p *Provider) ConvertResponse(resp *http.Response) ([]byte, error) {
	return p.getConverter().ConvertResponse(resp)
}

// ConvertResponseWithModel 将后端响应转为 Anthropic 格式，使用指定的虚拟模型名
func (p *Provider) ConvertResponseWithModel(resp *http.Response, virtualModel string) ([]byte, error) {
	return p.getConverter().ConvertResponseWithModel(resp, virtualModel)
}
