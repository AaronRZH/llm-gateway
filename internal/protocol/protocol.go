package protocol

import (
	"context"
	"io"
	"net/http"

	"llm-gateway/internal/provider"
	"llm-gateway/internal/router"
	"llm-gateway/internal/stream"
)

// Request Resolve 函数的输入参数
type Request struct {
	ClientProtocol provider.ClientProtocol // 客户端使用的协议
	UpstreamTarget *router.Target          // 路由目标（包含 Provider 和 GetProtocol()）
	ChatReq        *ChatCompletionRequest   // OpenAI 格式请求（非 nil 表示 OpenAI 客户端）
	AnthropicReq   *AnthropicRequest        // Anthropic 格式请求（非 nil 表示 Anthropic 客户端）
	ExtraParams    map[string]interface{}   // Case 4 专用（SendDirect 额外参数）
	IsStream       bool                     // 是否流式
	Ctx            context.Context        // 请求上下文
	StreamHandler  *stream.Handler          // SSE 转发器（可为 nil）
	VirtualModel   string                   // 虚拟模型名（用于 SSE 转换）
}

// Result Resolve 函数的返回结果
type Result struct {
	Response  *http.Response     // 完整响应（用于非流式StatusCode和Body）
	Body      []byte             // 非流式完整响应体（供后续body处理）
	StatusCode int               // HTTP状态码
	StreamBody io.ReadCloser      // 流式响应体（已包装 SSE converter 如需）
}

