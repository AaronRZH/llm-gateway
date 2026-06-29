package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"llm-gateway/internal/mapper"
	"llm-gateway/internal/provider"
	"llm-gateway/internal/router"
	"llm-gateway/internal/stream"
	"llm-gateway/internal/token"
)

// ChatCompletionRequest OpenAI 兼容请求格式
type ChatCompletionRequest struct {
	Model       string                  `json:"model" binding:"required"`
	Messages    []Message               `json:"messages" binding:"required"`
	Stream      bool                    `json:"stream,omitempty"`
	MaxTokens   int                     `json:"max_tokens,omitempty"`
	Temperature float64                 `json:"temperature,omitempty"`
	TopP        float64                 `json:"top_p,omitempty"`
	Tools       []Tool                  `json:"tools,omitempty"`
}

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

type Tool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Parameters  any    `json:"parameters,omitempty"`
	} `json:"function"`
}

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ChatCompletionResponse OpenAI 兼容响应格式
type ChatCompletionResponse struct {
	ID         string   `json:"id"`
	Object     string   `json:"object"`
	Created    int64    `json:"created"`
	Model      string   `json:"model"`
	Choices    []Choice `json:"choices"`
	Usage      Usage    `json:"usage"`
	ToolCalls  []Choice `json:"tool_calls,omitempty"` // 兼容 Anthropic 响应
}

type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func handleChatCompletion(
	mapper *mapper.Service,
	router *router.Service,
	streamHandler *stream.Handler,
	tokenService *token.Service,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		reqID := uuid.New().String()
		log := log.With().Str("request_id", reqID).Logger()

		var req ChatCompletionRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		// 1. 模型名映射: virtual -> real
		mapped, err := mapper.Resolve(req.Model)
		realModel := req.Model
		providerName := ""
		if err != nil {
			log.Warn().Str("model", req.Model).Msg("model not found in mapping")
		} else {
			realModel = mapped.RealModel
			providerName = mapped.Provider
		}
		log.Debug().Str("virtual", req.Model).Str("real", realModel).Str("provider", providerName).Msg("model mapped")

		// 2. 估算输入 token（加上 tools 定义的粗略开销）
		inputTokens := tokenService.EstimateInput(toTokenMessages(req.Messages), realModel)
		if len(req.Tools) > 0 {
			inputTokens += len(req.Tools) * 80 // 每个 tool 定义约 80 tokens 开销
		}
		log.Debug().Int("input_tokens", inputTokens).Msg("token estimated")

		// 3. 路由选择（如果配置了模型组）
		target, err := router.Select(c.Request.Context(), req.Model, inputTokens)
		if err != nil {
			log.Error().Err(err).Msg("router selection failed")
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no available model"})
			return
		}

		// 4. 请求上游
		upstreamModel := target.Model
		targetProvider := target.ProviderName
		if targetProvider == "" {
			targetProvider = providerName
		}

		if req.Stream {
			// 流式响应
			c.Header("Content-Type", "text/event-stream")
			c.Header("Cache-Control", "no-cache")
			c.Header("Connection", "keep-alive")

			start := time.Now()
			upstream, err := target.Provider.StreamChat(c.Request.Context(), upstreamModel, toProviderMessages(req.Messages), toProviderTools(req.Tools))
			if err != nil {
				log.Error().Err(err).Msg("upstream stream failed")
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			defer upstream.Close()

			result := streamHandler.RewriteAndForward(c.Writer, upstream, req.Model)

			// 记录延迟（用于 latency_optimized 策略）
			if targetProvider != "" {
				router.RecordLatency(req.Model, targetProvider, upstreamModel, float64(time.Since(start).Milliseconds()))
			}

			// 5. 流式：根据累计内容估算输出 token，异步记录用量
			estimatedOutput := tokenService.EstimateOutput(result.AccumulatedContent, realModel)
			toolCalls := streamHandler.ExtractToolCalls(result)
			estimatedToolCalls := len(toolCalls)
			go tokenService.RecordUsage(reqID, realModel, req.Model, targetProvider,
				inputTokens, estimatedOutput, 0, 0, 0, estimatedToolCalls)
		} else {
			// 非流式响应
			start := time.Now()
			resp, err := target.Provider.Chat(c.Request.Context(), upstreamModel, toProviderMessages(req.Messages), toProviderTools(req.Tools))
			if err != nil {
				log.Error().Err(err).Msg("upstream request failed")
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			defer resp.Body.Close()

			body, _ := io.ReadAll(resp.Body)

			// 记录延迟（用于 latency_optimized 策略）
			if targetProvider != "" {
				router.RecordLatency(req.Model, targetProvider, upstreamModel, float64(time.Since(start).Milliseconds()))
			}

			// 5. 解析上游返回的真实 usage，异步记录用量
			realInput, realOutput, realTotal := parseUsage(body)
			// 解析 tool_calls
			toolCalls := parseToolCalls(body, targetProvider)
			_ = realTotal // 保留用于未来的扩展
			go tokenService.RecordUsage(reqID, realModel, req.Model, targetProvider,
				inputTokens, 0, realInput, realOutput, realTotal, len(toolCalls))

			// 重写响应中的 model 字段
			body = mapper.RewriteResponse(body, req.Model)

			// 6. 将 Anthropic tool_use 转换为 OpenAI tool_calls 格式
			body = rewriteAnthropicToolCalls(body, targetProvider)

			c.Data(resp.StatusCode, "application/json", body)
		}
	}
}

func handleCompletion(
	mapper *mapper.Service,
	router *router.Service,
	streamHandler *stream.Handler,
	tokenService *token.Service,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "not implemented"})
	}
}

