package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/sony/gobreaker"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/mapper"
	"llm-gateway/internal/metrics"
	"llm-gateway/internal/storage"
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
		apiKeyVal, _ := c.Get("api_key")
		apiKey, _ := apiKeyVal.(string)

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

		// 3. 路由选择 — 获取候选列表（支持 fallback 重试）
		sel, err := router.SelectCandidates(c.Request.Context(), req.Model, inputTokens)
		if err != nil {
			log.Error().Err(err).Msg("router selection failed")
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no available model"})
			return
		}

		if req.Stream {
			// 流式响应 — 遍历候选直到连接成功
			var upstream io.ReadCloser
			var targetProvider string
			var upstreamModel string
			var start time.Time

			for target := sel.Next(); target != nil; target = sel.Next() {
				upstreamModel = target.Model
				targetProvider = target.ProviderName
				if targetProvider == "" {
					targetProvider = providerName
				}

				start = time.Now()
				var execErr error

				if target.Breaker != nil {
					result, err := target.Breaker.Execute(func() (interface{}, error) {
						return target.Provider.StreamChat(c.Request.Context(), upstreamModel,
							toProviderMessages(req.Messages), toProviderTools(req.Tools))
					})
					if err != nil {
						if err == gobreaker.ErrOpenState || err == gobreaker.ErrTooManyRequests {
							log.Debug().Str("provider", targetProvider).Msg("breaker rejected, trying next")
							continue
						}
						execErr = err
					} else {
						upstream = result.(io.ReadCloser)
					}
				} else {
					upstream, execErr = target.Provider.StreamChat(c.Request.Context(), upstreamModel,
						toProviderMessages(req.Messages), toProviderTools(req.Tools))
				}

				if execErr != nil {
					log.Error().Err(execErr).Str("provider", targetProvider).Msg("stream connect failed, trying next")
					continue
				}
				break
			}

			if upstream == nil {
				log.Error().Msg("all upstream models failed for stream")
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "all upstream models failed"})
				return
			}
			defer upstream.Close()

			c.Header("Content-Type", "text/event-stream")
			c.Header("Cache-Control", "no-cache")
			c.Header("Connection", "keep-alive")

			result := streamHandler.RewriteAndForward(c.Writer, upstream, req.Model)

			// 记录延迟（用于 latency_optimized 策略）
			if targetProvider != "" {
				router.RecordLatency(req.Model, targetProvider, upstreamModel, float64(time.Since(start).Milliseconds()))
			}

			// 5. 流式：根据累计内容估算输出 token，异步记录用量
			estimatedOutput := tokenService.EstimateOutput(result.AccumulatedContent, realModel)
			toolCalls := streamHandler.ExtractToolCalls(result)
			estimatedToolCalls := len(toolCalls)

				// 优先使用从 SSE 提取的真实 token 数，没有则回退到本地估算值
				var realInput, realOutput, realTotal int
				if result.Usage != nil {
					realInput = result.Usage.PromptTokens
					realOutput = result.Usage.CompletionTokens
					realTotal = result.Usage.TotalTokens
				}
				// 记录用量：优先使用 upstream 返回的真实 token 数，没有则使用本地估算值
				effInput := realInput
				if effInput == 0 {
					effInput = inputTokens
				}
				effOutput := realOutput
				if effOutput == 0 {
					effOutput = estimatedOutput
				}
				effTotal := effInput + effOutput
				if realTotal > 0 {
					effTotal = realTotal
				}
				go tokenService.RecordUsageNow(reqID, realModel, req.Model, targetProvider,
					inputTokens, estimatedOutput, effInput, effOutput, effTotal, estimatedToolCalls, apiKey)

			metrics.RecordRequest("POST", "/v1/chat/completions", http.StatusOK, req.Model, time.Since(start).Seconds())
		} else {
			// 非流式响应 — 遍历候选直到请求成功
			var resp *http.Response
			var body []byte
			var targetProvider string
			var upstreamModel string
			var start time.Time

			for target := sel.Next(); target != nil; target = sel.Next() {
				upstreamModel = target.Model
				targetProvider = target.ProviderName
				if targetProvider == "" {
					targetProvider = providerName
				}

				start = time.Now()
				var execErr error

				if target.Breaker != nil {
					result, err := target.Breaker.Execute(func() (interface{}, error) {
						return target.Provider.Chat(c.Request.Context(), upstreamModel,
							toProviderMessages(req.Messages), toProviderTools(req.Tools))
					})
					if err != nil {
						if err == gobreaker.ErrOpenState || err == gobreaker.ErrTooManyRequests {
							log.Debug().Str("provider", targetProvider).Msg("breaker rejected, trying next")
							continue
						}
						execErr = err
					} else {
						resp = result.(*http.Response)
					}
				} else {
					resp, execErr = target.Provider.Chat(c.Request.Context(), upstreamModel,
						toProviderMessages(req.Messages), toProviderTools(req.Tools))
				}

				if execErr != nil {
					log.Error().Err(execErr).Str("provider", targetProvider).Msg("upstream request failed, trying next")
					continue
				}
				break
			}

			if resp == nil {
				log.Error().Msg("all upstream models failed for non-stream")
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "all upstream models failed"})
				return
			}
			defer resp.Body.Close()

			body, _ = io.ReadAll(resp.Body)

			// 记录延迟（用于 latency_optimized 策略）
			if targetProvider != "" {
				router.RecordLatency(req.Model, targetProvider, upstreamModel, float64(time.Since(start).Milliseconds()))
			}

			// 5. 解析上游返回的真实 usage，异步记录用量
			realInput, realOutput, realTotal := parseUsage(body)
			// 解析 tool_calls
			toolCalls := parseToolCalls(body, targetProvider)
			_ = realTotal // 保留用于未来的扩展
			tokenService.RecordUsageNow(reqID, realModel, req.Model, targetProvider,
				inputTokens, 0, realInput, realOutput, realTotal, len(toolCalls), apiKey)

			metrics.RecordRequest("POST", "/v1/chat/completions", resp.StatusCode, req.Model, time.Since(start).Seconds())

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
	Stream        bool                     `json:"stream,omitempty"`
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

