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
	"llm-gateway/internal/protocol"
	"llm-gateway/internal/provider"
	"llm-gateway/internal/router"
	"llm-gateway/internal/storage"
	"llm-gateway/internal/stream"
	"llm-gateway/internal/token"
)

func handleChatCompletion(
	mapper *mapper.Service,
	routerSvc *router.Service,
	streamHandler *stream.Handler,
	tokenService *token.Service,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		reqID := uuid.New().String()
		log := log.With().Str("request_id", reqID).Logger()
		apiKeyVal, _ := c.Get("api_key")
		apiKey, _ := apiKeyVal.(string)

		var req protocol.ChatCompletionRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		// 1. 模型名 allowlist 校验
		if err := mapper.Validate(req.Model); err != nil {
			log.Warn().Str("model", req.Model).Msg("model not in allowlist")
			c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("model '%s' not found", req.Model)})
			return
		}
		log.Debug().Str("model", req.Model).Msg("model validated")

		// 2. 估算输入 token（加上 tools 定义的粗略开销）
		inputTokens := tokenService.EstimateInput(toTokenMessages(req.Messages), req.Model)
		if len(req.Tools) > 0 {
			inputTokens += len(req.Tools) * 80 // 每个 tool 定义约 80 tokens 开销
		}
		log.Debug().Int("input_tokens", inputTokens).Msg("token estimated")

		// 3. 路由选择 — 获取候选列表（支持 fallback 重试）
		sel, err := routerSvc.SelectCandidates(c.Request.Context(), inputTokens)
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

			var lastErr error
			for target := sel.Next(); target != nil; target = sel.Next() {
				upstreamModel = target.Model
				targetProvider = target.ProviderName

				start = time.Now()

				// 协议处理：统一调用 protocol.Resolve 处理 4 种协议组合
				// 通过 Breaker.Execute() 包裹，使熔断器可统计成功/失败并自动转换状态
				var res *protocol.Result
				var resolveErr error
				if target.Breaker != nil {
					result, breakerErr := target.Breaker.Execute(func() (interface{}, error) {
						return protocol.Resolve(protocol.Request{
							ClientProtocol: provider.ProtocolOpenAI,
							UpstreamTarget: target,
							ChatReq:        &req,
							IsStream:       true,
							Ctx:            c.Request.Context(),
							VirtualModel:   req.Model,
						})
					})
					if result != nil {
						res = result.(*protocol.Result)
					}
					resolveErr = breakerErr
				} else {
					res, resolveErr = protocol.Resolve(protocol.Request{
						ClientProtocol: provider.ProtocolOpenAI,
						UpstreamTarget: target,
						ChatReq:        &req,
						IsStream:       true,
						Ctx:            c.Request.Context(),
						VirtualModel:   req.Model,
					})
				}
				if resolveErr != nil {
					if resolveErr == gobreaker.ErrOpenState || resolveErr == gobreaker.ErrTooManyRequests {
						log.Debug().Str("provider", targetProvider).Msg("breaker rejected, trying next")
						continue
					}
					if lastErr == nil {
						lastErr = resolveErr
					}
					log.Error().Err(resolveErr).Str("provider", targetProvider).Msg("stream connect failed, trying next")
					continue
				}
				upstream = res.StreamBody
				break
			}

			if upstream == nil {
				if lastErr != nil {
					log.Error().Err(lastErr).Msg("all upstream models failed for stream")
					c.JSON(http.StatusServiceUnavailable, gin.H{"error": lastErr.Error()})
				} else {
					log.Error().Msg("all upstream models failed for stream")
					c.JSON(http.StatusServiceUnavailable, gin.H{"error": "all upstream models failed"})
				}
				return
			}

			defer upstream.Close()

			c.Header("Content-Type", "text/event-stream")
			c.Header("Cache-Control", "no-cache")
			c.Header("Connection", "keep-alive")

			result := streamHandler.RewriteAndForward(c.Writer, upstream, req.Model)

			// 记录延迟（用于 latency_optimized 策略）
			if targetProvider != "" {
				routerSvc.RecordLatency(targetProvider, upstreamModel, float64(time.Since(start).Milliseconds()))
			}

			// 5. 流式：根据累计内容估算输出 token，异步记录用量
			estimatedOutput := tokenService.EstimateOutput(result.AccumulatedContent, req.Model)
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
			go tokenService.RecordUsageNow(reqID, upstreamModel, req.Model, targetProvider,
				inputTokens, estimatedOutput, effInput, effOutput, effTotal, estimatedToolCalls, apiKey)

			metrics.RecordRequest("POST", "/v1/chat/completions", http.StatusOK, req.Model, time.Since(start).Seconds())
		} else {
			// 非流式响应 — 遍历候选直到请求成功
			var targetProvider string
			var upstreamModel string
			var start time.Time
			var lastErr error

			for target := sel.Next(); target != nil; target = sel.Next() {
				upstreamModel = target.Model
				targetProvider = target.ProviderName

				start = time.Now()

				// 协议处理：统一调用 protocol.Resolve 处理 4 种协议组合
				// 通过 Breaker.Execute() 包裹，使熔断器可统计成功/失败并自动转换状态
				var res *protocol.Result
				var resolveErr error
				if target.Breaker != nil {
					result, breakerErr := target.Breaker.Execute(func() (interface{}, error) {
						return protocol.Resolve(protocol.Request{
							ClientProtocol: provider.ProtocolOpenAI,
							UpstreamTarget: target,
							ChatReq:        &req,
							IsStream:       false,
							Ctx:            c.Request.Context(),
							VirtualModel:   req.Model,
						})
					})
					if result != nil {
						res = result.(*protocol.Result)
					}
					resolveErr = breakerErr
				} else {
					res, resolveErr = protocol.Resolve(protocol.Request{
						ClientProtocol: provider.ProtocolOpenAI,
						UpstreamTarget: target,
						ChatReq:        &req,
						IsStream:       false,
						Ctx:            c.Request.Context(),
						VirtualModel:   req.Model,
					})
				}
				if resolveErr != nil {
					if resolveErr == gobreaker.ErrOpenState || resolveErr == gobreaker.ErrTooManyRequests {
						log.Debug().Str("provider", targetProvider).Msg("breaker rejected, trying next")
						continue
					}
					if lastErr == nil {
						lastErr = resolveErr
					}
					log.Error().Err(resolveErr).Str("provider", targetProvider).Msg("upstream request failed, trying next")
					continue
				}
				// Body 已在 protocol.Resolve 中读取并转换，直接使用
				if res.Response != nil {
					defer res.Response.Body.Close()
				}

				body := res.Body

				// 记录延迟（用于 latency_optimized 策略）
				if targetProvider != "" {
					routerSvc.RecordLatency(targetProvider, upstreamModel, float64(time.Since(start).Milliseconds()))
				}

				// 5. 解析上游返回的真实 usage，异步记录用量
				realInput, realOutput, realTotal := parseUsage(body)
				// 解析 tool_calls
				toolCalls := parseToolCalls(body, targetProvider)
				_ = realTotal // 保留用于未来的扩展
				go tokenService.RecordUsageNow(reqID, upstreamModel, req.Model, targetProvider,
					inputTokens, 0, realInput, realOutput, realTotal, len(toolCalls), apiKey)

				metrics.RecordRequest("POST", "/v1/chat/completions", res.StatusCode, req.Model, time.Since(start).Seconds())

				// 重写响应中的 model 字段
				body = mapper.RewriteResponse(body, req.Model)

				// 6. 将 Anthropic tool_use 转换为 OpenAI tool_calls 格式
				body = rewriteAnthropicToolCalls(body, targetProvider)

				c.Data(res.StatusCode, "application/json", body)
				return
			}

			if lastErr != nil {
				log.Error().Err(lastErr).Msg("all upstream models failed for non-stream")
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": lastErr.Error()})
			} else {
				log.Error().Msg("all upstream models failed for non-stream")
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "all upstream models failed"})
			}
			return
		}
	}
}