// AnthropicRequest 网关接收的 Anthropic Messages API 请求格式
type AnthropicRequest struct {
	Model         string                   `json:"model"`
	Messages      []map[string]interface{} `json:"messages"`
	System        interface{}              `json:"system,omitempty"`
	MaxTokens     int                      `json:"max_tokens"`
	Temperature   float64                  `json:"temperature,omitempty"`
	TopP          float64                  `json:"top_p,omitempty"`
	StopSequences []string                 `json:"stop_sequences,omitempty"`
	Tools         []map[string]interface{} `json:"tools,omitempty"`
	ToolChoice    map[string]interface{}   `json:"tool_choice,omitempty"`
}

func handleListModels(mapper *mapper.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		models := mapper.ListVirtualModels()
		c.JSON(http.StatusOK, gin.H{
			"object": "list",
			"data":   models,
		})
	}
}

func handleAnthropicMessages(
	mapper *mapper.Service,
	router *router.Service,
	streamHandler *stream.Handler,
	tokenService *token.Service,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		reqID := uuid.New().String()
		log := log.With().Str("request_id", reqID).Logger()
		log.Info().Msg("anthropic /messages request")

		var req AnthropicRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		// 1. 模型名映射: virtual -> real
		mapped, err := mapper.Resolve(req.Model)
		realModel := req.Model
		providerName := ""
		if err != nil {
			log.Warn().Str("model", req.Model).Msg("model not found in mapping")
		} else {
			realModel = mapped.RealModel
			providerName = mapped.Provider
		}
		log.Debug().Str("virtual", req.Model).Str("real", realModel).Str("provider", providerName).Msg("anthropic model mapped")

		// 2. 估算输入 token
		var inputTokens int
		for _, msg := range req.Messages {
			if content, ok := msg["content"].(string); ok {
				inputTokens += len([]rune(content)) / 4
			}
			if tools, ok := msg["tool_calls"].([]interface{}); ok {
				inputTokens += len(tools) * 20
			}
		}
		if len(req.Tools) > 0 {
			inputTokens += len(req.Tools) * 80
		}
		log.Debug().Int("input_tokens", inputTokens).Msg("anthropic token estimated")

		// 3. 路由选择
		target, err := router.Select(c.Request.Context(), req.Model, inputTokens)
		if err != nil {
			log.Error().Err(err).Msg("anthropic router selection failed")
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no available model"})
			return
		}

		// 4. 调用 AnthropicProvider 的 ChatWithRequest（已包含格式转换）
		start := time.Now()
		upstreamModel := req.Model
		targetProvider := target.ProviderName
		if targetProvider == "" {
			targetProvider = providerName
		}

		// 判断是否是 Anthropic 类型的 provider
		if ap, ok := target.Provider.(*provider.AnthropicProvider); ok {
			// 上游为 Anthropic 时，直接使用原始 Anthropic 格式消息发送，跳过格式转换
			extraParams := map[string]interface{}{
				"max_tokens": func() int { if req.MaxTokens > 0 { return req.MaxTokens }; return 4096 }(),
			}
			if req.Temperature > 0 {
				extraParams["temperature"] = req.Temperature
			}
			if req.TopP > 0 {
				extraParams["top_p"] = req.TopP
			}
			if len(req.StopSequences) > 0 {
				extraParams["stop_sequences"] = req.StopSequences
			}
			if len(req.Tools) > 0 {
				extraParams["tools"] = req.Tools
			}
			if req.ToolChoice != nil {
				extraParams["tool_choice"] = req.ToolChoice
			}

			// 使用 target 的超时时间设置 context deadline（优先于 provider 默认超时）
			reqCtx := c.Request.Context()
			if target.Timeout > 0 {
				var cancel context.CancelFunc
				reqCtx, cancel = context.WithTimeout(reqCtx, target.Timeout)
				defer cancel()
			}

			resp, err := ap.SendDirect(
				reqCtx,
				realModel,
				req.Messages,
				req.System,
				extraParams,
				false, // /messages 端点当前不支持流式
			)
			if err != nil {
				log.Error().Err(err).Msg("anthropic upstream request failed")
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			defer resp.Body.Close()

			// 上游返回错误状态码时，直接转发错误 body
			if resp.StatusCode >= 400 {
				body, _ := io.ReadAll(resp.Body)
				log.Error().Int("status", resp.StatusCode).RawJSON("body", body).Msg("anthropic upstream returned error")
				c.Data(resp.StatusCode, "application/json", body)
				return
			}

			// 记录延迟
			if targetProvider != "" {
				router.RecordLatency(req.Model, targetProvider, upstreamModel, float64(time.Since(start).Milliseconds()))
			}

			// 5. 将后端响应转为 Anthropic 格式（使用虚拟模型名替换 real 模型名）
			anthropicResp, err := ap.ConvertResponseWithModel(resp, req.Model)
			if err != nil {
				log.Error().Err(err).Msg("anthropic response conversion failed")
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}

			c.Data(resp.StatusCode, "application/json", anthropicResp)
			return
		}

		// 非 Anthropic provider 不支持 /messages 端点
		log.Warn().Str("provider", targetProvider).Msg("provider does not support /messages endpoint, use /chat/completions instead")
		c.JSON(http.StatusNotImplemented, gin.H{"error": "/messages endpoint only supports Anthropic provider"})
	}
}

