package protocol

import (
	"context"
	"io"
	"strings"
	"testing"

	"llm-gateway/internal/provider"
)

// ==================== ProviderBehavior 接口 ====================

// providerBehavior 提取 Provider 的关键行为为接口，供 mock 实现。
type providerBehavior interface {
	GetProtocol() provider.ClientProtocol
	Chat(ctx context.Context, model string, messages []provider.Message, tools []provider.Tool) (body []byte, err error)
	StreamChat(ctx context.Context, model string, messages []provider.Message, tools []provider.Tool) (streamBody string, err error)
	ChatWithProtocol(ctx context.Context, model string, messages []provider.Message, tools []provider.Tool, clientProtocol provider.ClientProtocol, maxTokens int) (body []byte, err error)
	StreamChatWithProtocol(ctx context.Context, model string, messages []provider.Message, tools []provider.Tool, clientProtocol provider.ClientProtocol, maxTokens int) (streamBody string, err error)
	SendDirect(ctx context.Context, model string, messages []map[string]interface{}, system interface{}, extraParams map[string]interface{}, stream bool) (body []byte, err error)
	ConvertAnthropicMessagesToOpenAI(messages []map[string]interface{}, system interface{}, tools []map[string]interface{}) ([]provider.Message, []provider.Tool)
	ConvertOpenAIToAnthropicResponse(body []byte, virtualModel string, inputTokens int) ([]byte, error)
}

// mockProvider 实现 providerBehavior 接口，记录调用历史。
type mockProvider struct {
	protocol   provider.ClientProtocol
	calls      []string
	chatBody   []byte
	streamBody string
	converter  *mockConverter
}

type mockConverter struct {
	conversionResult  []provider.Message
	conversionTools   []provider.Tool
	responseConverted []byte
	responseError     error
}

func (m *mockProvider) GetProtocol() provider.ClientProtocol {
	return m.protocol
}

func (m *mockProvider) Chat(_ context.Context, _ string, _ []provider.Message, _ []provider.Tool) ([]byte, error) {
	m.calls = append(m.calls, "Chat")
	if m.chatBody != nil {
		return m.chatBody, nil
	}
	return []byte(`{}`), nil
}

func (mockProvider) StreamChat(_ context.Context, _ string, _ []provider.Message, _ []provider.Tool) (string, error) {
	return "", nil
}

func (m *mockProvider) ChatWithProtocol(_ context.Context, _ string, _ []provider.Message, _ []provider.Tool, _ provider.ClientProtocol, _ int) ([]byte, error) {
	m.calls = append(m.calls, "ChatWithProtocol")
	if m.chatBody != nil {
		return m.chatBody, nil
	}
	return []byte(`{}`), nil
}

func (mockProvider) StreamChatWithProtocol(_ context.Context, _ string, _ []provider.Message, _ []provider.Tool, _ provider.ClientProtocol, _ int) (string, error) {
	return "", nil
}

func (m *mockProvider) SendDirect(_ context.Context, _ string, _ []map[string]interface{}, _ interface{}, _ map[string]interface{}, _ bool) ([]byte, error) {
	m.calls = append(m.calls, "SendDirect")
	return []byte(`{}`), nil
}

func (m *mockProvider) ConvertAnthropicMessagesToOpenAI(_ []map[string]interface{}, _ interface{}, _ []map[string]interface{}) ([]provider.Message, []provider.Tool) {
	m.calls = append(m.calls, "ConvertAnthropicMessagesToOpenAI")
	return m.converter.conversionResult, m.converter.conversionTools
}

func (m *mockProvider) ConvertOpenAIToAnthropicResponse(body []byte, _ string, _ int) ([]byte, error) {
	m.calls = append(m.calls, "ConvertOpenAIToAnthropicResponse")
	return m.converter.responseConverted, m.converter.responseError
}

// ==================== Helper: 工具函数直接测试 ====================

