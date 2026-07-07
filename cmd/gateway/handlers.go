package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/sony/gobreaker"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/config"
	"llm-gateway/internal/mapper"
	"llm-gateway/internal/middleware"
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
		sel, err := routerSvc.SelectCandidates(c.Request.Context(), req.Model, inputTokens)
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
				// 429 退避：不触发熔断，退避后继续 fallback 尝试下一个候选
				if res.StatusCode == 429 {
					backoff := parseRetryAfter(res.Body, 5*time.Second)
					log.Warn().Dur("backoff", backoff).Str("provider", targetProvider).Msg("rate limited (429), backing off")
					time.Sleep(backoff)
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
				// 429 退避：不触发熔断，退避后继续 fallback 尝试下一个候选
				if res.StatusCode == 429 {
					backoff := parseRetryAfter(res.Body, 5*time.Second)
					log.Warn().Dur("backoff", backoff).Str("provider", targetProvider).Msg("rate limited (429), backing off")
					time.Sleep(backoff)
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
			sel, err := routerSvc.SelectCandidates(c.Request.Context(), parseReq.Model, 0)
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

			// 非 Anthropic 上游不支持 /count_tokens，本地粗略估算
			if target.Provider.GetProtocol() != provider.ProtocolAnthropic {
				estimatedInput := len(body) / 4 // 简单字符估算
				c.JSON(http.StatusOK, gin.H{"usage": map[string]interface{}{
					"input_tokens": estimatedInput,
				}})
				return
			}

			// 使用路由解析到的 provider 发送 count_tokens 请求
			resp, err := target.Provider.CountTokens(c.Request.Context(), body)
			if err != nil {
				log.Error().Err(err).Str("provider", target.ProviderName).Msg("count_tokens upstream request failed")
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
			return
		}

		// 没有 model 字段或 model 为空时，尝试直接调用 anthropic provider 兜底
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
		sel, err := routerSvc.SelectCandidates(c.Request.Context(), req.Model, inputTokens)
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
			// 429 退避：不触发熔断，退避后继续 fallback 尝试下一个候选
			if res.StatusCode == 429 {
				backoff := parseRetryAfter(res.Body, 5*time.Second)
				log.Warn().Dur("backoff", backoff).Str("provider", targetProvider).Msg("rate limited (429), backing off")
				time.Sleep(backoff)
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
			log.Error().Int("status", protocolResult.StatusCode).RawJSON("body", body).Str("provider", targetProvider).Msg("upstream returned error")
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
					Type: func() string {
						if tt, ok := t["type"].(string); ok {
							return tt
						}
						return "function"
					}(),
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
				Type: func() string {
					if tt, ok := t["type"].(string); ok {
						return tt
					}
					return "function"
				}(),
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

// parseRetryAfter 从 429 响应的 body 中解析 Retry-After，返回退避时间。
func parseRetryAfter(body []byte, fallback time.Duration) time.Duration {
	if len(body) == 0 {
		return fallback
	}
	var parsed map[string]interface{}
	if json.Unmarshal(body, &parsed) != nil {
		return fallback
	}
	// 尝试嵌套的 error.retry_after 字段（OpenAI 格式）
	if errMsg, ok := parsed["error"].(map[string]interface{}); ok {
		if retryAfter, ok := errMsg["retry_after"].(float64); ok {
			d := time.Duration(retryAfter) * time.Second
			if d > 0 && d <= 300*time.Second {
				return d
			}
		}
	}
	// 尝试顶层 retry_after 字段
	if retryAfter, ok := parsed["retry_after"].(float64); ok {
		d := time.Duration(retryAfter) * time.Second
		if d > 0 && d <= 300*time.Second {
			return d
		}
	}
	return fallback
}

// handleAdminCalibration 管理端校准信息
func handleAdminCalibration(tokenService *token.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		info := tokenService.CalibrationInfo()
		c.JSON(http.StatusOK, info)
	}
}

// handleAdminBreakers 返回所有熔断器状态
func handleAdminBreakers(routerSvc *router.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		states := routerSvc.BreakerStates()
		c.JSON(http.StatusOK, states)
	}
}

// handleAdminUsageByRealModel 管理员查询按 real_model 汇总的 token 统计
func handleAdminUsageByRealModel(tokenService *token.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		startTime := c.Query("start_time")
		endTime := c.Query("end_time")

		summaries, err := tokenService.AggregateByRealModel(startTime, endTime)
		if err != nil {
			log.Error().Err(err).Msg("aggregate by real model failed")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
			return
		}
		if summaries == nil {
			summaries = []storage.UsageSummary{}
		}
		c.JSON(http.StatusOK, gin.H{
			"data":        summaries,
			"model_count": len(summaries),
		})
	}
}