func toProviderMessages(msgs []Message) []provider.Message {
	out := make([]provider.Message, len(msgs))
	for i, m := range msgs {
		out[i] = provider.Message{Role: m.Role, Content: m.Content}
	}
	return out
}

func toTokenMessages(msgs []Message) []token.Message {
	out := make([]token.Message, len(msgs))
	for i, m := range msgs {
		out[i] = token.Message{Role: m.Role, Content: m.Content}
	}
	return out
}

// parseUsage 从 OpenAI 兼容响应 JSON 中提取 usage 字段
func parseUsage(body []byte) (promptTokens, completionTokens, totalTokens int) {
	var resp ChatCompletionResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, 0, 0
	}
	return resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens
}

// toProviderTools 将网关的 Tool 转为 Provider 格式的 Tool
func toProviderTools(tools []Tool) []provider.Tool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]provider.Tool, len(tools))
	for i, t := range tools {
		out[i] = provider.Tool{
			Type: t.Type,
			Function: provider.ToolFunc{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  t.Function.Parameters,
			},
		}
	}
	return out
}

// parseToolCalls 从响应中解析 tool_calls
func parseToolCalls(body []byte, providerName string) []ChatCompletionResponse {
	var results []ChatCompletionResponse

	// 尝试 OpenAI 格式
	var openAIResp ChatCompletionResponse
	if err := json.Unmarshal(body, &openAIResp); err == nil {
		for _, choice := range openAIResp.Choices {
			if len(choice.Message.ToolCalls) > 0 {
				results = append(results, openAIResp)
			}
		}
	}

	// 尝试 Anthropic 格式（tool_use）
	var anthropicResp map[string]interface{}
	if err := json.Unmarshal(body, &anthropicResp); err == nil {
		if content, ok := anthropicResp["content"].([]interface{}); ok {
			for _, item := range content {
				block, ok := item.(map[string]interface{})
				if !ok || block["type"] != "tool_use" {
					continue
				}
				// 转换为 OpenAI tool_call 格式
				name, _ := block["name"].(string)
				inputBytes, _ := json.Marshal(block["input"])
				inputStr := string(inputBytes)
				id, _ := block["id"].(string)
				if id == "" {
					id = "call_" + uuid.New().String()[:8]
				}
				toolCall := ChatCompletionResponse{
					Choices: []Choice{{
						Message: Message{
							ToolCalls: []ToolCall{{
								ID:   id,
								Type: "function",
								Function: struct {
									Name      string `json:"name"`
									Arguments string `json:"arguments"`
								}{
									Name:      name,
									Arguments: inputStr,
								},
							}},
						},
						FinishReason: "tool_calls",
					}},
				}
				results = append(results, toolCall)
			}
		}
	}

	return results
}