func TestToProviderMessages(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "Hello"},
		{Role: "assistant", Content: "Hi back"},
	}
	result := toProviderMessages(msgs)
	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result))
	}
	if result[0].Role != "user" || result[0].Content != "Hello" {
		t.Errorf("unexpected message[0]: %+v", result[0])
	}
	if result[1].Role != "assistant" || result[1].Content != "Hi back" {
		t.Errorf("unexpected message[1]: %+v", result[1])
	}
}

func TestToProviderMessages_Empty(t *testing.T) {
	result := toProviderMessages(nil)
	// 注意：源码对 nil 输入返回空切片 []（非 nil），这是 Go make() 的正常行为
	if len(result) != 0 {
		t.Errorf("expected empty slice, got %d items", len(result))
	}
}

func TestToProviderTools(t *testing.T) {
	tools := []Tool{
		{
			Type:     "function",
			Function: ToolFunc{Name: "get_weather", Description: "Get weather", Parameters: map[string]interface{}{"type": "object"}},
		},
	}
	result := toProviderTools(tools)
	if len(result) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(result))
	}
	if result[0].Function.Name != "get_weather" {
		t.Errorf("unexpected tool name: %s", result[0].Function.Name)
	}
}

func TestToProviderTools_Empty(t *testing.T) {
	result := toProviderTools(nil)
	if result != nil {
		t.Errorf("expected nil, got %v", result)
	}
}

func TestToProviderMessagesFromMap(t *testing.T) {
	msgs := []map[string]interface{}{
		{"role": "user", "content": "Hello"},
		{"role": "assistant", "content": "Hi"},
	}
	result := toProviderMessagesFromMap(msgs)
	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result))
	}
	if result[0].Role != "user" || result[0].Content != "Hello" {
		t.Errorf("unexpected message[0]: %+v", result[0])
	}
}

func TestToProviderMessagesFromMap_WithContentBlocks(t *testing.T) {
	// 测试 Anthropic 格式的 content 为数组（含多个 text blocks）
	msgs := []map[string]interface{}{
		{
			"role": "user",
			"content": []interface{}{
				map[string]interface{}{"type": "text", "text": "Hello"},
				map[string]interface{}{"type": "text", "text": " World"},
			},
		},
	}
	result := toProviderMessagesFromMap(msgs)
	if len(result) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result))
	}
	// 多个 text blocks 应该拼接成一个字符串
	if result[0].Content != "Hello World" {
		t.Errorf("expected 'Hello World', got '%s'", result[0].Content)
	}
}

func TestToProviderMessagesFromMap_SkipsNoRole(t *testing.T) {
	// 没有 role 字段的消息应该被跳过
	msgs := []map[string]interface{}{
		{"content": "no role here"},
		{"role": "user", "content": "valid"},
	}
	result := toProviderMessagesFromMap(msgs)
	if len(result) != 1 {
		t.Fatalf("expected 1 message (skipped no-role), got %d", len(result))
	}
}

func TestToProviderMessagesFromMap_NoContent(t *testing.T) {
	// 没有 content 字段的消息，Content 应为空字符串
	msgs := []map[string]interface{}{
		{"role": "user"},
	}
	result := toProviderMessagesFromMap(msgs)
	if len(result) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result))
	}
	if result[0].Content != "" {
		t.Errorf("expected empty content, got '%s'", result[0].Content)
	}
}

func TestToolsFromAnthropicRequest(t *testing.T) {
	tools := []map[string]interface{}{
		{
			"type":         "function",
			"name":         "get_weather",
			"description":  "Get weather",
			"input_schema": map[string]interface{}{"type": "object"},
		},
	}
	result := toolsFromAnthropicRequest(tools)
	if len(result) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(result))
	}
	if result[0].Function.Name != "get_weather" {
		t.Errorf("unexpected tool name: %s", result[0].Function.Name)
	}
}

func TestToolsFromAnthropicRequest_Empty(t *testing.T) {
	result := toolsFromAnthropicRequest(nil)
	if result != nil {
		t.Errorf("expected nil, got %v", result)
	}
}

