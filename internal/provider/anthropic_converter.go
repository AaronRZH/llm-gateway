package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// AnthropicRequest 网关接收的完整 Anthropic 请求体
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

// AnthropicResponse 从后端收到 OpenAI 格式的响应后，转换成的 Anthropic 格式响应
type AnthropicResponse struct {
	ID           string                   `json:"id"`
	Type         string                   `json:"type"`
	Model        string                   `json:"model"`
	Role         string                   `json:"role"`
	Content      []map[string]interface{} `json:"content"`
	Usage        map[string]interface{}   `json:"usage"`
	FinishReason string                   `json:"finish_reason"`
}

// AnthropicConverter 负责 Anthropic ↔ OpenAI 格式转换
type AnthropicConverter struct{}

// NewAnthropicConverter 创建 AnthropicConverter
func NewAnthropicConverter() *AnthropicConverter {
	return &AnthropicConverter{}
}

// contentToBlocks 将消息内容转换为 Anthropic content blocks 格式
// 处理所有可能的输入类型：
//   - string: 包装为 {type: "text", text: v}
//   - []interface{}: 遍历每个元素，字符串元素转换为 text block，map 元素保留并验证必需字段
//   - map[string]interface{}: 解包单个 map 对象，保留所有字段
//   - 其他 (null, float64, bool): 转换为空文本块
func (c *AnthropicConverter) ContentToBlocks(content interface{}) []map[string]interface{} {
	switch v := content.(type) {
	case string:
		return []map[string]interface{}{{"type": "text", "text": v}}
	case []interface{}:
		blocks := make([]map[string]interface{}, 0, len(v))
		for _, block := range v {
			switch b := block.(type) {
			case map[string]interface{}:
				// 对已知的 content block type，补充缺失的必需字段
				if bt, ok := b["type"].(string); ok {
					switch bt {
					case "text":
						if _, hasText := b["text"]; !hasText {
							b["text"] = ""
						}
					case "image":
						if _, hasSource := b["source"]; !hasSource {
							b["source"] = nil
						}
					case "tool_use":
						if _, hasID := b["id"]; !hasID {
							b["id"] = ""
						}
						if _, hasName := b["name"]; !hasName {
							b["name"] = ""
						}
						if _, hasInput := b["input"]; !hasInput {
							b["input"] = map[string]interface{}{}
						}
					case "tool_result":
						if _, hasToolID := b["tool_use_id"]; !hasToolID {
							b["tool_use_id"] = ""
						}
						if _, hasContent := b["content"]; !hasContent {
							b["content"] = ""
						}
					case "thinking":
						if _, hasThinking := b["thinking"]; !hasThinking {
							b["thinking"] = ""
						}
					case "document":
						if _, hasSource := b["source"]; !hasSource {
							b["source"] = nil
						}
					case "input_audio":
						if _, hasAudio := b["input_audio"]; !hasAudio {
							b["input_audio"] = nil
						}
					}
				}
				blocks = append(blocks, b)
			case string:
				// 数组中的字符串元素 → 转换为 text block
				blocks = append(blocks, map[string]interface{}{"type": "text", "text": b})
			case float64:
				// 数字 → 转换为字符串表示
				blocks = append(blocks, map[string]interface{}{"type": "text", "text": fmt.Sprintf("%v", b)})
			case bool:
				// 布尔值 → 转换为字符串表示
				blocks = append(blocks, map[string]interface{}{"type": "text", "text": fmt.Sprintf("%v", b)})
			case nil:
				// null → 跳过（不添加到 blocks）
				continue
			default:
				// 其他未知类型（嵌套数组等）→ 忽略
				continue
			}
		}
		// 如果 blocks 为空（原数组只有字符串等非 map 元素），返回一个空文本块
		// 避免返回空数组导致 API 422
		if len(blocks) == 0 {
			return []map[string]interface{}{{"type": "text", "text": ""}}
		}
		return blocks
	default:
		// 单个 map 对象（不是数组）→ 解包返回，保留所有字段
		if m, ok := content.(map[string]interface{}); ok {
			return []map[string]interface{}{m}
		}
		return []map[string]interface{}{{"type": "text", "text": ""}}
	}
}