// handleCountTokens 代理 /v1/messages/count_tokens 请求到上游 Anthropic 端点
func handleCountTokens(mapper *mapper.Service, router *router.Service, providerManager *provider.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		// 解析虚拟模型名，解析到真实模型名并注入请求体
		var parseReq struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &parseReq); err == nil && parseReq.Model != "" {
			// 模型未配置则直接 404
			if _, err := mapper.Resolve(parseReq.Model); err != nil {
				log.Warn().Str("model", parseReq.Model).Msg("count_tokens model not found in mapping")
				c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("model '%s' not found in mapping", parseReq.Model)})
				return
			}

			// 通过路由获取真实模型（支持 fallback chain）
			sel, err := router.SelectCandidates(c.Request.Context(), parseReq.Model, 0)
			if err != nil {
				log.Warn().Str("model", parseReq.Model).Msg("count_tokens router selection failed")
				c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("model '%s' has no available target", parseReq.Model)})
				return
			}
			target := sel.Next()
			if target == nil {
				c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("model '%s' has no available target", parseReq.Model)})
				return
			}

			// 将请求体中的 model 字段替换为真实模型名
			var reqMap map[string]interface{}
			if json.Unmarshal(body, &reqMap) == nil {
				reqMap["model"] = target.Model
				body, _ = json.Marshal(reqMap)
			}
		}

		// 调用上游的 /messages/count_tokens 端点
		p, ok := providerManager.Get("anthropic")
		if !ok {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "anthropic provider not available"})
			return
		}
		ap, ok := p.(*provider.AnthropicProvider)
		if !ok {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "provider is not an AnthropicProvider"})
			return
		}

		resp, err := ap.CountTokens(c.Request.Context(), body)
		if err != nil {
			log.Error().Err(err).Msg("count_tokens upstream request failed")
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		defer resp.Body.Close()

		respBody, _ := io.ReadAll(resp.Body)

		// 如果上游返回了 usage 字段，提取并返回 token 统计
		if resp.StatusCode == http.StatusOK {
			var usageData map[string]interface{}
			var parsedResp map[string]interface{}
			if json.Unmarshal(respBody, &parsedResp) == nil {
				if u, ok := parsedResp["usage"].(map[string]interface{}); ok {
					usageData = u
				}
			}
			if usageData != nil {
				c.JSON(http.StatusOK, gin.H{"usage": usageData})
				return
			}
		}

		// 没有 usage（上游可能返回 error），原样转发
		c.Data(resp.StatusCode, "application/json", respBody)
	}
}