func TestToolsFromAnthropicRequest_NoInputSchema(t *testing.T) {
	// 没有 input_schema 的 tool 定义（旧格式）
	tools := []map[string]interface{}{
		{
			"name":        "get_weather",
			"description": "Get weather",
		},
	}
	result := toolsFromAnthropicRequest(tools)
	if len(result) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(result))
	}
	// 旧格式没有 input_schema，所以 Parameters 为 nil
	if result[0].Function.Parameters != nil {
		t.Errorf("expected nil parameters for old format, got %v", result[0].Function.Parameters)
	}
}

// ==================== 辅助函数测试：ContentToBlocks ====================

func TestContentToBlocks_String(t *testing.T) {
	converter := provider.NewAnthropicConverter()
	result := converter.ContentToBlocks("plain text")
	if len(result) != 1 || result[0]["type"] != "text" {
		t.Errorf("unexpected result: %v", result)
	}
	if result[0]["text"] != "plain text" {
		t.Errorf("unexpected text: %v", result[0]["text"])
	}
}

func TestContentToBlocks_EmptyArray(t *testing.T) {
	converter := provider.NewAnthropicConverter()
	empty := make([]interface{}, 0)
	result := converter.ContentToBlocks(empty)
	if len(result) != 1 || result[0]["type"] != "text" {
		t.Errorf("expected single empty text block for empty array, got: %v", result)
	}
}

func TestContentToBlocks_Nil(t *testing.T) {
	converter := provider.NewAnthropicConverter()
	result := converter.ContentToBlocks(nil)
	if len(result) != 1 {
		t.Errorf("expected single text block for nil, got: %v", result)
	}
}

func TestContentToBlocks_NilInArray(t *testing.T) {
	converter := provider.NewAnthropicConverter()
	input := []interface{}{"hello", nil, "world"}
	result := converter.ContentToBlocks(input)
	// nil 应该被跳过
	if len(result) != 2 {
		t.Errorf("expected 2 blocks (nil skipped), got %d", len(result))
	}
}

// ==================== 辅助函数测试：ContentToBlocks 各种类型 ====================

func TestContentToBlocks_Float(t *testing.T) {
	converter := provider.NewAnthropicConverter()
	result := converter.ContentToBlocks(3.14)
	if len(result) != 1 {
		t.Fatalf("expected 1 block for float, got %d", len(result))
	}
}

func TestContentToBlocks_Boolean(t *testing.T) {
	converter := provider.NewAnthropicConverter()
	result := converter.ContentToBlocks(true)
	if len(result) != 1 {
		t.Fatalf("expected 1 block for bool, got %d", len(result))
	}
}

func TestContentToBlocks_SingleMap(t *testing.T) {
	converter := provider.NewAnthropicConverter()
	input := map[string]interface{}{"type": "image", "source": "base64"}
	result := converter.ContentToBlocks(input)
	if len(result) != 1 {
		t.Fatalf("expected 1 block for single map, got %d", len(result))
	}
}

// ==================== 辅助函数测试：ContentToBlocks 补充字段 ====================

func TestContentToBlocks_ToolUseMissingFields(t *testing.T) {
	converter := provider.NewAnthropicConverter()
	blocks := []interface{}{
		map[string]interface{}{"type": "tool_use"},
	}
	result := converter.ContentToBlocks(blocks)
	if len(result) != 1 {
		t.Fatalf("expected 1 block, got %d", len(result))
	}
	// 应该补充缺失的必需字段
	if result[0]["id"] == nil || result[0]["name"] == nil || result[0]["input"] == nil {
		t.Errorf("expected missing fields to be filled, got: %v", result[0])
	}
}

func TestContentToBlocks_TextMessageMissingText(t *testing.T) {
	converter := provider.NewAnthropicConverter()
	blocks := []interface{}{
		map[string]interface{}{"type": "text"},
	}
	result := converter.ContentToBlocks(blocks)
	if len(result) != 1 {
		t.Fatalf("expected 1 block, got %d", len(result))
	}
	if result[0]["text"] == nil {
		t.Errorf("expected 'text' field to be filled, got: %v", result[0])
	}
}

// ==================== 辅助函数测试：ContentToBlocks 混合内容 ====================