// detectClientProtocol 根据请求 URL 路径和 HTTP 头部判断客户端使用的协议类型。
// OpenAI 特征：URL 路径包含 /chat/completions 或 /completions。
// Anthropic 特征：URL 路径包含 /messages，且头部包含 anthropic-version 或 x-api-key。
func detectClientProtocol(c *gin.Context) provider.ClientProtocol {
	path := c.Request.URL.Path
	if path == "/v1/messages" || path == "/messages" {
		return provider.ProtocolAnthropic
	}
	if path == "/v1/chat/completions" || path == "/chat/completions" {
		return provider.ProtocolOpenAI
	}
	if path == "/v1/completions" || path == "/completions" {
		return provider.ProtocolOpenAI
	}
	// 通过头部做兜底判断
	if c.GetHeader("anthropic-version") != "" || c.GetHeader("x-api-key") != "" {
		return provider.ProtocolAnthropic
	}
	return provider.ProtocolOpenAI
}

func handleCompletion(
	mapper *mapper.Service,
	routerSvc *router.Service,
	streamHandler *stream.Handler,
	tokenService *token.Service,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "not implemented"})
	}
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
func handleCountTokens(mapper *mapper.Service, routerSvc *router.Service, providerManager *provider.Manager) gin.HandlerFunc {
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
			if err := mapper.Validate(parseReq.Model); err != nil {
				log.Warn().Str("model", parseReq.Model).Msg("count_tokens model not found in mapping")
				c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("model '%s' not found in mapping", parseReq.Model)})
				return
			}

			// 通过路由获取真实模型（支持 fallback chain）
			sel, err := routerSvc.SelectCandidates(c.Request.Context(), 0)
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
		resp, err := p.CountTokens(c.Request.Context(), body)
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
	routerSvc *router.Service,
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

		var req protocol.AnthropicRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		// 1. 模型名 allowlist 校验
		if err := mapper.Validate(req.Model); err != nil {
			log.Warn().Str("model", req.Model).Msg("model not in allowlist")
			c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("model '%s' not found", req.Model)})
			return
		}
		log.Debug().Str("model", req.Model).Msg("anthropic model validated")

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
		sel, err := routerSvc.SelectCandidates(c.Request.Context(), inputTokens)
		if err != nil {
			log.Error().Err(err).Msg("anthropic router selection failed")
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no available model"})
			return
		}

		// 4. 客户端协议检测
		clientProtocol := detectClientProtocol(c)
		log.Debug().Str("client_protocol", string(clientProtocol)).Msg("anthropic /messages request")

		// 4. 遍历候选，带断路器保护和 fallback 重试
		var protocolResult *protocol.Result
		var targetProvider string
		var upstreamModel string
		var start time.Time
		var lastErr error
		for target := sel.Next(); target != nil; target = sel.Next() {
			upstreamModel = target.Model
			targetProvider = target.ProviderName

			// 构建额外参数（Case 4 专用）
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
			var cancel context.CancelFunc
			if target.Timeout > 0 {
				reqCtx, cancel = context.WithTimeout(reqCtx, target.Timeout)
				defer cancel()
			}

			start = time.Now()

			// 协议处理：统一调用 protocol.Resolve 处理 4 种协议组合
			// 通过 Breaker.Execute() 包裹，使熔断器可统计成功/失败并自动转换状态
			var res *protocol.Result
			var resolveErr error
			if target.Breaker != nil {
				result, breakerErr := target.Breaker.Execute(func() (interface{}, error) {
					return protocol.Resolve(protocol.Request{
						ClientProtocol: clientProtocol,
						UpstreamTarget: target,
						AnthropicReq:   &req,
						ExtraParams:    extraParams,
						IsStream:       req.Stream,
						Ctx:            reqCtx,
						VirtualModel:   req.Model,
					})
				})
				if result != nil {
					res = result.(*protocol.Result)
				}
				resolveErr = breakerErr
			} else {
				res, resolveErr = protocol.Resolve(protocol.Request{
					ClientProtocol: clientProtocol,
					UpstreamTarget: target,
					AnthropicReq:   &req,
					ExtraParams:    extraParams,
					IsStream:       req.Stream,
					Ctx:            reqCtx,
					VirtualModel:   req.Model,
				})
			}
			if resolveErr != nil {
				if resolveErr == gobreaker.ErrOpenState || resolveErr == gobreaker.ErrTooManyRequests {
					log.Debug().Str("provider", targetProvider).Msg("breaker rejected, trying next")
					continue
				}
				if lastErr == nil {
					lastErr = resolveErr
				}
				log.Error().Err(resolveErr).Str("provider", targetProvider).Msg("anthropic upstream request failed, trying next")
				continue
			}
			protocolResult = res
			break
		}

		if protocolResult == nil {
			if lastErr != nil {
				log.Error().Err(lastErr).Msg("all upstream models failed for anthropic /messages")
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": lastErr.Error()})
			} else {
				log.Error().Msg("all upstream models failed for anthropic /messages")
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "all upstream models failed"})
			}
			return
		}

		// 上游返回错误状态码时，直接转发错误 body
		// （仅当 Response 可用时 — Case 4 流式/非流式及 Cases 1-3 非流式）
		if protocolResult.Response != nil && protocolResult.StatusCode >= 400 {
			body := protocolResult.Body
			if len(body) == 0 {
				body, _ = io.ReadAll(protocolResult.Response.Body)
			}
			protocolResult.Response.Body.Close()
			log.Error().Int("status", protocolResult.StatusCode).RawJSON("body", body).Msg("anthropic upstream returned error")
			c.Data(protocolResult.StatusCode, "application/json", body)
			return
		}

		if req.Stream {
			// 流式响应：使用 StreamBody（可能已包装 SSE converter）
			if protocolResult.StreamBody == nil {
				log.Error().Msg("stream body is nil after successful resolve")
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "upstream returned empty stream"})
				return
			}
			defer protocolResult.StreamBody.Close()

			c.Header("Content-Type", "text/event-stream")
			c.Header("Cache-Control", "no-cache")
			c.Header("Connection", "keep-alive")

			// 记录延迟（在流开始后记录，而不是结束后）
			if targetProvider != "" {
				routerSvc.RecordLatency(targetProvider, upstreamModel, float64(time.Since(start).Milliseconds()))
			}

			result := streamHandler.RewriteAndForward(c.Writer, protocolResult.StreamBody, req.Model)

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
			go tokenService.RecordUsageNow(reqID, upstreamModel, req.Model, targetProvider,
				inputTokens, 0, effInput, effOutput, effTotal, 0, apiKey)

			metrics.RecordRequest("POST", "/v1/messages", protocolResult.StatusCode, req.Model, time.Since(start).Seconds())
		} else {
			// 非流式响应：使用 Body（已在 Resolve 中读取并转换）
			if protocolResult.Response != nil {
				defer protocolResult.Response.Body.Close()
			}

			// 记录延迟
			if targetProvider != "" {
				routerSvc.RecordLatency(targetProvider, upstreamModel, float64(time.Since(start).Milliseconds()))
			}

			// 记录用量
			realInput, realOutput, realTotal := parseUsage(protocolResult.Body)
			toolCalls := parseToolCalls(protocolResult.Body, targetProvider)
			_ = realTotal
			go tokenService.RecordUsageNow(reqID, upstreamModel, req.Model, targetProvider,
				inputTokens, 0, realInput, realOutput, realTotal, len(toolCalls), apiKey)

			metrics.RecordRequest("POST", "/v1/messages", protocolResult.StatusCode, req.Model, time.Since(start).Seconds())

			c.Data(protocolResult.StatusCode, "application/json", protocolResult.Body)
		}
	}
}