// convertMessages 转换消息格式（OpenAI → Anthropic）
// 除文本外，还处理 OpenAI 工具调用协议：
//   - assistant 消息带 tool_calls → 追加 tool_use content blocks
//   - role:"tool" 消息（工具结果）→ 转为 user 消息里的 tool_result content block
func (c *AnthropicConverter) ConvertMessagesToAnthropic(messages []Message) ([]map[string]interface{}, interface{}) {
	var result []map[string]interface{}
	var systemParts []string
	for _, msg := range messages {
		switch msg.Role {
		case "system":
			// OpenAI system 角色 → Anthropic 顶层 system 参数（多条合并，避免 messages 里出现非法的 role:"system" 导致上游 400）
			if msg.Content != "" {
				systemParts = append(systemParts, msg.Content)
			}
			continue
		case "assistant":
			// 文本 content blocks
			var content []map[string]interface{}
			if msg.Content != "" {
				content = c.ContentToBlocks(msg.Content)
			}
			// OpenAI tool_calls → Anthropic tool_use blocks
			for _, tc := range msg.ToolCalls {
				id, _ := tc["id"].(string)
				fn, _ := tc["function"].(map[string]interface{})
				name, _ := fn["name"].(string)
				argsRaw, _ := fn["arguments"].(string)
				var input map[string]interface{}
				if argsRaw == "" {
					input = map[string]interface{}{}
				} else if err := json.Unmarshal([]byte(argsRaw), &input); err != nil {
					// arguments 解析失败（非合法 JSON）时保留原始串，避免上游 400
					input = map[string]interface{}{"raw": argsRaw}
				}
				content = append(content, map[string]interface{}{
					"type":  "tool_use",
					"id":    id,
					"name":  name,
					"input": input,
				})
			}
			if content == nil {
				// Anthropic 不允许 content 为空数组，至少给一个空文本块
				content = []map[string]interface{}{{"type": "text", "text": ""}}
			}
			result = append(result, map[string]interface{}{
				"role":    "assistant",
				"content": content,
			})
		case "tool":
			// OpenAI role:"tool" → Anthropic user 消息里的 tool_result block
			result = append(result, map[string]interface{}{
				"role": "user",
				"content": []map[string]interface{}{
					{
						"type":        "tool_result",
						"tool_use_id": msg.ToolCallID,
						"content":     msg.Content,
					},
				},
			})
		default:
			result = append(result, map[string]interface{}{
				"role":    msg.Role,
				"content": c.ContentToBlocks(msg.Content),
			})
		}
	}

	var system interface{}
	if len(systemParts) > 0 {
		system = strings.Join(systemParts, "\n\n")
	}
	return result, system
}

// convertMessagesToOpenAI 将 Anthropic 消息列表（含 system）转为 OpenAI 消息列表
func (c *AnthropicConverter) ConvertMessagesToOpenAI(messages []map[string]interface{}, system interface{}) []Message {
	var result []Message

	// 如果有 system 参数，转为 system 消息
	if system != nil {
		content := c.ContentToBlocks(system)
		text := ""
		if len(content) > 0 && content[0]["text"] != nil {
			text = content[0]["text"].(string)
		}
		result = append(result, Message{Role: "system", Content: text})
	}

	// 转换用户/助手消息
	for _, msg := range messages {
		role, _ := msg["role"].(string)
		if role == "user" || role == "assistant" {
			blocks := c.ContentToBlocks(msg["content"])
			text := ""
			for _, b := range blocks {
				if b["type"] == "text" && b["text"] != nil {
					text += b["text"].(string)
				}
			}
			result = append(result, Message{Role: role, Content: text})
		}
	}

	return result
}