func TestContentToBlocks_MixedStringAndMap(t *testing.T) {
	converter := provider.NewAnthropicConverter()
	input := []interface{}{
		"Hello",
		map[string]interface{}{"type": "text", "text": "World"},
	}
	result := converter.ContentToBlocks(input)
	if len(result) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(result))
	}
}

// ==================== 辅助函数测试：ConvertAnthropicMessagesToOpenAI ====================

func TestConvertAnthropicMessagesToOpenAI_SysMessage(t *testing.T) {
	converter := provider.NewAnthropicConverter()
	messages := []map[string]interface{}{
		{"role": "user", "content": "Hi"},
	}
	system := "You are a helpful assistant."
	result, tools := converter.ConvertAnthropicMessagesToOpenAI(messages, system, nil)

	if len(result) < 1 {
		t.Fatalf("expected at least 1 message, got %d", len(result))
	}
	// 第一条应该是 system 消息
	if result[0].Role != "system" {
		t.Errorf("expected first message to be system, got %s", result[0].Role)
	}
	if result[0].Content != "You are a helpful assistant." {
		t.Errorf("unexpected system content: %s", result[0].Content)
	}
	if len(tools) != 0 {
		t.Errorf("expected no tools, got %d", len(tools))
	}
}

func TestConvertAnthropicMessagesToOpenAI_UserMessages(t *testing.T) {
	converter := provider.NewAnthropicConverter()
	messages := []map[string]interface{}{
		{"role": "user", "content": "Hello"},
		{"role": "assistant", "content": "Hi back"},
	}
	result, _ := converter.ConvertAnthropicMessagesToOpenAI(messages, nil, nil)

	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result))
	}
	if result[0].Role != "user" || result[0].Content != "Hello" {
		t.Errorf("unexpected message[0]: %+v", result[0])
	}
	if result[1].Role != "assistant" || result[1].Content != "Hi back" {
		t.Errorf("unexpected message[1]: %+v", result[1])
	}
}

func TestConvertAnthropicMessagesToOpenAI_MixedContent(t *testing.T) {
	converter := provider.NewAnthropicConverter()
	// content 是数组（包含多个 text blocks）
	messages := []map[string]interface{}{
		{"role": "user", "content": []interface{}{
			map[string]interface{}{"type": "text", "text": "Part1"},
			map[string]interface{}{"type": "text", "text": "Part2"},
		}},
	}
	result, _ := converter.ConvertAnthropicMessagesToOpenAI(messages, nil, nil)

	// 多个 text blocks 应该拼接成一个字符串
	if result[0].Content != "Part1Part2" {
		t.Errorf("expected 'Part1Part2', got '%s'", result[0].Content)
	}
}

func TestConvertAnthropicMessagesToOpenAI_WithTools(t *testing.T) {
	converter := provider.NewAnthropicConverter()
	messages := []map[string]interface{}{
		{"role": "user", "content": "Weather?"},
	}
	tools := []map[string]interface{}{
		{"name": "get_weather", "description": "Get weather", "input_schema": map[string]interface{}{"type": "object"}},
	}
	_, result := converter.ConvertAnthropicMessagesToOpenAI(messages, nil, tools)

	if len(result) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(result))
	}
	if result[0].Function.Name != "get_weather" {
		t.Errorf("unexpected tool name: %s", result[0].Function.Name)
	}
}

// ==================== 辅助函数测试：ConvertAnthropicMessagesToOpenAI 系统消息处理 ====================

func TestConvertAnthropicMessagesToOpenAI_EmptySystem(t *testing.T) {
	converter := provider.NewAnthropicConverter()
	messages := []map[string]interface{}{
		{"role": "user", "content": "Hi"},
	}
	result, _ := converter.ConvertAnthropicMessagesToOpenAI(messages, "", nil)

	// 空字符串系统消息不应产生 system message
	for _, msg := range result {
		if msg.Role == "system" {
			t.Errorf("expected no system message for empty system, got: %+v", msg)
		}
	}
}