// rewriteAnthropicToolCalls 将 Anthropic tool_use 响应转换为 OpenAI tool_calls 格式
func rewriteAnthropicToolCalls(body []byte, providerName string) []byte {
	if providerName != "anthropic" {
		return body
	}

	var anthropicResp map[string]interface{}
	if err := json.Unmarshal(body, &anthropicResp); err != nil {
		return body
	}

	content, ok := anthropicResp["content"].([]interface{})
	if !ok {
		return body
	}

	var toolCalls []map[string]interface{}
	found := false
	for _, item := range content {
		block, ok := item.(map[string]interface{})
		if !ok || block["type"] != "tool_use" {
			continue
		}
		found = true
		id, _ := block["id"].(string)
		if id == "" {
			id = "call_" + uuid.New().String()[:8]
		}
		name, _ := block["name"].(string)
		inputBytes, _ := json.Marshal(block["input"])
		inputStr := string(inputBytes)
		toolCalls = append(toolCalls, map[string]interface{}{
			"id":   id,
			"type": "function",
			"function": map[string]interface{}{
				"name":      name,
				"arguments": inputStr,
			},
		})
	}

	if !found || len(toolCalls) == 0 {
		return body
	}

	// 提取各字段，缺省则用默认值
	respID, _ := anthropicResp["id"].(string)
	modelName, _ := anthropicResp["model"].(string)
	usage, _ := anthropicResp["usage"]

	// 尝试 timestamp，fallback 到当前时间
	var created int64 = 0
	if ts, ok := anthropicResp["timestamp"].(float64); ok {
		created = int64(ts)
	} else if ts2, ok := anthropicResp["created"].(float64); ok {
		created = int64(ts2)
	}

	// 替换 response 格式为 OpenAI 风格
	openAIResp := map[string]interface{}{
		"id":      respID,
		"object":  "chat.completion",
		"created": created,
		"model":   modelName,
		"choices": []map[string]interface{}{
			{
				"index":        0,
				"finish_reason": "tool_calls",
				"message": map[string]interface{}{
					"content":   "",
					"role":      "assistant",
					"tool_calls": toolCalls,
				},
			},
		},
		"usage": usage,
	}

	result, _ := json.Marshal(openAIResp)
	return result
}
