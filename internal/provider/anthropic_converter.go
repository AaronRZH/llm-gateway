package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// AnthropicRequest 网关接收的完整 Anthropic 请求体
type AnthropicRequest struct {
	Model         string                 `json:"model"`
	Messages      []map[string]interface{} `json:"messages"`
	System        interface{}            `json:"system,omitempty"`
	MaxTokens     int                    `json:"max_tokens"`
	Temperature   float64                `json:"temperature,omitempty"`
	TopP          float64                `json:"top_p,omitempty"`
	StopSequences []string               `json:"stop_sequences,omitempty"`
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
func (c *AnthropicConverter) ConvertMessagesToAnthropic(messages []Message) []map[string]interface{} {
	var result []map[string]interface{}
	for _, msg := range messages {
		result = append(result, map[string]interface{}{
			"role":    msg.Role,
			"content": c.ContentToBlocks(msg.Content),
		})
	}
	return result
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
		"model":    req.Model,
		"messages": c.ConvertMessagesToOpenAI(req.Messages, req.System),
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

// ConvertAnthropicMessagesToOpenAI 将 Anthropic 格式消息和工具转换为 OpenAI 格式
func (c *AnthropicConverter) ConvertAnthropicMessagesToOpenAI(
	messages []map[string]interface{},
	system interface{},
	tools []map[string]interface{},
) ([]Message, []Tool) {
	var result []Message

	// 1. system → 第一条 system message
	if system != nil {
		content := ""
		switch v := system.(type) {
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
		if content != "" {
			result = append(result, Message{Role: "system", Content: content})
		}
	}

	// 2. messages: content blocks → string
	for _, msg := range messages {
		role, _ := msg["role"].(string)
		if role != "user" && role != "assistant" && role != "system" {
			continue
		}
		content := c.FlattenContent(msg["content"])
		result = append(result, Message{Role: role, Content: content})
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

// ConvertOpenAIToAnthropicResponse 将 OpenAI 非流式响应体转换为 Anthropic 格式
func (c *AnthropicConverter) ConvertOpenAIToAnthropicResponse(body []byte, virtualModel string, inputTokens int) ([]byte, error) {
	var openAIResp struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Index        int    `json:"index"`
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Role    string `json:"role"`
				Content string `json:"content"`
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

	content := ""
	finishReason := "end_turn"
	if len(openAIResp.Choices) > 0 {
		content = openAIResp.Choices[0].Message.Content
		if openAIResp.Choices[0].FinishReason == "stop" {
			finishReason = "end_turn"
		} else if openAIResp.Choices[0].FinishReason != "" {
			finishReason = openAIResp.Choices[0].FinishReason
		}
	}

	// 构建 content blocks
	anthropicContent := []map[string]interface{}{
		{"type": "text", "text": content},
	}

	anthropicResp := map[string]interface{}{
		"id":          "msg_" + strings.TrimPrefix(openAIResp.ID, "chatcmpl-"),
		"type":        "message",
		"role":        "assistant",
		"content":     anthropicContent,
		"model":       virtualModel,
		"stop_reason": finishReason,
		"stop_sequence": nil,
		"usage": map[string]interface{}{
			"input_tokens":  inputTokens,
			"output_tokens": openAIResp.Usage.CompletionTokens,
		},
	}

	return json.Marshal(anthropicResp)
}