func TestConvertAnthropicMessagesToOpenAI_SystemAsBlocks(t *testing.T) {
	converter := provider.NewAnthropicConverter()
	messages := []map[string]interface{}{
		{"role": "user", "content": "Hi"},
	}
	// system 是一个 blocks 数组
	system := []interface{}{
		map[string]interface{}{"type": "text", "text": "Be helpful."},
	}
	result, _ := converter.ConvertAnthropicMessagesToOpenAI(messages, system, nil)

	if len(result) < 1 || result[0].Role != "system" {
		t.Errorf("expected first message to be system, got: %v", result)
	}
}

// ==================== 辅助函数测试：ConvertAnthropicMessagesToOpenAI 角色过滤 ====================

func TestConvertAnthropicMessagesToOpenAI_FilteredRoles(t *testing.T) {
	converter := provider.NewAnthropicConverter()
	// 包含 system 角色（除了 user/assistant 外的角色应被跳过）
	messages := []map[string]interface{}{
		{"role": "user", "content": "Hello"},
		{"role": "tool", "content": "result"}, // 应该被过滤掉
		{"role": "assistant", "content": "Hi"},
	}
	result, _ := converter.ConvertAnthropicMessagesToOpenAI(messages, nil, nil)

	// tool 角色应该被跳过
	if len(result) != 2 {
		t.Errorf("expected 2 messages (tool filtered), got %d", len(result))
	}
}

// ==================== 辅助函数测试：ConvertOpenAIToAnthropicResponse ====================

func TestConvertOpenAIToAnthropicResponse_Basic(t *testing.T) {
	converter := provider.NewAnthropicConverter()
	openAIResp := []byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)

	result, err := converter.ConvertOpenAIToAnthropicResponse(openAIResp, "virtual-model", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 验证响应包含 Anthropic 格式字段
	if !strings.Contains(string(result), `"type":"message"`) {
		t.Errorf("expected Anthropic response type, got: %s", string(result))
	}
	if !strings.Contains(string(result), `"role":"assistant"`) {
		t.Errorf("expected role: assistant, got: %s", string(result))
	}
	if !strings.Contains(string(result), `"model":"virtual-model"`) {
		t.Errorf("expected virtual-model, got: %s", string(result))
	}
}

func TestConvertOpenAIToAnthropicResponse_NoChoice(t *testing.T) {
	converter := provider.NewAnthropicConverter()
	// 空 choices 的响应
	openAIResp := []byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)

	result, err := converter.ConvertOpenAIToAnthropicResponse(openAIResp, "virtual-model", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 应返回 stop_reason = end_turn
	if !strings.Contains(string(result), `"stop_reason":"end_turn"`) {
		t.Errorf("expected end_turn, got: %s", string(result))
	}
}

// ==================== 辅助函数测试：ConvertMessagesToAnthropic ====================

func TestConvertMessagesToAnthropic(t *testing.T) {
	converter := provider.NewAnthropicConverter()
	messages := []provider.Message{
		{Role: "user", Content: "Hello"},
		{Role: "assistant", Content: "Hi back"},
	}
	result, system := converter.ConvertMessagesToAnthropic(messages)

	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result))
	}
	if result[0]["role"] != "user" {
		t.Errorf("unexpected role[0]: %v", result[0]["role"])
	}
	if result[0]["content"] == nil {
		t.Error("expected content to be non-nil array")
	}
	if system != nil {
		t.Errorf("expected nil system, got %v", system)
	}
}