func toProviderMessages(msgs []protocol.Message) []provider.Message {
	out := make([]provider.Message, len(msgs))
	for i, m := range msgs {
		out[i] = provider.Message{Role: m.Role, Content: m.Content}
	}
	return out
}

func toTokenMessages(msgs []protocol.Message) []token.Message {
	out := make([]token.Message, len(msgs))
	for i, m := range msgs {
		out[i] = token.Message{Role: m.Role, Content: m.Content}
	}
	return out
}

// toProviderMessagesFromMap 将 Anthropic 格式的消息列表（[]map[string]interface{}）转为 []provider.Message
// content blocks 会被展平为字符串
func toProviderMessagesFromMap(msgs []map[string]interface{}) []provider.Message {
	var out []provider.Message
	for _, msg := range msgs {
		role, _ := msg["role"].(string)
		if role == "" {
			continue
		}
		content := ""
		switch v := msg["content"].(type) {
		case string:
			content = v
		case []interface{}:
			for _, block := range v {
				if b, ok := block.(map[string]interface{}); ok {
					if b["type"] == "text" {
						if t, ok := b["text"].(string); ok {
							content += t
						}
					}
				}
			}
		}
		out = append(out, provider.Message{Role: role, Content: content})
	}
	return out
}

// parseUsage 从 OpenAI 兼容响应 JSON 中提取 usage 字段
// 同时支持 OpenAI 格式（prompt_tokens/completion_tokens）和 Anthropic 格式（input_tokens/output_tokens）
func parseUsage(body []byte) (promptTokens, completionTokens, totalTokens int) {
	// 优先尝试 OpenAI 格式: {"usage":{"prompt_tokens":N,"completion_tokens":M,"total_tokens":T}}
	var resp protocol.ChatCompletionResponse
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
func toProviderTools(tools []protocol.Tool) []provider.Tool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]provider.Tool, len(tools))
	for i, t := range tools {
		out[i] = provider.Tool{
			Type:     t.Type,
			Function: provider.ToolFunc(t.Function),
		}
	}
	return out
}