// handleAdminUsageByAPIKey 管理员按 API Key + 时间粒度查询用量统计
func handleAdminUsageByAPIKey(authService *auth.Service, tokenService *token.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		apiKey := c.Query("api_key")
		granularity := c.DefaultQuery("granularity", "daily")
		startTime := c.Query("start_time")
		endTime := c.Query("end_time")

		// 支持按名称查询
		name := c.Query("name")
		if name != "" && apiKey == "" {
			if info, ok := authService.FindKeyByName(name); ok {
				apiKey = info.Key
			}
		}

		if apiKey == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "api_key or name is required"})
			return
		}

		summaries, err := tokenService.AggregateByAPIKey(apiKey, granularity, startTime, endTime)
		if err != nil {
			log.Error().Err(err).Msg("aggregate by api key failed")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
			return
		}
		if summaries == nil {
			summaries = []storage.UsageSummary{}
		}
		c.JSON(http.StatusOK, gin.H{
			"data":        summaries,
			"granularity": granularity,
		})
	}
}

// ================= 管理后台扩展 API =================

// handleAdminAPIKeys 列出所有 API Key
func handleAdminAPIKeys(authService *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		keys := authService.ListSeedKeys()
		c.JSON(http.StatusOK, gin.H{
			"data":  keys,
			"count": len(keys),
		})
	}
}

