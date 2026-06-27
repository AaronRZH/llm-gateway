package main

import (
	"encoding/json"
	"io"
	"net/http"

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
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
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

		// 2. 估算输入 token
		inputTokens := tokenService.EstimateInput(toTokenMessages(req.Messages), realModel)
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

			upstream, err := target.Provider.StreamChat(c.Request.Context(), upstreamModel, toProviderMessages(req.Messages))
			if err != nil {
				log.Error().Err(err).Msg("upstream stream failed")
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			defer upstream.Close()

			result := streamHandler.RewriteAndForward(c.Writer, upstream, req.Model)

			// 5. 流式：根据累计内容估算输出 token，异步记录用量
			estimatedOutput := tokenService.EstimateOutput(result.AccumulatedContent, realModel)
			go tokenService.RecordUsage(reqID, realModel, req.Model, targetProvider,
				inputTokens, estimatedOutput, 0, 0, 0)
		} else {
			// 非流式响应
			resp, err := target.Provider.Chat(c.Request.Context(), upstreamModel, toProviderMessages(req.Messages))
			if err != nil {
				log.Error().Err(err).Msg("upstream request failed")
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			defer resp.Body.Close()

			body, _ := io.ReadAll(resp.Body)

			// 5. 解析上游返回的真实 usage，异步记录用量
			realInput, realOutput, realTotal := parseUsage(body)
			go tokenService.RecordUsage(reqID, realModel, req.Model, targetProvider,
				inputTokens, 0, realInput, realOutput, realTotal)

			// 6. 重写响应中的 model 字段
			body = mapper.RewriteResponse(body, req.Model)
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

func handleListModels(mapper *mapper.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		models := mapper.ListVirtualModels()
		c.JSON(http.StatusOK, gin.H{
			"object": "list",
			"data":   models,
		})
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
