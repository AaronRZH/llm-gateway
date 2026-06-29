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

// AnthropicProvider Anthropic 实现
type AnthropicProvider struct {
	baseProvider
}

// NewAnthropicProvider 创建 Anthropic Provider
func NewAnthropicProvider(cfg config.ProviderConfig) *AnthropicProvider {
	return &AnthropicProvider{
		baseProvider: newBaseProvider(cfg),
	}
}

// Chat 非流式请求
func (p *AnthropicProvider) Chat(ctx context.Context, model string, messages []Message, tools []Tool) (*http.Response, error) {
	// Anthropic API 格式转换
	anthropicMessages := p.convertMessages(messages)

	reqBody := map[string]interface{}{
		"model":    model,
		"messages": anthropicMessages,
		"max_tokens": 4096,
	}

	// 如果有工具定义，转换为 Anthropic tools 格式
	if len(tools) > 0 {
		reqBody["tools"] = p.convertTools(tools)
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", p.baseURL+"/messages", bytes.NewReader(jsonBody))
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

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("anthropic error %d: %s", resp.StatusCode, string(body))
	}

	return resp, nil
}

// StreamChat 流式请求
func (p *AnthropicProvider) StreamChat(ctx context.Context, model string, messages []Message, tools []Tool) (io.ReadCloser, error) {
	anthropicMessages := p.convertMessages(messages)

	reqBody := map[string]interface{}{
		"model":      model,
		"messages":   anthropicMessages,
		"max_tokens": 4096,
		"stream":     true,
	}

	// 如果有工具定义，转换为 Anthropic tools 格式
	if len(tools) > 0 {
		reqBody["tools"] = p.convertTools(tools)
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", p.baseURL+"/messages", bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", p.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("anthropic stream error %d: %s", resp.StatusCode, string(body))
	}

	return resp.Body, nil
}

// convertTools 将 OpenAI 格式的 tools 转换为 Anthropic 格式
func (p *AnthropicProvider) convertTools(tools []Tool) []map[string]interface{} {
	converted := make([]map[string]interface{}, len(tools))
	for i, tool := range tools {
		fn := tool.Function
		t := map[string]interface{}{
			"name":        fn.Name,
			"description": fn.Description,
		}
		if fn.Parameters != nil {
			t["input_schema"] = fn.Parameters
		}
		converted[i] = t
	}
	return converted
}

// convertMessages 转换消息格式
func (p *AnthropicProvider) convertMessages(messages []Message) []map[string]interface{} {
	var result []map[string]interface{}
	for _, msg := range messages {
		result = append(result, map[string]interface{}{
			"role":    msg.Role,
			"content": msg.Content,
		})
	}
	return result
}

