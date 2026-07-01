package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"llm-gateway/internal/config"
)

// OpenAIProvider OpenAI 实现
type OpenAIProvider struct {
	baseProvider
}

// NewOpenAIProvider 创建 OpenAI Provider
func NewOpenAIProvider(cfg config.ProviderConfig) *OpenAIProvider {
	return &OpenAIProvider{
		baseProvider: newBaseProvider(cfg),
	}
}

// Chat 非流式请求
func (p *OpenAIProvider) Chat(ctx context.Context, model string, messages []Message, tools []Tool) (*http.Response, error) {
	reqBody := map[string]interface{}{
		"model":    model,
		"messages": messages,
	}

	if len(tools) > 0 {
		reqBody["tools"] = tools
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", p.fullURL(""), bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("openai error %d: %s", resp.StatusCode, string(body))
	}

	return resp, nil
}

// StreamChat 流式请求
func (p *OpenAIProvider) StreamChat(ctx context.Context, model string, messages []Message, tools []Tool) (io.ReadCloser, error) {
	reqBody := map[string]interface{}{
		"model":    model,
		"messages": messages,
		"stream":   true,
	}

	if len(tools) > 0 {
		reqBody["tools"] = tools
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", p.fullURL(""), bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("openai stream error %d: %s", resp.StatusCode, string(body))
	}

	return resp.Body, nil
}

// ChatWithProtocol 忽略 protocol 参数，始终使用 OpenAI 格式（默认行为）
func (p *OpenAIProvider) ChatWithProtocol(ctx context.Context, model string, messages []Message, tools []Tool, clientProtocol ClientProtocol) (*http.Response, error) {
	return p.Chat(ctx, model, messages, tools)
}

// StreamChatWithProtocol 忽略 protocol 参数，始终使用 OpenAI 格式（默认行为）
func (p *OpenAIProvider) StreamChatWithProtocol(ctx context.Context, model string, messages []Message, tools []Tool, clientProtocol ClientProtocol) (io.ReadCloser, error) {
	return p.StreamChat(ctx, model, messages, tools)
}