// ConvertTools 将 OpenAI 格式的 tools 转换为 Anthropic 格式
func (c *AnthropicConverter) ConvertTools(tools []Tool) []map[string]interface{} {
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

// ConvertRequest 将完整的 Anthropic 请求转换为后端请求体（OpenAI 格式）
func (c *AnthropicConverter) ConvertRequest(req *AnthropicRequest) (map[string]interface{}, error) {
	body := map[string]interface{}{
		"model":      req.Model,
		"messages":   c.ConvertMessagesToOpenAI(req.Messages, req.System),
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

// ConvertResponse 将后端返回的 ChatCompletionResponse 转为 Anthropic 格式的响应
func (c *AnthropicConverter) ConvertResponse(resp *http.Response) ([]byte, error) {
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
				// 跳过 thinking 和 cache_control 整个块，而非仅删除字段
				// 否则客户端解析到 {type: "thinking"} 会尝试访问 s.thinking.length → undefined
				if blockType, ok := blockMap["type"].(string); ok {
					if blockType == "thinking" || blockType == "cache_control" {
						continue
					}
				}
				filteredContent = append(filteredContent, blockMap)
			}
		}
		anthropicResp["content"] = filteredContent
	} else if content, ok := respBody["content"].(string); ok {
		anthropicResp["content"] = content
	} else {
		anthropicResp["content"] = ""
	}

	// usage：支持 Anthropic 格式（input_tokens/output_tokens）和后端代理的 OpenAI 格式（prompt_tokens/completion_tokens）
	var inputTokens, outputTokens, totalTokens float64
	if usage, ok := respBody["usage"].(map[string]interface{}); ok {
		inputTokens, _ = usage["input_tokens"].(float64)
		outputTokens, _ = usage["output_tokens"].(float64)
		if inputTokens == 0 {
			inputTokens, _ = usage["prompt_tokens"].(float64)
		}
		if outputTokens == 0 {
			outputTokens, _ = usage["completion_tokens"].(float64)
		}
		totalTokens, _ = usage["total_tokens"].(float64)
		if totalTokens == 0 {
			totalTokens = inputTokens + outputTokens
		}
	}
	anthropicResp["usage"] = map[string]interface{}{
		"input_tokens":  int(inputTokens),
		"output_tokens": int(outputTokens),
		"total_tokens":  int(totalTokens),
	}

	jsonBody, err := json.Marshal(anthropicResp)
	if err != nil {
		return nil, err
	}

	return jsonBody, nil
}

// ConvertResponseWithModel 将后端响应转为 Anthropic 格式，使用指定的虚拟模型名
func (c *AnthropicConverter) ConvertResponseWithModel(resp *http.Response, virtualModel string) ([]byte, error) {
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
				// 跳过 thinking 和 cache_control 整个块，而非仅删除字段
				// 否则客户端解析到 {type: "thinking"} 会尝试访问 s.thinking.length → undefined
				if blockType, ok := blockMap["type"].(string); ok {
					if blockType == "thinking" || blockType == "cache_control" {
						continue
					}
				}
				filteredContent = append(filteredContent, blockMap)
			}
		}
		anthropicResp["content"] = filteredContent
	} else if content, ok := respBody["content"].(string); ok {
		anthropicResp["content"] = content
	} else {
		anthropicResp["content"] = ""
	}

	// usage：支持 Anthropic 格式（input_tokens/output_tokens）和后端代理的 OpenAI 格式（prompt_tokens/completion_tokens）
	var inputTokens, outputTokens, totalTokens float64
	if usage, ok := respBody["usage"].(map[string]interface{}); ok {
		inputTokens, _ = usage["input_tokens"].(float64)
		outputTokens, _ = usage["output_tokens"].(float64)
		if inputTokens == 0 {
			inputTokens, _ = usage["prompt_tokens"].(float64)
		}
		if outputTokens == 0 {
			outputTokens, _ = usage["completion_tokens"].(float64)
		}
		totalTokens, _ = usage["total_tokens"].(float64)
		if totalTokens == 0 {
			totalTokens = inputTokens + outputTokens
		}
	}
	anthropicResp["usage"] = map[string]interface{}{
		"input_tokens":  int(inputTokens),
		"output_tokens": int(outputTokens),
		"total_tokens":  int(totalTokens),
	}

	jsonBody, err := json.Marshal(anthropicResp)
	if err != nil {
		return nil, err
	}

	return jsonBody, nil
}