// AnthropicRequest 网关接收的完整 Anthropic 请求体
type AnthropicRequest struct {
	Model        string             `json:"model"`
	Messages     []map[string]interface{} `json:"messages"`
	System       interface{}        `json:"system,omitempty"`
	MaxTokens    int                `json:"max_tokens"`
	Temperature  float64            `json:"temperature,omitempty"`
	TopP       float64            `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Tools        []map[string]interface{} `json:"tools,omitempty"`
	ToolChoice   map[string]interface{}   `json:"tool_choice,omitempty"`
}

// AnthropicResponse 从后端收到 OpenAI 格式的响应后，转换成的 Anthropic 格式响应
type AnthropicResponse struct {
	ID          string           `json:"id"`
	Type        string           `json:"type"`
	Model       string           `json:"model"`
	Role        string           `json:"role"`
	Content     []map[string]interface{} `json:"content"`
	Usage       map[string]interface{} `json:"usage"`
	FinishReason string          `json:"finish_reason"`
}

// ConvertRequest 将完整的 Anthropic 请求转换为后端请求体
func (p *AnthropicProvider) ConvertRequest(req *AnthropicRequest) (map[string]interface{}, error) {
	body := map[string]interface{}{
		"model":    req.Model,
		"messages": p.convertMessagesToOpenAI(req.Messages, req.System),
		"max_tokens": 4096,
	}

	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if req.Temperature > 0 {
		body["temperature"] = req.Temperature
	}
	if req.TopP > 0 {
		body["top_p"] = req.TopP
	}
	if len(req.StopSequences) > 0 {
		body["stop_sequences"] = req.StopSequences
	}
	if len(req.Tools) > 0 {
		// req.Tools is already in Anthropic format (maps with name/description/input_schema)
		body["tools"] = req.Tools
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(jsonBody, &parsed); err != nil {
		return nil, err
	}
	return parsed, nil
}

// convertMessagesToOpenAI 将 Anthropic 消息列表（含 system）转为 OpenAI 消息列表
func (p *AnthropicProvider) convertMessagesToOpenAI(messages []map[string]interface{}, system interface{}) []map[string]interface{} {
	var result []map[string]interface{}

	// 如果有 system 参数，转为 system 消息
	if system != nil {
		result = append(result, map[string]interface{}{
			"role":    "system",
			"content": system,
		})
	}

	// 转换用户/助手消息
	for _, msg := range messages {
		role, _ := msg["role"].(string)
		if role == "user" || role == "assistant" {
			result = append(result, map[string]interface{}{
				"role":    role,
				"content": msg["content"],
			})
		}
	}

	return result
}

// ChatWithRequest 用完整 Anthropic 请求体发送非流式请求
func (p *AnthropicProvider) ChatWithRequest(ctx context.Context, req *AnthropicRequest) (*http.Response, error) {
	body, err := p.ConvertRequest(req)
	if err != nil {
		return nil, err
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req2, err := http.NewRequestWithContext(ctx, "POST", p.baseURL+"/messages", bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}

	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("x-api-key", p.apiKey)
	req2.Header.Set("anthropic-version", "2023-06-01")

	resp, err := p.httpClient.Do(req2)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("anthropic error %d: %s", resp.StatusCode, string(body))
	}

	return resp, nil
}

// ConvertResponse 将后端返回的 ChatCompletionResponse 转为 Anthropic 格式的响应
func (p *AnthropicProvider) ConvertResponse(resp *http.Response) ([]byte, error) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// 后端返回的是 Anthropic 格式（来自 /messages）
	var respBody map[string]interface{}
	if err := json.Unmarshal(body, &respBody); err != nil {
		return nil, err
	}

	// 构建 Anthropic 响应
	anthropicResp := map[string]interface{}{
		"id":    respBody["id"],
		"model": respBody["model"],
		"role":  "assistant",
		"type":  "message",
	}

	if anthropicResp["model"] == nil {
		anthropicResp["model"] = "message"
	}

	// finish_reason
	finishReason, _ := respBody["stop_reason"].(string)
	if finishReason == "" {
		finishReason, _ = respBody["finish_reason"].(string)
	}
	if finishReason == "" || finishReason == "stop" {
		finishReason = "end_turn"
	}
	anthropicResp["finish_reason"] = finishReason

	// content：直接从响应中获取（后端已返回 Anthropic 格式）
	if content, ok := respBody["content"].([]interface{}); ok && len(content) > 0 {
		// 过滤掉 proxy 特有的字段（thinking, cache_control 等）
		filteredContent := make([]map[string]interface{}, 0, len(content))
		for _, block := range content {
			if blockMap, ok := block.(map[string]interface{}); ok {
				// 移除 proxy 特有的字段
				delete(blockMap, "thinking")
				delete(blockMap, "cache_control")
				filteredContent = append(filteredContent, blockMap)
			}
		}
		anthropicResp["content"] = filteredContent
	} else if content, ok := respBody["content"].(string); ok {
		anthropicResp["content"] = content
	} else {
		anthropicResp["content"] = ""
	}

	// usage（直接映射后端已返回的字段）
	if usage, ok := respBody["usage"].(map[string]interface{}); ok {
		inputTokens, _ := usage["input_tokens"].(float64)
		outputTokens, _ := usage["output_tokens"].(float64)
		totalTokens, ok := usage["total_tokens"].(float64)
		if !ok || totalTokens == 0 {
			totalTokens = inputTokens + outputTokens
		}
		anthropicResp["usage"] = map[string]interface{}{
			"input_tokens":  int(inputTokens),
			"output_tokens": int(outputTokens),
			"total_tokens":  int(totalTokens),
		}
	}

	jsonBody, err := json.Marshal(anthropicResp)
	if err != nil {
		return nil, err
	}

	return jsonBody, nil
}

// ConvertResponseWithModel 将后端响应转为 Anthropic 格式，使用指定的虚拟模型名
func (p *AnthropicProvider) ConvertResponseWithModel(resp *http.Response, virtualModel string) ([]byte, error) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var respBody map[string]interface{}
	if err := json.Unmarshal(body, &respBody); err != nil {
		return nil, err
	}

	// 构建 Anthropic 响应
	anthropicResp := map[string]interface{}{
		"id":    respBody["id"],
		"model": virtualModel, // 使用虚拟模型名替换后端返回的真实模型名
		"role":  "assistant",
		"type":  "message",
	}

	// finish_reason
	finishReason, _ := respBody["stop_reason"].(string)
	if finishReason == "" {
		finishReason, _ = respBody["finish_reason"].(string)
	}
	if finishReason == "" || finishReason == "stop" {
		finishReason = "end_turn"
	}
	anthropicResp["finish_reason"] = finishReason

	// content：过滤掉 proxy 特有字段（thinking, cache_control 等）
	if content, ok := respBody["content"].([]interface{}); ok && len(content) > 0 {
		filteredContent := make([]map[string]interface{}, 0, len(content))
		for _, block := range content {
			if blockMap, ok := block.(map[string]interface{}); ok {
				delete(blockMap, "thinking")
				delete(blockMap, "cache_control")
				filteredContent = append(filteredContent, blockMap)
			}
		}
		anthropicResp["content"] = filteredContent
	} else if content, ok := respBody["content"].(string); ok {
		anthropicResp["content"] = content
	} else {
		anthropicResp["content"] = ""
	}

	// usage：如果 total_tokens 为 null，从 input + output 计算
	if usage, ok := respBody["usage"].(map[string]interface{}); ok {
		inputTokens, _ := usage["input_tokens"].(float64)
		outputTokens, _ := usage["output_tokens"].(float64)
		totalTokens, ok := usage["total_tokens"].(float64)
		if !ok || totalTokens == 0 {
			totalTokens = inputTokens + outputTokens
		}
		anthropicResp["usage"] = map[string]interface{}{
			"input_tokens":  int(inputTokens),
			"output_tokens": int(outputTokens),
			"total_tokens":  int(totalTokens),
		}
	}

	jsonBody, err := json.Marshal(anthropicResp)
	if err != nil {
		return nil, err
	}

	return jsonBody, nil
}