func handleAnthropicMessages(
	mapper *mapper.Service,
	router *router.Service,
	streamHandler *stream.Handler,
	tokenService *token.Service,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		// /count_tokens 由独立的 handleCountTokens 处理，跳过此 handler 避免消费 body
		if c.Request.URL.Path == "/v1/messages/count_tokens" {
			c.Next()
			return
		}

		reqID := uuid.New().String()
		log := log.With().Str("request_id", reqID).Logger()
		apiKeyVal, _ := c.Get("api_key")
		apiKey, _ := apiKeyVal.(string)
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

		// 3. 路由选择 — 获取候选列表（支持 fallback 重试）
		sel, err := router.SelectCandidates(c.Request.Context(), req.Model, inputTokens)
		if err != nil {
			log.Error().Err(err).Msg("anthropic router selection failed")
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no available model"})
			return
		}

		// 4. 遍历候选，带断路器保护和 fallback 重试
		var resp *http.Response
		var targetProvider string
		var upstreamModel string
		var start time.Time
		var ap *provider.AnthropicProvider

		for target := sel.Next(); target != nil; target = sel.Next() {
			upstreamModel = target.Model
			targetProvider = target.ProviderName
			if targetProvider == "" {
				targetProvider = providerName
			}

			// 非 Anthropic provider 不支持 /messages 端点，跳过
			var ok bool
			ap, ok = target.Provider.(*provider.AnthropicProvider)
			if !ok {
				log.Debug().Str("provider", targetProvider).Msg("provider does not support /messages, skipping")
				continue
			}

			// 构建额外参数
			extraParams := map[string]interface{}{
				"max_tokens": func() int {
					if req.MaxTokens > 0 {
						return req.MaxTokens
					}
					return 4096
				}(),
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

			// 使用 target 的超时时间设置 context deadline
			reqCtx := c.Request.Context()
			if target.Timeout > 0 {
				var cancel context.CancelFunc
				reqCtx, cancel = context.WithTimeout(reqCtx, target.Timeout)
				defer cancel()
			}

			start = time.Now()
			var execErr error

			if target.Breaker != nil {
				result, err := target.Breaker.Execute(func() (interface{}, error) {
					return ap.SendDirect(
						reqCtx,
						realModel,
						req.Messages,
						req.System,
						extraParams,
						req.Stream,
					)
				})
				if err != nil {
					if err == gobreaker.ErrOpenState || err == gobreaker.ErrTooManyRequests {
						log.Debug().Str("provider", targetProvider).Msg("breaker rejected, trying next")
						continue
					}
					execErr = err
				} else {
					resp = result.(*http.Response)
				}
			} else {
				resp, execErr = ap.SendDirect(
					reqCtx,
					realModel,
					req.Messages,
					req.System,
					extraParams,
					req.Stream,
				)
			}

			if execErr != nil {
				log.Error().Err(execErr).Str("provider", targetProvider).Msg("anthropic upstream request failed, trying next")
				continue
			}
			break
		}

		if resp == nil {
			log.Error().Msg("all upstream models failed for anthropic /messages")
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "all upstream models failed"})
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

		if req.Stream {
			// 流式响应：转发 SSE，并替换 model 字段为虚拟模型名
			c.Header("Content-Type", "text/event-stream")
			c.Header("Cache-Control", "no-cache")
			c.Header("Connection", "keep-alive")

			// 记录延迟（在流开始后记录，而不是结束后）
			if targetProvider != "" {
				router.RecordLatency(req.Model, targetProvider, upstreamModel, float64(time.Since(start).Milliseconds()))
			}

			result := streamHandler.RewriteAndForward(c.Writer, resp.Body, req.Model)

			// 记录用量：优先使用从 SSE 提取的真实 token 数，没有则使用本地估算值
			var realInput, realOutput, realTotal int
			if result.Usage != nil {
				realInput = result.Usage.PromptTokens
				realOutput = result.Usage.CompletionTokens
				realTotal = result.Usage.TotalTokens
			}
			// 优先使用 upstream 返回的真实 token 数，没有则使用本地估算值
			effInput := realInput
			if effInput == 0 {
				effInput = inputTokens
			}
			effOutput := realOutput
			effTotal := effInput + effOutput
			if realTotal > 0 {
				effTotal = realTotal
			}
			go tokenService.RecordUsageNow(reqID, realModel, req.Model, targetProvider,
				inputTokens, 0, effInput, effOutput, effTotal, 0, apiKey)
		}
			// 流式已处理，直接返回（避免后续非流式代码重复读取 resp.Body）
			return

		// 记录延迟（非流式）
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

		// 提取用量并记录（同时支持 Anthropic 格式 input_tokens/output_tokens 和 OpenAI 格式 prompt_tokens/completion_tokens）
		var anthropicUsage struct {
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		realInput, realOutput := 0, 0
		if json.Unmarshal(anthropicResp, &anthropicUsage) == nil {
			realInput = anthropicUsage.Usage.InputTokens
			realOutput = anthropicUsage.Usage.OutputTokens
		}
		// 回退：如果 Anthropic 格式没有值，尝试 OpenAI 格式
		if realInput == 0 || realOutput == 0 {
			var openAIUsage struct {
				Usage struct {
					PromptTokens     int `json:"prompt_tokens"`
					CompletionTokens int `json:"completion_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(anthropicResp, &openAIUsage) == nil {
				if realInput == 0 {
					realInput = openAIUsage.Usage.PromptTokens
				}
				if realOutput == 0 {
					realOutput = openAIUsage.Usage.CompletionTokens
				}
			}
		}
		// 记录用量（无论解析是否成功，都写入存储层）
		tokenService.RecordUsageNow(reqID, realModel, req.Model, targetProvider,
			inputTokens, 0, realInput, realOutput, realInput+realOutput, 0, apiKey)

		metrics.RecordRequest("POST", "/v1/messages", resp.StatusCode, req.Model, time.Since(start).Seconds())
		c.Data(resp.StatusCode, "application/json", anthropicResp)
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
// 同时支持 OpenAI 格式（prompt_tokens/completion_tokens）和 Anthropic 格式（input_tokens/output_tokens）
func parseUsage(body []byte) (promptTokens, completionTokens, totalTokens int) {
	// 优先尝试 OpenAI 格式: {"usage":{"prompt_tokens":N,"completion_tokens":M,"total_tokens":T}}
	var resp ChatCompletionResponse
	if err := json.Unmarshal(body, &resp); err == nil {
		if resp.Usage.PromptTokens > 0 || resp.Usage.CompletionTokens > 0 {
			return resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens
		}
	}
	// 回退：尝试 Anthropic 格式 {"usage":{"input_tokens":N,"output_tokens":M}}
	var anthropicResp map[string]interface{}
	if err := json.Unmarshal(body, &anthropicResp); err == nil {
		if usage, ok := anthropicResp["usage"].(map[string]interface{}); ok {
			if v, ok := usage["input_tokens"]; ok {
				if f, ok := v.(float64); ok {
					promptTokens = int(f)
				}
			}
			if v, ok := usage["output_tokens"]; ok {
				if f, ok := v.(float64); ok {
					completionTokens = int(f)
				}
			}
			if v, ok := usage["total_tokens"]; ok {
				if f, ok := v.(float64); ok {
					totalTokens = int(f)
				}
			} else {
				totalTokens = promptTokens + completionTokens
			}
		}
	}
	return promptTokens, completionTokens, totalTokens
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

// ================= Token 用量查询 API =================

// handleUsageQuery 按 API Key 查询自己的 token 用量统计（聚合结果，不输出全部记录）
func handleUsageQuery(tokenService *token.Service, authService *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		apiKeyStr := c.Query("api_key")
		name := c.Query("name")
		model := c.Query("model")
		startTime := c.Query("start_time")
		endTime := c.Query("end_time")

		// 如果传了 name，先解析为实际 API Key
		if name != "" && apiKeyStr == "" {
			if info, ok := authService.FindKeyByName(name); ok {
				apiKeyStr = info.Key
			}
		}

		inputTokens, outputTokens, totalTokens, requestCount, err := tokenService.SumTokensByAPIKey(apiKeyStr, model, startTime, endTime)
		if err != nil {
			log.Error().Err(err).Msg("sum tokens failed")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"input_tokens":  inputTokens,
			"output_tokens": outputTokens,
			"total_tokens":  totalTokens,
			"request_count": requestCount,
		})
	}
}

// handleUsageByID 按 request_id 查询用量（只返回自己的）
func handleUsageByID(tokenService *token.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := c.Param("request_id")
		record, err := tokenService.QueryByRequestID(requestID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
			return
		}
		if record == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "record not found"})
			return
		}
		c.JSON(http.StatusOK, record)
	}
}

// handleUsageStats 按 API Key 查询自己的聚合统计
func handleUsageStats(tokenService *token.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		granularity := c.DefaultQuery("granularity", "daily")
		startTime := c.Query("start_time")
		endTime := c.Query("end_time")

		var summaries []storage.UsageSummary
		var err error
		switch granularity {
		case "daily":
			summaries, err = tokenService.AggregateDaily(startTime, endTime)
		case "weekly":
			summaries, err = tokenService.AggregateWeekly(startTime, endTime)
		case "monthly":
			summaries, err = tokenService.AggregateMonthly(startTime, endTime)
		default:
			summaries, err = tokenService.AggregateDaily(startTime, endTime)
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "aggregation failed"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": summaries, "granularity": granularity})
	}
}

// handleAdminUsage 管理员查询所有用量统计（聚合结果）
func handleAdminUsage(tokenService *token.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		startTime := c.Query("start_time")
		endTime := c.Query("end_time")

		inputTokens, outputTokens, totalTokens, requestCount, err := tokenService.SumTokensByTimeRange(startTime, endTime)
		if err != nil {
			log.Error().Err(err).Msg("sum tokens failed")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"input_tokens":  inputTokens,
			"output_tokens": outputTokens,
			"total_tokens":  totalTokens,
			"request_count": requestCount,
		})
	}
}

// handleAdminDailyUsage 管理端按日统计
func handleAdminDailyUsage(tokenService *token.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		startTime := c.Query("start_time")
		endTime := c.Query("end_time")

		summaries, err := tokenService.AdminDailyStats(startTime, endTime)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": summaries})
	}
}

// handleAdminStats 管理端总统计
func handleAdminStats(tokenService *token.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		startTime := c.Query("start_time")
		endTime := c.Query("end_time")

		stats, err := tokenService.AdminTotalStats(startTime, endTime)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": stats})
	}
}

// handleAdminCalibration 管理端校准信息
func handleAdminCalibration(tokenService *token.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		info := tokenService.CalibrationInfo()
		c.JSON(http.StatusOK, info)
	}
}
