package protocol

// ChatCompletionRequest OpenAI 兼容请求格式
type ChatCompletionRequest struct {
	Model       string    `json:"model" binding:"required"`
	Messages    []Message `json:"messages" binding:"required"`
	Stream      bool      `json:"stream,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Temperature float64   `json:"temperature,omitempty"`
	TopP        float64   `json:"top_p,omitempty"`
	Tools       []Tool    `json:"tools,omitempty"`
}

// Message 消息
type Message struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// Tool 工具定义
type Tool struct {
	Type     string `json:"type"`
	Function ToolFunc `json:"function"`
}

// ToolFunc 工具函数定义（请求侧，来自 config 或 API 定义）
type ToolFunc struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters,omitempty"`
}

// ToolCallFunc 工具调用返回时的函数信息（响应侧，OpenAI SSE 格式）
type ToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolCall 工具调用（响应格式）
type ToolCall struct {
	ID       string        `json:"id"`
	Type     string        `json:"type"`
	Function ToolCallFunc  `json:"function"`
}

// ChatCompletionResponse OpenAI 兼容响应格式
type ChatCompletionResponse struct {
	ID        string   `json:"id"`
	Object    string   `json:"object"`
	Created   int64    `json:"created"`
	Model     string   `json:"model"`
	Choices   []Choice `json:"choices"`
	Usage     Usage    `json:"usage"`
	ToolCalls []Choice `json:"tool_calls,omitempty"` // 兼容 Anthropic 响应
}

// Choice 选择项
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// Usage 用量
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// AnthropicRequest Anthropic Messages API 请求格式
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