// handleAdminCreateAPIKey 新增 API Key
func handleAdminCreateAPIKey(authService *auth.Service, cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Key  string `json:"key"`
			Name string `json:"name"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}
		if req.Key == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "key is required"})
			return
		}
		if req.Name == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
			return
		}
		if ok := authService.CreateSeedKey(req.Key, req.Name); !ok {
			c.JSON(http.StatusConflict, gin.H{"error": "key already exists"})
			return
		}
		// 持久化到 config.yaml
		cfg.APIKeys = append(cfg.APIKeys, config.APIKeyConfig{Key: req.Key, Name: req.Name})
		if err := cfg.AppendAPIKey(config.APIKeyConfig{Key: req.Key, Name: req.Name}); err != nil {
			log.Error().Err(err).Msg("failed to save config after creating api key")
		}
		c.JSON(http.StatusCreated, gin.H{"message": "key created"})
	}
}

// handleAdminDeleteAPIKey 删除 API Key
func handleAdminDeleteAPIKey(authService *auth.Service, cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := c.Param("key")
		if ok := authService.DeleteSeedKey(key); !ok {
			c.JSON(http.StatusNotFound, gin.H{"error": "key not found"})
			return
		}
		// 从 config.yaml 中移除
		for i, k := range cfg.APIKeys {
			if k.Key == key {
				cfg.APIKeys = append(cfg.APIKeys[:i], cfg.APIKeys[i+1:]...)
				break
			}
		}
		if err := cfg.RemoveAPIKey(key); err != nil {
			log.Error().Err(err).Msg("failed to save config after deleting api key")
		}
		c.JSON(http.StatusOK, gin.H{"message": "key deleted"})
	}
}

// handleAdminAPIKeyUsage 按 API Key 查询用量统计
func handleAdminAPIKeyUsage(authService *auth.Service, tokenService *token.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := c.Param("key")
		startTime := c.Query("start_time")
		endTime := c.Query("end_time")

		inputTokens, outputTokens, totalTokens, requestCount, err := tokenService.SumTokensByAPIKey(key, "", startTime, endTime)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"key":            key,
			"input_tokens":   inputTokens,
			"output_tokens":  outputTokens,
			"total_tokens":   totalTokens,
			"request_count":  requestCount,
		})
	}
}

// handleAdminProviders 列出所有 Provider 及熔断器状态
func handleAdminProviders(routerSvc *router.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		breakerStates := routerSvc.BreakerStates()
		c.JSON(http.StatusOK, gin.H{
			"breaker_states": breakerStates,
		})
	}
}

// providerNameToEnvKey 把 provider 名转为环境变量 Key
// 如 "seneenova_me" → "SENEENOVA_ME_KEY"
func providerNameToEnvKey(name string) string {
	s := strings.ToUpper(name)
	s = strings.NewReplacer("-", "_", ".", "_", " ", "_").Replace(s)
	return s + "_KEY"
}

// resolveEnvKey 从现有 config 的 api_key 字段提取 env key 名称，
// 如果不存在则从 provider name 推导。
// 例如 "${SENSENOVA_AUTH_TOKEN}" → "SENSENOVA_AUTH_TOKEN"
//     "" (新 provider) → providerNameToEnvKey(name)
func resolveEnvKey(apiKeyRef string, providerName string) string {
	if strings.HasPrefix(apiKeyRef, "${") && strings.HasSuffix(apiKeyRef, "}") {
		return apiKeyRef[2 : len(apiKeyRef)-1]
	}
	return providerNameToEnvKey(providerName)
}

// updateEnvFile 在 .env 文件中设置 KEY=VALUE（追加或替换）
func updateEnvFile(key, value string) error {
	configDir := filepath.Dir("configs/config.yaml")
	envPath := filepath.Join(configDir, "..", ".env")

	// 读取当前 .env 文件
	lines := []string{}
	data, err := os.ReadFile(envPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("read .env failed: %w", err)
		}
	} else {
		lines = strings.Split(string(data), "\n")
	}

	// 查找并替换已有的 key，或标记追加
	found := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		eqIdx := strings.Index(trimmed, "=")
		if eqIdx > 0 && strings.TrimSpace(trimmed[:eqIdx]) == key {
			lines[i] = key + "=" + value
			found = true
			break
		}
	}

	if !found {
		lines = append(lines, key+"="+value)
	}

	output := strings.Join(lines, "\n")
	if !strings.HasSuffix(output, "\n") {
		output += "\n"
	}

	if err := os.WriteFile(envPath, []byte(output), 0644); err != nil {
		return fmt.Errorf("write .env failed: %w", err)
	}
	return nil
}

// updateEnvFileRemove 从 .env 文件中移除指定 KEY 的行
func updateEnvFileRemove(key string) {
	configDir := filepath.Dir("configs/config.yaml")
	envPath := filepath.Join(configDir, "..", ".env")
	data, err := os.ReadFile(envPath)
	if err != nil {
		return
	}
	lines := strings.Split(string(data), "\n")
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		eqIdx := strings.Index(trimmed, "=")
		if eqIdx > 0 && strings.TrimSpace(trimmed[:eqIdx]) == key {
			continue
		}
		filtered = append(filtered, line)
	}
	output := strings.Join(filtered, "\n")
	if !strings.HasSuffix(output, "\n") {
		output += "\n"
	}
	os.WriteFile(envPath, []byte(output), 0644)
}

// handleAdminProvidersConfig 列出所有 Provider 配置（隐藏 API Key）
func handleAdminProvidersConfig(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		type safeProvider struct {
			BaseURL  string `json:"base_url"`
			Protocol string `json:"protocol"`
			Timeout  string `json:"timeout"`
		}
		providers := make(map[string]safeProvider)
		for name, p := range cfg.Providers {
			providers[name] = safeProvider{
				BaseURL:  p.BaseURL,
				Protocol: p.Protocol,
				Timeout:  p.Timeout.String(),
			}
		}
		c.JSON(http.StatusOK, gin.H{"data": providers})
	}
}

// handleAdminAddProvider 新增 Provider 配置
func handleAdminAddProvider(cfg *config.Config, providerMgr *provider.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Name     string `json:"name"`
			BaseURL  string `json:"base_url"`
			APIKey   string `json:"api_key"`
			Protocol string `json:"protocol"`
			Timeout  string `json:"timeout"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
			return
		}
		if req.Name == "" || req.BaseURL == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name and base_url are required"})
			return
		}
		if _, exists := cfg.Providers[req.Name]; exists {
			c.JSON(http.StatusConflict, gin.H{"error": "provider already exists"})
			return
		}
		if req.Protocol == "" {
			req.Protocol = "openai"
		}
		timeout, err := time.ParseDuration(req.Timeout)
		if err != nil || timeout <= 0 {
			timeout = 3000 * time.Second
		}

		// 处理 API Key：存环境变量引用到 config.yaml，实际值写到 .env
		apiKeyRef := req.APIKey
		if apiKeyRef != "" {
			envKey := providerNameToEnvKey(req.Name)
			if err := updateEnvFile(envKey, apiKeyRef); err != nil {
				log.Error().Err(err).Msg("failed to update .env file")
			}
			apiKeyRef = "${" + envKey + "}"
		}

		cfg.Providers[req.Name] = config.ProviderConfig{
			BaseURL:  req.BaseURL,
			APIKey:   apiKeyRef,
			Protocol: req.Protocol,
			Timeout:  timeout,
		}
		if err := cfg.SaveProvider(req.Name, cfg.Providers[req.Name]); err != nil {
			log.Error().Err(err).Msg("failed to save config after adding provider")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to persist config"})
			return
		}

		// 立即生效：用实际 API Key 更新运行时的 Provider
		providerMgr.UpdateProvider(req.Name, config.ProviderConfig{
			BaseURL:  req.BaseURL,
			APIKey:   req.APIKey,
			Protocol: req.Protocol,
			Timeout:  timeout,
		})

		c.JSON(http.StatusCreated, gin.H{"message": "provider added"})
	}
}