// Resolve 根据客户端协议和上游协议的组合执行对应的 HTTP 请求和转换逻辑。
// handler 侧负责重试循环（breaker 错误时 continue 重试下一个候选），
// Resolve 侧负责协议判断、格式转换和 SSE 转换包装。
func Resolve(req Request) (*Result, error) {
	clientProto := req.ClientProtocol
	upstreamProto := req.UpstreamTarget.Provider.GetProtocol()

	if clientProto == upstreamProto {
		// Case 1/4: 协议一致，直接转发
		if upstreamProto == provider.ProtocolAnthropic {
			// Case 4: Anthropic → Anthropic
			if req.IsStream {
				resp, err := req.UpstreamTarget.Provider.SendDirect(
					req.Ctx,
					req.UpstreamTarget.Model,
					req.AnthropicReq.Messages,
					req.AnthropicReq.System,
					req.ExtraParams,
					true,
				)
				if err != nil {
					return nil, err
				}
				return &Result{
					Response:   resp,
					StatusCode: resp.StatusCode,
					StreamBody: resp.Body,
				}, nil
			}
			resp, err := req.UpstreamTarget.Provider.SendDirect(
				req.Ctx,
				req.UpstreamTarget.Model,
				req.AnthropicReq.Messages,
				req.AnthropicReq.System,
				req.ExtraParams,
				false,
			)
			if err != nil {
				return nil, err
			}
			body, _ := io.ReadAll(resp.Body)
			return &Result{
				Response:   resp,
				Body:       body,
				StatusCode: resp.StatusCode,
			}, nil
		}
		// Case 1: OpenAI → OpenAI
		if req.ChatReq == nil {
			// Anthropic 客户端 → OpenAI 上游（非标准情况），使用 Chat
			messages := toProviderMessagesFromMap(req.AnthropicReq.Messages)
			tools := toolsFromAnthropicRequest(req.AnthropicReq.Tools)
			if req.IsStream {
				body, err := req.UpstreamTarget.Provider.StreamChat(req.Ctx, req.UpstreamTarget.Model, messages, tools)
				if err != nil {
					return nil, err
				}
				return &Result{
					StatusCode: http.StatusOK,
					StreamBody: body,
				}, nil
			}
			messages = toProviderMessagesFromMap(req.AnthropicReq.Messages)
			tools = toolsFromAnthropicRequest(req.AnthropicReq.Tools)
			resp, err := req.UpstreamTarget.Provider.Chat(req.Ctx, req.UpstreamTarget.Model, messages, tools)
			if err != nil {
				return nil, err
			}
			body, _ := io.ReadAll(resp.Body)
			return &Result{
				Response:   resp,
				Body:       body,
				StatusCode: resp.StatusCode,
			}, nil
		}
		// OpenAI 客户端 → OpenAI 上游
		if req.IsStream {
			body, err := req.UpstreamTarget.Provider.StreamChat(
				req.Ctx, req.UpstreamTarget.Model,
				toProviderMessages(req.ChatReq.Messages),
				toProviderTools(req.ChatReq.Tools),
			)
			if err != nil {
				return nil, err
			}
			return &Result{
				StatusCode: http.StatusOK,
				StreamBody: body,
			}, nil
		}
		resp, err := req.UpstreamTarget.Provider.Chat(
			req.Ctx, req.UpstreamTarget.Model,
			toProviderMessages(req.ChatReq.Messages),
			toProviderTools(req.ChatReq.Tools),
		)
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(resp.Body)
		return &Result{
			Response:   resp,
			Body:       body,
			StatusCode: resp.StatusCode,
		}, nil
	}

	// clientProto != upstreamProto
	if clientProto == provider.ProtocolOpenAI && upstreamProto == provider.ProtocolAnthropic {
		// Case 2: OpenAI 客户端 → Anthropic 上游
		if req.IsStream {
			body, err := req.UpstreamTarget.Provider.StreamChatWithProtocol(
				req.Ctx, req.UpstreamTarget.Model,
				toProviderMessages(req.ChatReq.Messages),
				toProviderTools(req.ChatReq.Tools),
				provider.ProtocolOpenAI,
			)
			if err != nil {
				return nil, err
			}
			return &Result{
				StatusCode: http.StatusOK,
				StreamBody: stream.NewOpenAIStreamConverter(body, req.VirtualModel),
			}, nil
		}
		// 非流式：ChatWithProtocol 转换格式
		resp, err := req.UpstreamTarget.Provider.ChatWithProtocol(
			req.Ctx, req.UpstreamTarget.Model,
			toProviderMessages(req.ChatReq.Messages),
			toProviderTools(req.ChatReq.Tools),
			provider.ProtocolOpenAI,
		)
		if err != nil {
			return nil, err
		}
		respBody, _ := io.ReadAll(resp.Body)
		converted, convErr := req.UpstreamTarget.Provider.ConvertOpenAIToAnthropicResponse(respBody, req.VirtualModel, 0)
		if convErr == nil {
			return &Result{
				Response:   resp,
				Body:       converted,
				StatusCode: http.StatusOK,
			}, nil
		}
		return &Result{
			Response:   resp,
			Body:       respBody,
			StatusCode: resp.StatusCode,
		}, nil
	}

	// Case 3: Anthropic 客户端 → OpenAI 上游
	if req.IsStream {
		openAIMsgs, openAITools := req.UpstreamTarget.Provider.ConvertAnthropicMessagesToOpenAI(
			req.AnthropicReq.Messages, req.AnthropicReq.System, req.AnthropicReq.Tools)
		body, err := req.UpstreamTarget.Provider.StreamChat(
			req.Ctx, req.UpstreamTarget.Model, openAIMsgs, openAITools)
		if err != nil {
			return nil, err
		}
		return &Result{
			StatusCode: http.StatusOK,
			StreamBody: stream.NewAnthropicSSEConverter(body, req.VirtualModel),
		}, nil
	}

	// 非流式 Case 3
	openAIMsgs, openAITools := req.UpstreamTarget.Provider.ConvertAnthropicMessagesToOpenAI(
		req.AnthropicReq.Messages, req.AnthropicReq.System, req.AnthropicReq.Tools)
	resp, err := req.UpstreamTarget.Provider.Chat(
		req.Ctx, req.UpstreamTarget.Model, openAIMsgs, openAITools)
	if err != nil {
		return nil, err
	}
	respBody, _ := io.ReadAll(resp.Body)
	converted, convErr := req.UpstreamTarget.Provider.ConvertOpenAIToAnthropicResponse(respBody, req.VirtualModel, 0)
	if convErr == nil {
		return &Result{
			Response:   resp,
			Body:       converted,
			StatusCode: http.StatusOK,
		}, nil
	}
	return &Result{
		Response:   resp,
		Body:       respBody,
		StatusCode: resp.StatusCode,
	}, nil
}

// ==================== 辅助函数 ====================

// toProviderMessages 将网关的 []Message 转为 provider.Message
func toProviderMessages(msgs []Message) []provider.Message {
	out := make([]provider.Message, len(msgs))
	for i, m := range msgs {
		out[i] = provider.Message{Role: m.Role, Content: m.Content}
	}
	return out
}

// toProviderMessagesFromMap 将 Anthropic 格式的 []map 转为 provider.Message
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

// toProviderTools 将网关的 []Tool 转为 provider.Tool
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

// toolsFromAnthropicRequest 将 Anthropic 请求中的 tools 转为 Provider 格式的 Tool
func toolsFromAnthropicRequest(reqTools []map[string]interface{}) []provider.Tool {
	if len(reqTools) == 0 {
		return nil
	}
	out := make([]provider.Tool, 0, len(reqTools))
	for _, t := range reqTools {
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