func TestConvertMessagesToAnthropic_WithToolCalls(t *testing.T) {
	converter := provider.NewAnthropicConverter()
	messages := []provider.Message{
		{Role: "user", Content: "What's the weather in Beijing?"},
		{
			Role:    "assistant",
			Content: "",
			ToolCalls: []map[string]interface{}{
				{
					"id":   "call_1",
					"type": "function",
					"function": map[string]interface{}{
						"name":      "get_weather",
						"arguments": `{"location":"beijing"}`,
					},
				},
			},
		},
		{Role: "tool", Content: "sunny, 25C", ToolCallID: "call_1"},
	}

	result, system := converter.ConvertMessagesToAnthropic(messages)
	if len(result) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(result))
	}

	// 第 2 条（assistant）应含 tool_use block
	asst, ok := result[1]["content"].([]map[string]interface{})
	if !ok || len(asst) != 1 {
		t.Fatalf("expected assistant content with 1 block, got %+v", result[1]["content"])
	}
	if asst[0]["type"] != "tool_use" {
		t.Errorf("expected tool_use block, got %v", asst[0]["type"])
	}
	if asst[0]["name"] != "get_weather" {
		t.Errorf("unexpected tool name: %v", asst[0]["name"])
	}

	// 第 3 条（role:tool）应转为 user + tool_result block
	if result[2]["role"] != "user" {
		t.Errorf("expected role user for tool result, got %v", result[2]["role"])
	}
	tr, ok := result[2]["content"].([]map[string]interface{})
	if !ok || len(tr) != 1 || tr[0]["type"] != "tool_result" {
		t.Fatalf("expected tool_result block, got %+v", result[2]["content"])
	}
	if tr[0]["tool_use_id"] != "call_1" {
		t.Errorf("unexpected tool_use_id: %v", tr[0]["tool_use_id"])
	}
	if system != nil {
		t.Errorf("expected nil system, got %v", system)
	}
}

func TestConvertMessagesToAnthropic_WithSystem(t *testing.T) {
	converter := provider.NewAnthropicConverter()
	messages := []provider.Message{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "Hello"},
		{Role: "system", Content: "Be concise."},
		{Role: "assistant", Content: "Hi!"},
	}

	result, system := converter.ConvertMessagesToAnthropic(messages)

	// 两条 system 消息被抽走，不应再出现在 messages 中
	if len(result) != 2 {
		t.Fatalf("expected 2 messages (system removed), got %d", len(result))
	}
	for _, m := range result {
		if m["role"] == "system" {
			t.Errorf("system role must not appear in messages: %+v", m)
		}
	}

	// system 应多条合并为单一字符串，作为 Anthropic 顶层参数
	sysStr, ok := system.(string)
	if !ok || sysStr != "You are a helpful assistant.\n\nBe concise." {
		t.Errorf("expected merged system string, got %v", system)
	}
}

func TestConvertMessagesToOpenAI(t *testing.T) {
	converter := provider.NewAnthropicConverter()
	// Anthropic 格式的 content 为数组
	messages := []map[string]interface{}{
		{"role": "user", "content": []interface{}{
			map[string]interface{}{"type": "text", "text": "Hello"},
		}},
		{"role": "assistant", "content": "Hi"},
	}
	result := converter.ConvertMessagesToOpenAI(messages, nil)

	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result))
	}
}

func TestConvertMessagesToOpenAI_SystemParameter(t *testing.T) {
	converter := provider.NewAnthropicConverter()
	messages := []map[string]interface{}{
		{"role": "user", "content": "Hi"},
	}
	result := converter.ConvertMessagesToOpenAI(messages, "You are helpful.")

	if len(result) < 1 || result[0].Role != "system" {
		t.Errorf("expected first message to be system, got: %v", result)
	}
}

// ==================== 辅助函数测试：ConvertTools ====================

func TestConvertTools(t *testing.T) {
	converter := provider.NewAnthropicConverter()
	tools := []provider.Tool{
		{
			Type: "function",
			Function: provider.ToolFunc{
				Name:        "get_weather",
				Description: "Get weather",
				Parameters:  map[string]interface{}{"type": "object"},
			},
		},
	}
	result := converter.ConvertTools(tools)

	if len(result) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(result))
	}
	if result[0]["name"] != "get_weather" {
		t.Errorf("unexpected tool name: %v", result[0]["name"])
	}
	if result[0]["input_schema"] == nil {
		t.Error("expected input_schema to be present")
	}
}

func TestConvertTools_Empty(t *testing.T) {
	converter := provider.NewAnthropicConverter()
	result := converter.ConvertTools(nil)
	if len(result) != 0 {
		t.Errorf("expected empty result, got %d", len(result))
	}
}

// ==================== 辅助函数测试：FlattenContent ====================