// handleAdminUpdateProvider 更新 Provider 配置
func handleAdminUpdateProvider(cfg *config.Config, providerMgr *provider.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		name := c.Param("name")
		if _, exists := cfg.Providers[name]; !exists {
			c.JSON(http.StatusNotFound, gin.H{"error": "provider not found"})
			return
		}
		var req struct {
			BaseURL  string `json:"base_url"`
			APIKey   string `json:"api_key"`
			Protocol string `json:"protocol"`
			Timeout  string `json:"timeout"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
			return
		}
		p := cfg.Providers[name]
		actualKey := ""
		if req.BaseURL != "" {
			p.BaseURL = req.BaseURL
		}
		if req.Protocol != "" {
			p.Protocol = req.Protocol
		}
		if req.APIKey != "" {
			actualKey = req.APIKey
			// 复用现有 env key 名称，或从 provider name 推导
			envKey := resolveEnvKey(p.APIKey, name)
			if err := updateEnvFile(envKey, actualKey); err != nil {
				log.Error().Err(err).Msg("failed to update .env file")
			}
			p.APIKey = "${" + envKey + "}"
		}
		if req.Timeout != "" {
			timeout, err := time.ParseDuration(req.Timeout)
			if err == nil && timeout > 0 {
				p.Timeout = timeout
			}
		}
		cfg.Providers[name] = p
		if err := cfg.SaveProvider(name, cfg.Providers[name]); err != nil {
			log.Error().Err(err).Msg("failed to save config after updating provider")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to persist config"})
			return
		}

		// 立即生效：更新运行时的 Provider
		providerMgr.UpdateProvider(name, config.ProviderConfig{
			BaseURL:  p.BaseURL,
			APIKey:   actualKey,
			Protocol: p.Protocol,
			Timeout:  p.Timeout,
		})
		if actualKey == "" {
			// 没有新 API Key，从环境变量取回实际值
			ref := p.APIKey
			if strings.HasPrefix(ref, "$") && strings.HasSuffix(ref, "}") {
				if envVal := os.Getenv(ref[2 : len(ref)-1]); envVal != "" {
					if prov, ok := providerMgr.Get(name); ok {
						prov.SetAPIKey(envVal)
					}
				}
			}
		}

		c.JSON(http.StatusOK, gin.H{"message": "provider updated"})
	}
}

// handleAdminDeleteProvider 删除 Provider 配置
func handleAdminDeleteProvider(cfg *config.Config, providerMgr *provider.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		name := c.Param("name")
		if _, exists := cfg.Providers[name]; !exists {
			c.JSON(http.StatusNotFound, gin.H{"error": "provider not found"})
			return
		}
		delete(cfg.Providers, name)
		if err := cfg.DeleteProvider(name); err != nil {
			log.Error().Err(err).Msg("failed to save config after deleting provider")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to persist config"})
			return
		}

		// 立即生效：删除运行时的 Provider
		providerMgr.DeleteProvider(name)

		// 清理 .env 中的对应环境变量
		envKey := providerNameToEnvKey(name)
		updateEnvFileRemove(envKey)

		c.JSON(http.StatusOK, gin.H{"message": "provider deleted"})
	}
}

// handleAdminModels 列出所有虚拟模型
func handleAdminModels(mapperService *mapper.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		models := mapperService.ListVirtualModels()
		c.JSON(http.StatusOK, gin.H{
			"data":  models,
			"count": len(models),
		})
	}
}

// handleAdminAddModel 新增虚拟模型
func handleAdminAddModel(mapperService *mapper.Service, cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Name string `json:"name"`
			Tier string `json:"tier"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}
		if req.Name == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
			return
		}
		if ok := mapperService.AddModel(req.Name, req.Tier); !ok {
			c.JSON(http.StatusConflict, gin.H{"error": "model already exists"})
			return
		}
		// 持久化：更新 Config.Models
		cfg.Models = append(cfg.Models, config.ModelEntry{Name: req.Name, Tier: req.Tier})
		if err := cfg.AppendModel(config.ModelEntry{Name: req.Name, Tier: req.Tier}); err != nil {
			log.Error().Err(err).Msg("failed to save config after add model")
		}
		c.JSON(http.StatusCreated, gin.H{"message": "model added"})
	}
}