// ConvertAnthropicMessagesToOpenAI 将 Anthropic 格式消息和工具转换为 OpenAI 格式。
// 处理 content blocks（text、tool_use、tool_result），
// tool_use → assistant + tool_calls，tool_result → role:"tool"。
func (c *AnthropicConverter) ConvertAnthropicMessagesToOpenAI(
	messages []map[string]interface{},
	system interface{},
	tools []map[string]interface{},
) ([]Message, []Tool) {
	var result []Message

	// 1. system → 第一条 system message
	var systemContent string
	if system != nil {
		switch v := system.(type) {
		case string:
			systemContent = v
		case []interface{}:
			for _, block := range v {
				if b, ok := block.(map[string]interface{}); ok {
					if b["type"] == "text" {
						if t, ok := b["text"].(string); ok {
							systemContent += t
						}
					}
				}
			}
		}
	}

	// 2. messages: content blocks → OpenAI 格式
	for _, msg := range messages {
		role, _ := msg["role"].(string)

		// role="system" 合并到 systemContent
		if role == "system" {
			systemContent += "\n\n" + c.FlattenContent(msg["content"])
			continue
		}

		if role != "user" && role != "assistant" {
			continue
		}

		// 解析 content blocks
		textContent, toolCalls, toolResults := c.parseContentBlocks(msg["content"])

		switch {
		case role == "assistant" && len(toolCalls) > 0:
			// assistant 消息带有 tool_use → OpenAI tool_calls 格式
			result = append(result, Message{
				Role:      "assistant",
				Content:   textContent,
				ToolCalls: toolCalls,
			})

		case role == "user" && len(toolResults) > 0:
			// user 消息带有 tool_result → 每个转为一条 role:"tool" 消息
			for _, tr := range toolResults {
				toolCallID, _ := tr["tool_call_id"].(string)
				content, _ := tr["content"].(string)
				result = append(result, Message{
					Role:       "tool",
					Content:    content,
					ToolCallID: toolCallID,
				})
			}
			// 如果同时还有文本内容，追加一条 user 消息
			if textContent != "" {
				result = append(result, Message{Role: "user", Content: textContent})
			}

		default:
			// 纯文本消息
			if textContent == "" && role == "assistant" {
				continue // 跳过空的 assistant 消息
			}
			result = append(result, Message{Role: role, Content: textContent})
		}
	}

	// 合并后的 systemContent 放到最前面
	if systemContent != "" {
		result = append([]Message{{Role: "system", Content: systemContent}}, result...)
	}

	// 3. tools: Anthropic → OpenAI 格式
	var openAITools []Tool
	for _, t := range tools {
		name, _ := t["name"].(string)
		desc, _ := t["description"].(string)
		inputSchema, _ := t["input_schema"]
		openAITools = append(openAITools, Tool{
			Type: "function",
			Function: ToolFunc{
				Name:        name,
				Description: desc,
				Parameters:  inputSchema,
			},
		})
	}

	return result, openAITools
}

// FlattenContent 将 Anthropic content blocks 展平为字符串
func (c *AnthropicConverter) FlattenContent(content interface{}) string {
	switch v := content.(type) {
	case string:
		return v
	case []interface{}:
		var parts []string
		for _, block := range v {
			if b, ok := block.(map[string]interface{}); ok {
				if b["type"] == "text" {
					if t, ok := b["text"].(string); ok {
						parts = append(parts, t)
					}
				}
			}
		}
		return strings.Join(parts, "")
	default:
		return ""
	}
}