func TestFlattenContent_String(t *testing.T) {
	converter := provider.NewAnthropicConverter()
	result := converter.FlattenContent("plain text")
	if result != "plain text" {
		t.Errorf("expected 'plain text', got '%s'", result)
	}
}

func TestFlattenContent_Array(t *testing.T) {
	converter := provider.NewAnthropicConverter()
	input := []interface{}{
		map[string]interface{}{"type": "text", "text": "Hello"},
		map[string]interface{}{"type": "text", "text": " World"},
	}
	result := converter.FlattenContent(input)
	if result != "Hello World" {
		t.Errorf("expected 'Hello World', got '%s'", result)
	}
}

func TestFlattenContent_Nil(t *testing.T) {
	converter := provider.NewAnthropicConverter()
	result := converter.FlattenContent(nil)
	if result != "" {
		t.Errorf("expected empty string, got '%s'", result)
	}
}

// ==================== 辅助函数测试：Request 和 Result 结构 ====================

func TestRequest_StructFields(t *testing.T) {
	req := Request{
		ClientProtocol: provider.ProtocolOpenAI,
		ChatReq: &ChatCompletionRequest{
			Model:    "gpt-4",
			Messages: []Message{{Role: "user", Content: "Hi"}},
			Stream:   true,
		},
		IsStream:     true,
		VirtualModel: "gpt-4-virtual",
		Ctx:          context.Background(),
	}

	if req.ClientProtocol != provider.ProtocolOpenAI {
		t.Errorf("unexpected protocol: %v", req.ClientProtocol)
	}
	if !req.IsStream {
		t.Error("expected IsStream to be true")
	}
	if req.VirtualModel != "gpt-4-virtual" {
		t.Errorf("unexpected VirtualModel: %s", req.VirtualModel)
	}
}

func TestRequest_AnthropicFields(t *testing.T) {
	req := Request{
		ClientProtocol: provider.ProtocolAnthropic,
		AnthropicReq: &AnthropicRequest{
			Model:     "claude-3",
			Messages:  []map[string]interface{}{{"role": "user", "content": "Hi"}},
			MaxTokens: 4096,
			Stream:    false,
		},
		IsStream:    false,
		ExtraParams: map[string]interface{}{"temperature": 0.7},
		Ctx:         context.Background(),
	}

	if req.ClientProtocol != provider.ProtocolAnthropic {
		t.Errorf("unexpected protocol: %v", req.ClientProtocol)
	}
	if req.ExtraParams["temperature"] != 0.7 {
		t.Errorf("expected temperature 0.7, got %v", req.ExtraParams["temperature"])
	}
}

func TestResult_StructFields(t *testing.T) {
	result := Result{
		StatusCode: 200,
		Body:       []byte(`{"id":"msg-1"}`),
	}

	if result.StatusCode != 200 {
		t.Errorf("unexpected StatusCode: %d", result.StatusCode)
	}
	if len(result.Body) == 0 {
		t.Error("expected non-empty Body")
	}
}

func TestResult_StreamFields(t *testing.T) {
	result := Result{
		StatusCode: 200,
		StreamBody: io.NopCloser(strings.NewReader("test")),
	}

	if result.StreamBody == nil {
		t.Error("expected non-nil StreamBody")
	}
}

// ==================== 辅助函数测试：各种 Message 类型 ====================

func TestMessage_Roles(t *testing.T) {
	roles := []string{"user", "assistant", "system"}
	for _, role := range roles {
		msg := Message{Role: role, Content: "test"}
		if msg.Role != role {
			t.Errorf("unexpected role: %s", msg.Role)
		}
	}
}

func TestTool_FunctionFields(t *testing.T) {
	tool := Tool{
		Type:     "function",
		Function: ToolFunc{Name: "weather", Description: "Get weather info", Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}},
	}

	if tool.Type != "function" {
		t.Errorf("unexpected type: %s", tool.Type)
	}
	if tool.Function.Name != "weather" {
		t.Errorf("unexpected function name: %s", tool.Function.Name)
	}
}