// handleAdminDeleteModel 删除虚拟模型
func handleAdminDeleteModel(mapperService *mapper.Service, cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		name := c.Param("name")
		// URL encode support
		if decoded, err := pathUnescape(name); err == nil {
			name = decoded
		}
		if ok := mapperService.DeleteModel(name); !ok {
			c.JSON(http.StatusNotFound, gin.H{"error": "model not found"})
			return
		}
		// 持久化：从 Config.Models 中移除
		for i, m := range cfg.Models {
			if m.Name == name {
				cfg.Models = append(cfg.Models[:i], cfg.Models[i+1:]...)
				break
			}
		}
		if err := cfg.RemoveModel(name); err != nil {
			log.Error().Err(err).Msg("failed to save config after delete model")
		}
		c.JSON(http.StatusOK, gin.H{"message": "model deleted"})
	}
}

// pathUnescape 简单的 URL 解码
func pathUnescape(s string) (string, error) {
	return url.PathUnescape(s)
}

// handleAdminRealModels 列出所有 real_model 路由配置
func handleAdminRealModels(routerSvc *router.Service, cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"strategy": routerSvc.GetStrategy(),
			"models":   cfg.RealModels.Models,
		})
	}
}

// handleAdminAddRealModel 新增 real_model 路由配置
func handleAdminAddRealModel(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Provider string  `json:"provider"`
			Model    string  `json:"model"`
			Weight   int     `json:"weight"`
			Tier     string  `json:"tier"`
			Cost     float64 `json:"cost"`
			Timeout  string  `json:"timeout"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
			return
		}
		if req.Provider == "" || req.Model == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "provider and model are required"})
			return
		}
		if req.Weight <= 0 {
			req.Weight = 1
		}
		timeout, err := time.ParseDuration(req.Timeout)
		if err != nil || timeout <= 0 {
			timeout = 3000 * time.Second
		}
		item := config.FallbackItem{
			Provider: req.Provider,
			Model:    req.Model,
			Weight:   req.Weight,
			Tier:     req.Tier,
			Cost:     req.Cost,
			Timeout:  timeout,
		}
		cfg.RealModels.Models = append(cfg.RealModels.Models, item)
		if err := cfg.AppendRealModel(item); err != nil {
			log.Error().Err(err).Msg("failed to save config after adding real model")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to persist config"})
			return
		}
		c.JSON(http.StatusCreated, gin.H{"message": "real model added", "model": item})
	}
}

// handleAdminUpdateRealModel 更新 real_model 路由配置（按索引）
func handleAdminUpdateRealModel(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		idx, err := parseIndex(c.Param("index"), len(cfg.RealModels.Models))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		var req struct {
			Provider string  `json:"provider"`
			Model    string  `json:"model"`
			Weight   int     `json:"weight"`
			Tier     string  `json:"tier"`
			Cost     float64 `json:"cost"`
			Timeout  string  `json:"timeout"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
			return
		}
		if req.Provider == "" || req.Model == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "provider and model are required"})
			return
		}
		if req.Weight <= 0 {
			req.Weight = 1
		}
		timeout, err := time.ParseDuration(req.Timeout)
		if err != nil || timeout <= 0 {
			timeout = 3000 * time.Second
		}
		item := config.FallbackItem{
			Provider: req.Provider,
			Model:    req.Model,
			Weight:   req.Weight,
			Tier:     req.Tier,
			Cost:     req.Cost,
			Timeout:  timeout,
		}
		cfg.RealModels.Models[idx] = item
		if err := cfg.UpdateRealModel(idx, item); err != nil {
			log.Error().Err(err).Msg("failed to save config after updating real model")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to persist config"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "real model updated", "model": item})
	}
}