// parseContentBlocks 解析 Anthropic content blocks，提取文本、tool_use 和 tool_result。
// 返回：textContent、toolCalls（OpenAI 格式）、toolResults（OpenAI 格式）。
func (c *AnthropicConverter) parseContentBlocks(content interface{}) (string, []map[string]interface{}, []map[string]interface{}) {
	blocks, ok := content.([]interface{})
	if !ok {
		// 不是 content blocks 数组，按字符串处理
		return c.FlattenContent(content), nil, nil
	}

	var textParts []string
	var toolCalls []map[string]interface{}
	var toolResults []map[string]interface{}

	for _, block := range blocks {
		b, ok := block.(map[string]interface{})
		if !ok {
			continue
		}
		blockType, _ := b["type"].(string)
		switch blockType {
		case "text":
			if t, ok := b["text"].(string); ok {
				textParts = append(textParts, t)
			}
		case "tool_use":
			// Anthropic tool_use → OpenAI tool_call
			id, _ := b["id"].(string)
			name, _ := b["name"].(string)
			input, _ := b["input"]
			argsJSON, _ := json.Marshal(input)
			toolCalls = append(toolCalls, map[string]interface{}{
				"id":   id,
				"type": "function",
				"function": map[string]interface{}{
					"name":      name,
					"arguments": string(argsJSON),
				},
			})
		case "tool_result":
			// Anthropic tool_result → OpenAI role:"tool" 消息
			toolUseID, _ := b["tool_use_id"].(string)
			resultContent := ""
			switch v := b["content"].(type) {
			case string:
				resultContent = v
			case []interface{}:
				// tool_result content 也可能是 content blocks
				for _, cb := range v {
					if cbm, ok := cb.(map[string]interface{}); ok {
						if cbm["type"] == "text" {
							if t, ok := cbm["text"].(string); ok {
								resultContent += t
							}
						}
					}
				}
			}
			toolResults = append(toolResults, map[string]interface{}{
				"tool_call_id": toolUseID,
				"content":      resultContent,
			})
		}
	}

	return strings.Join(textParts, ""), toolCalls, toolResults
}

// ConvertOpenAIToAnthropicResponse 将 OpenAI 非流式响应体转换为 Anthropic 格式。
// 支持 text 和 tool_calls 两种响应内容。
func (c *AnthropicConverter) ConvertOpenAIToAnthropicResponse(body []byte, virtualModel string, inputTokens int) ([]byte, error) {
	var openAIResp struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Index        int    `json:"index"`
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Role      string `json:"role"`
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls,omitempty"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &openAIResp); err != nil {
		return nil, err
	}

	// 构建 Anthropic content blocks
	var anthropicContent []map[string]interface{}
	finishReason := "end_turn"

	if len(openAIResp.Choices) > 0 {
		choice := openAIResp.Choices[0]

		// 映射 finish_reason → stop_reason
		switch choice.FinishReason {
		case "stop":
			finishReason = "end_turn"
		case "tool_calls":
			finishReason = "tool_use"
		case "length":
			finishReason = "max_tokens"
		default:
			if choice.FinishReason != "" {
				finishReason = choice.FinishReason
			}
		}

		// 文本内容
		if choice.Message.Content != "" {
			anthropicContent = append(anthropicContent, map[string]interface{}{
				"type": "text",
				"text": choice.Message.Content,
			})
		}

		// tool_calls → Anthropic tool_use content blocks
		for _, tc := range choice.Message.ToolCalls {
			var input map[string]interface{}
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil {
				// arguments 解析失败时保留原始字符串
				input = map[string]interface{}{"raw": tc.Function.Arguments}
			}
			anthropicContent = append(anthropicContent, map[string]interface{}{
				"type":  "tool_use",
				"id":    tc.ID,
				"name":  tc.Function.Name,
				"input": input,
			})
		}
	}

	// 确保 content 不为空
	if len(anthropicContent) == 0 {
		anthropicContent = []map[string]interface{}{
			{"type": "text", "text": ""},
		}
	}

	// usage: 优先使用 OpenAI 响应中的真实数据
	realInputTokens := openAIResp.Usage.PromptTokens
	if realInputTokens == 0 {
		realInputTokens = inputTokens
	}

	anthropicResp := map[string]interface{}{
		"id":            "msg_" + strings.TrimPrefix(openAIResp.ID, "chatcmpl-"),
		"type":          "message",
		"role":          "assistant",
		"content":       anthropicContent,
		"model":         virtualModel,
		"stop_reason":   finishReason,
		"stop_sequence": nil,
		"usage": map[string]interface{}{
			"input_tokens":  realInputTokens,
			"output_tokens": openAIResp.Usage.CompletionTokens,
		},
	}

	return json.Marshal(anthropicResp)
}