func TestToolCall_Struct(t *testing.T) {
	tc := ToolCall{
		ID:   "call_123",
		Type: "function",
		Function: ToolCallFunc{
			Name:      "get_weather",
			Arguments: `{"location":"beijing"}`,
		},
	}

	if tc.ID != "call_123" {
		t.Errorf("unexpected ID: %s", tc.ID)
	}
	if tc.Function.Name != "get_weather" {
		t.Errorf("unexpected function name: %s", tc.Function.Name)
	}
}

func TestChoice_Struct(t *testing.T) {
	choice := Choice{
		Index:        0,
		FinishReason: "stop",
		Message:      Message{Role: "assistant", Content: "Hello"},
	}

	if choice.Index != 0 {
		t.Errorf("unexpected index: %d", choice.Index)
	}
	if choice.FinishReason != "stop" {
		t.Errorf("unexpected finish_reason: %s", choice.FinishReason)
	}
}

func TestUsage_Struct(t *testing.T) {
	usage := Usage{
		PromptTokens:     10,
		CompletionTokens: 5,
		TotalTokens:      15,
	}

	if usage.PromptTokens != 10 || usage.CompletionTokens != 5 || usage.TotalTokens != 15 {
		t.Errorf("unexpected usage values: %+v", usage)
	}
}

func TestChatCompletionRequest_StreamFields(t *testing.T) {
	req := ChatCompletionRequest{
		Model:       "gpt-4",
		MaxTokens:   1024,
		Temperature: 0.7,
		TopP:        0.9,
	}

	if req.Model != "gpt-4" {
		t.Errorf("unexpected model: %s", req.Model)
	}
	if req.MaxTokens != 1024 {
		t.Errorf("unexpected max_tokens: %d", req.MaxTokens)
	}
	if req.Temperature != 0.7 {
		t.Errorf("unexpected temperature: %f", req.Temperature)
	}
	if req.TopP != 0.9 {
		t.Errorf("unexpected top_p: %f", req.TopP)
	}
}

func TestChatCompletionRequest_WithStream(t *testing.T) {
	req := ChatCompletionRequest{
		Model:    "gpt-4",
		Messages: []Message{{Role: "user", Content: "Hi"}},
		Stream:   true,
	}

	if !req.Stream {
		t.Error("expected Stream to be true")
	}
}

func TestChatCompletionRequest_WithTools(t *testing.T) {
	req := ChatCompletionRequest{
		Model:    "gpt-4",
		Messages: []Message{{Role: "user", Content: "Hi"}},
		Tools: []Tool{
			{Type: "function", Function: ToolFunc{Name: "test"}},
		},
	}

	if len(req.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(req.Tools))
	}
}

func TestAnthropicRequest_Fields(t *testing.T) {
	req := AnthropicRequest{
		Model:         "claude-3",
		Messages:      []map[string]interface{}{{"role": "user", "content": "Hi"}},
		MaxTokens:     4096,
		Temperature:   0.7,
		TopP:          0.9,
		StopSequences: []string{"\nHuman:", "\nAI:"},
		Tools: []map[string]interface{}{
			{"name": "weather", "description": "Get weather"},
		},
		ToolChoice: map[string]interface{}{"type": "auto"},
	}

	if req.Model != "claude-3" {
		t.Errorf("unexpected model: %s", req.Model)
	}
	if req.MaxTokens != 4096 {
		t.Errorf("unexpected max_tokens: %d", req.MaxTokens)
	}
	if len(req.StopSequences) != 2 {
		t.Errorf("expected 2 stop sequences, got %d", len(req.StopSequences))
	}
}

func TestAnthropicRequest_OmitEmptyFields(t *testing.T) {
	req := AnthropicRequest{
		Model:     "claude-3",
		Messages:  []map[string]interface{}{{"role": "user", "content": "Hi"}},
		MaxTokens: 4096,
		Stream:    true,
	}

	// 不设置 Temperature, TopP, StopSequences, Tools, ToolChoice
	if req.Temperature != 0 {
		t.Errorf("expected 0 temperature, got %f", req.Temperature)
	}
	if req.TopP != 0 {
		t.Errorf("expected 0 top_p, got %f", req.TopP)
	}
}