// handleAdminDeleteRealModel 删除 real_model 路由配置（按索引）
func handleAdminDeleteRealModel(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		idx, err := parseIndex(c.Param("index"), len(cfg.RealModels.Models))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		removed := cfg.RealModels.Models[idx]
		cfg.RealModels.Models = append(cfg.RealModels.Models[:idx], cfg.RealModels.Models[idx+1:]...)
		if err := cfg.RemoveRealModel(idx); err != nil {
			log.Error().Err(err).Msg("failed to save config after deleting real model")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to persist config"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "real model deleted", "model": removed})
	}
}

// parseIndex 解析路径中的索引参数
func parseIndex(raw string, length int) (int, error) {
	var n int
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil {
		return 0, fmt.Errorf("invalid index: %w", err)
	}
	if n < 0 || n >= length {
		return 0, fmt.Errorf("index out of range: %d (valid: 0-%d)", n, length-1)
	}
	return n, nil
}

// handleAdminUpdateStrategy 更新路由策略
func handleAdminUpdateStrategy(routerSvc *router.Service, cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Strategy string `json:"strategy"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}
		valid := map[string]bool{"priority": true, "round_robin": true, "latency_optimized": true, "cost_optimized": true}
		if !valid[req.Strategy] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid strategy, must be one of: priority, round_robin, latency_optimized, cost_optimized"})
			return
		}
		routerSvc.SetStrategy(req.Strategy)
		// 持久化
		cfg.RealModels.Strategy = req.Strategy
		if err := cfg.SaveStrategy(req.Strategy); err != nil {
			log.Error().Err(err).Msg("failed to save config after strategy update")
		}
		c.JSON(http.StatusOK, gin.H{"message": "strategy updated", "strategy": req.Strategy})
	}
}

// handleAdminConfig 返回当前配置概览
func handleAdminConfig(appCfg config.AppConfig, mapperService *mapper.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		models := mapperService.ListVirtualModels()
		c.JSON(http.StatusOK, gin.H{
			"app": gin.H{
				"name":    appCfg.Name,
				"version": appCfg.Version,
				"env":     appCfg.Env,
				"port":    appCfg.Port,
			},
			"virtual_model_count": len(models),
		})
	}
}

// handleAdminLogin 管理后台登录
func handleAdminLogin(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Password string `json:"password"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		if cfg.Admin.Password != "" && req.Password != cfg.Admin.Password {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid password"})
			return
		}

		expiry := cfg.Admin.TokenExpiry
		if expiry <= 0 {
			expiry = 24 * time.Hour
		}

		token, exp, err := middleware.GenerateAdminToken(cfg.Admin.JWTSecret, expiry)
		if err != nil {
			log.Error().Err(err).Msg("failed to generate admin token")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate token"})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"token":       token,
			"expires_in":  exp,
			"token_type":  "Bearer",
		})
	}
}