// ConvertAnthropicToOpenAIResponse 将 Anthropic 非流式响应体转换为 OpenAI 格式。
// 用于 Case 2 (OpenAI 客户端 → Anthropic 上游) 的非流式路径：
// ChatWithProtocol 发送 Anthropic 请求 → 上游返回 Anthropic 格式响应 → 此方法转换为 OpenAI 格式。
func (c *AnthropicConverter) ConvertAnthropicToOpenAIResponse(body []byte, virtualModel string) ([]byte, error) {
	var anthropicResp struct {
		ID         string                   `json:"id"`
		Model      string                   `json:"model"`
		Content    []map[string]interface{} `json:"content"`
		StopReason string                   `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &anthropicResp); err != nil {
		return nil, err
	}

	// 提取 text content 和 tool_use blocks
	var textParts []string
	var toolCalls []map[string]interface{}

	for _, block := range anthropicResp.Content {
		blockType, _ := block["type"].(string)
		switch blockType {
		case "text":
			if t, ok := block["text"].(string); ok {
				textParts = append(textParts, t)
			}
		case "tool_use":
			id, _ := block["id"].(string)
			name, _ := block["name"].(string)
			input, _ := block["input"]
			argsJSON, err := json.Marshal(input)
			if err != nil {
				argsJSON = []byte("{}")
			}
			toolCalls = append(toolCalls, map[string]interface{}{
				"id":   id,
				"type": "function",
				"function": map[string]interface{}{
					"name":      name,
					"arguments": string(argsJSON),
				},
			})
		}
	}

	content := strings.Join(textParts, "")

	// 映射 stop_reason → finish_reason
	finishReason := "stop"
	switch anthropicResp.StopReason {
	case "end_turn":
		finishReason = "stop"
	case "max_tokens":
		finishReason = "length"
	case "tool_use":
		finishReason = "tool_calls"
	case "stop_sequence":
		finishReason = "stop"
	default:
		if anthropicResp.StopReason != "" {
			finishReason = anthropicResp.StopReason
		}
	}

	// 构建 OpenAI 格式的 message
	message := map[string]interface{}{
		"role": "assistant",
	}
	if len(toolCalls) > 0 {
		message["content"] = ""
		message["tool_calls"] = toolCalls
	} else {
		message["content"] = content
	}

	// 构建 OpenAI 格式的 id
	id := "chatcmpl-" + strings.TrimPrefix(anthropicResp.ID, "msg_")
	if anthropicResp.ID == "" {
		id = "chatcmpl-" + uuid.New().String()[:12]
	}

	openAIResp := map[string]interface{}{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   virtualModel,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
		"usage": map[string]interface{}{
			"prompt_tokens":     anthropicResp.Usage.InputTokens,
			"completion_tokens": anthropicResp.Usage.OutputTokens,
			"total_tokens":      anthropicResp.Usage.InputTokens + anthropicResp.Usage.OutputTokens,
		},
	}

	return json.Marshal(openAIResp)
}