// toolsFromAnthropicRequest 将 Anthropic 请求中的 tools（[]map[string]interface{}）转为 Provider 格式的 Tool。
// 支持 OpenAI 格式（有 "function" 字段）和 Anthropic 格式（name 直接在顶层）的 tools。
func toolsFromAnthropicRequest(reqTools []map[string]interface{}) []provider.Tool {
	if len(reqTools) == 0 {
		return nil
	}
	out := make([]provider.Tool, 0, len(reqTools))
	for _, t := range reqTools {
		// 尝试 OpenAI 格式：{"type":"function","function":{"name":"...","description":"...","parameters":{}}}
		if fnRaw, ok := t["function"]; ok {
			if fn, ok := fnRaw.(map[string]interface{}); ok {
				name, _ := fn["name"].(string)
				desc, _ := fn["description"].(string)
				var params any
				if p, ok := fn["parameters"]; ok {
					params = p
				}
				out = append(out, provider.Tool{
					Type: func() string { if tt, ok := t["type"].(string); ok { return tt }; return "function" }(),
					Function: provider.ToolFunc{
						Name:        name,
						Description: desc,
						Parameters:  params,
					},
				})
				continue
			}
		}
		// 尝试 Anthropic 格式：{"name":"...","description":"...","input_schema":{}}
		if name, ok := t["name"].(string); ok {
			desc, _ := t["description"].(string)
			var params any
			if p, ok := t["input_schema"]; ok {
				params = p
			}
			out = append(out, provider.Tool{
				Type: func() string { if tt, ok := t["type"].(string); ok { return tt }; return "function" }(),
				Function: provider.ToolFunc{
					Name:        name,
					Description: desc,
					Parameters:  params,
				},
			})
		}
	}
	return out
}

// parseToolCalls 从响应中解析 tool_calls
func parseToolCalls(body []byte, providerName string) []protocol.ChatCompletionResponse {
	var results []protocol.ChatCompletionResponse

	// 尝试 OpenAI 格式
	var openAIResp protocol.ChatCompletionResponse
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
				toolCall := protocol.ToolCall{
					ID:   id,
					Type: "function",
					Function: protocol.ToolCallFunc{
						Name:      name,
						Arguments: inputStr,
					},
				}
				results = append(results, protocol.ChatCompletionResponse{
					Choices: []protocol.Choice{{
						Message: protocol.Message{
							ToolCalls: []protocol.ToolCall{toolCall},
						},
						FinishReason: "tool_calls",
					}},
				})
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
				"index":         0,
				"finish_reason": "tool_calls",
				"message": map[string]interface{}{
					"content":    "",
					"role":       "assistant",
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
