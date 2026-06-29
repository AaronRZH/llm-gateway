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

// DeepSeekProvider DeepSeek 实现（OpenAI 兼容格式）
type DeepSeekProvider struct {
	baseProvider
}

// NewDeepSeekProvider 创建 DeepSeek Provider
func NewDeepSeekProvider(cfg config.ProviderConfig) *DeepSeekProvider {
	return &DeepSeekProvider{
		baseProvider: newBaseProvider(cfg),
	}
}

// Chat 非流式请求
func (p *DeepSeekProvider) Chat(ctx context.Context, model string, messages []Message, tools []Tool) (*http.Response, error) {
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

	req, err := http.NewRequestWithContext(ctx, "POST", p.baseURL+"/chat/completions", bytes.NewReader(jsonBody))
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
		return nil, fmt.Errorf("deepseek error %d: %s", resp.StatusCode, string(body))
	}

	return resp, nil
}

// StreamChat 流式请求
func (p *DeepSeekProvider) StreamChat(ctx context.Context, model string, messages []Message, tools []Tool) (io.ReadCloser, error) {
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

	req, err := http.NewRequestWithContext(ctx, "POST", p.baseURL+"/chat/completions", bytes.NewReader(jsonBody))
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
		return nil, fmt.Errorf("deepseek stream error %d: %s", resp.StatusCode, string(body))
	}

	return resp.Body, nil
}
