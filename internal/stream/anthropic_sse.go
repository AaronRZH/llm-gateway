package stream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// OpenAIStreamEvent OpenAI SSE chunk 的解析结构
type OpenAIStreamEvent struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int `json:"index"`
		Delta        struct {
			Role      string                   `json:"role,omitempty"`
			Content   string                   `json:"content,omitempty"`
			ToolCalls []map[string]interface{} `json:"tool_calls,omitempty"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason,omitempty"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage,omitempty"`
}

// AnthropicSSEConverterState converter 内部状态
type AnthropicSSEConverterState int

const (
	stateIdle      AnthropicSSEConverterState = iota // 等待开始
	stateStarted                                      // message_start 已发送
	stateStreaming                                    // 正在接收 content delta
	stateTool                                         // 正在接收 tool_use delta
	stateDone                                         // 已结束
)

// AnthropicSSEConverter 将 OpenAI SSE 流实时转换为 Anthropic SSE 流
// 实现 io.ReadCloser，可直接作为 handler 的上游使用
type AnthropicSSEConverter struct {
	// pipe 内部管道：converter 写入，handler 读取
	pr *io.PipeReader
	pw *io.PipeWriter

	// upstream 上游 SSE reader
	upstream io.ReadCloser

	// 状态
	state       AnthropicSSEConverterState
	model       string // 虚拟模型名
	hasContent  bool   // 是否已发送 content_block_start
	promptTokens int
	usageDone   bool

	// wg 等待 convert goroutine 完成
	wg sync.WaitGroup
}

// NewAnthropicSSEConverter 创建转换器
func NewAnthropicSSEConverter(upstream io.ReadCloser, virtualModel string) *AnthropicSSEConverter {
	pr, pw := io.Pipe()
	c := &AnthropicSSEConverter{
		pr:       pr,
		pw:       pw,
		upstream: upstream,
		state:    stateIdle,
		model:    virtualModel,
	}

	c.wg.Add(1)
	go c.convert()
	return c
}

// Read 实现 io.ReadCloser
func (c *AnthropicSSEConverter) Read(p []byte) (n int, err error) {
	return c.pr.Read(p)
}

// Close 实现 io.ReadCloser
// 等待 convert goroutine 完成后再关闭 pipe 读取端，确保所有数据都被消费完。
// upstream 的关闭由 convert() 的 defer 负责。
func (c *AnthropicSSEConverter) Close() error {
	c.wg.Wait()
	return c.pr.Close()
}

// convert 在 goroutine 中运行，读取上游 OpenAI SSE 并转换为 Anthropic SSE
func (c *AnthropicSSEConverter) convert() {
	defer c.pw.Close()
	defer c.upstream.Close()
	defer c.wg.Done()

	scanner := bufio.NewScanner(c.upstream)
	scanner.Split(bufio.ScanLines)

	// 用于累积 tool_use 信息
	var toolID string
	var toolName string
	var toolInputBuf strings.Builder
	inTool := false

	for scanner.Scan() {
		line := scanner.Bytes()

		// SSE 分隔符，忽略
		if len(line) == 0 {
			continue
		}

		// 非 data: 行，忽略
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}

		payload := line[6:] // 去掉 "data: "

		// [DONE] 标记
		if bytes.Equal(payload, []byte("[DONE]")) {
			c.finish(true, inTool, toolID, toolName, toolInputBuf.String())
			continue
		}

		// 解析 OpenAI SSE chunk
		var event OpenAIStreamEvent
		if err := json.Unmarshal(payload, &event); err != nil {
			continue
		}

		if len(event.Choices) == 0 {
			continue
		}

		choice := event.Choices[0]
		delta := choice.Delta

		// === 处理 role 字段：message_start + content_block_start ===
		if delta.Role == "assistant" && c.state == stateIdle {
			c.state = stateStarted

			// message_start
			msgStart := map[string]interface{}{
				"type": "message_start",
				"message": map[string]interface{}{
					"id":      "msg_" + uuid.New().String()[:12],
					"type":    "message",
					"role":    "assistant",
					"content": []interface{}{},
					"model":   c.model,
				},
			}
			writeSSE(c.pw, msgStart)
		}

		// === 处理 tool_calls ===
		if len(delta.ToolCalls) > 0 {
			for _, tc := range delta.ToolCalls {
				idx, _ := tc["index"].(float64)
				fnRaw, hasFn := tc["function"].(map[string]interface{})

				if int(idx) == 0 && !inTool {
					// 关闭 text content block（如果有）
					if c.hasContent {
						writeSSE(c.pw, map[string]interface{}{
							"type":  "content_block_stop",
							"index": 0,
						})
						c.hasContent = false
					}

					// 开始 tool_use content block
					if id, ok := tc["id"].(string); ok {
						toolID = id
					} else {
						toolID = "toolu_" + uuid.New().String()[:12]
					}
					if hasFn {
						toolName, _ = fnRaw["name"].(string)
					}
					toolInputBuf.Reset()
					inTool = true

					c.state = stateTool
					writeSSE(c.pw, map[string]interface{}{
						"type":  "content_block_start",
						"index": 1,
						"content_block": map[string]interface{}{
							"type":  "tool_use",
							"id":    toolID,
							"name":  toolName,
							"input": map[string]interface{}{},
						},
					})
				}

				// tool_use input_json_delta
				if hasFn {
					if args, ok := fnRaw["arguments"].(string); ok {
						toolInputBuf.WriteString(args)
						writeSSE(c.pw, map[string]interface{}{
							"type":  "content_block_delta",
							"index": 1,
							"delta": map[string]interface{}{
								"type":        "input_json_delta",
								"partial_json": args,
							},
						})
					}
				}
			}

			// 如果有 finish_reason，在 tool_calls 行处理结束
			if choice.FinishReason != "" {
				c.finishTool(inTool, toolID, toolName, toolInputBuf.String())
				inTool = false
				continue
			}
		}

		// === 处理 content delta ===
		if delta.Content != "" {
			if !c.hasContent {
				c.hasContent = true
				// content_block_start（text）
				writeSSE(c.pw, map[string]interface{}{
					"type":  "content_block_start",
					"index": 0,
					"content_block": map[string]interface{}{
						"type": "text",
						"text": "",
					},
				})
			}

			// content_block_delta（text_delta）
			writeSSE(c.pw, map[string]interface{}{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]interface{}{
					"type": "text_delta",
					"text": delta.Content,
				},
			})
		}

		// === 处理 finish_reason ===
		if choice.FinishReason != "" {
			c.finish(c.hasContent || inTool, inTool, toolID, toolName, toolInputBuf.String())
		}

		// === 处理 usage ===
		if event.Usage != nil && !c.usageDone {
			c.promptTokens = event.Usage.PromptTokens
			c.usageDone = true
		}
	}

	// 扫描结束，确保完成
	if c.state != stateDone {
		c.finish(c.hasContent || inTool, inTool, toolID, toolName, toolInputBuf.String())
	}
}

// finish 关闭所有打开的 block，发送 message_delta 和 message_stop
func (c *AnthropicSSEConverter) finish(hasOpenBlock bool, inTool bool, toolID, toolName, toolInput string) {
	if c.state == stateDone {
		return
	}

	// 关闭 text content block
	if hasOpenBlock && !inTool {
		writeSSE(c.pw, map[string]interface{}{
			"type":  "content_block_stop",
			"index": 0,
		})
	}

	// 关闭 tool_use content block
	if inTool {
		writeSSE(c.pw, map[string]interface{}{
			"type":  "content_block_stop",
			"index": 1,
		})
	}

	// message_delta
	usage := map[string]interface{}{
		"output_tokens": 0,
	}
	if c.promptTokens > 0 {
		usage["input_tokens"] = c.promptTokens
	}
	writeSSE(c.pw, map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{
			"stop_reason":   "end_turn",
			"stop_sequence": nil,
		},
		"usage": usage,
	})

	// message_stop
	writeSSE(c.pw, map[string]interface{}{
		"type": "message_stop",
	})

	c.state = stateDone
}

// finishTool 关闭 tool_use content block，不发送 message_delta/message_stop
func (c *AnthropicSSEConverter) finishTool(inTool bool, toolID, toolName, toolInput string) {
	if inTool {
		writeSSE(c.pw, map[string]interface{}{
			"type":  "content_block_stop",
			"index": 1,
		})
	}
}

// ==================== OpenAIStreamConverter (Anthropic SSE → OpenAI SSE) ====================

// OpenAIStreamState openAI 转换器内部状态
type OpenAIStreamState int

const (
	oaiStateIdle      OpenAIStreamState = iota // 等待开始
	oaiStateStarted                            // message_start 已处理
	oaiStateStreaming                            // 正在处理 text content delta
	oaiStateTool                                 // 正在处理 tool_use delta
	oaiStateDone                                 // 已结束
)

// OpenAIStreamConverter 将 Anthropic SSE 流实时转换为 OpenAI SSE 流
// 实现 io.ReadCloser，可直接作为 handler 的上游使用
type OpenAIStreamConverter struct {
	pr *io.PipeReader
	pw *io.PipeWriter

	// upstream 上游 SSE reader（来自 Anthropic 上游的 SSE 流）
	upstream io.ReadCloser

	// 状态
	state        OpenAIStreamState
	model        string // 虚拟模型名
	promptTokens int

	// tool_use 累积
	inTool   bool
	toolID   string
	toolName string
	toolInput strings.Builder

	// id 和 created 用于全链路一致性
	streamID string
	created  int64

	// wg 等待 convert goroutine 完成
	wg sync.WaitGroup
}

// NewOpenAIStreamConverter 创建 Anthropic→OpenAI SSE 转换器
func NewOpenAIStreamConverter(upstream io.ReadCloser, virtualModel string) *OpenAIStreamConverter {
	pr, pw := io.Pipe()
	id := uuid.New().String()[:12]
	c := &OpenAIStreamConverter{
		pr:       pr,
		pw:       pw,
		upstream: upstream,
		state:    oaiStateIdle,
		model:    virtualModel,
		streamID: id,
		created:  time.Now().Unix(),
	}

	c.wg.Add(1)
	go c.convert()
	return c
}

// Read 实现 io.ReadCloser
func (c *OpenAIStreamConverter) Read(p []byte) (n int, err error) {
	return c.pr.Read(p)
}

// Close 实现 io.ReadCloser
func (c *OpenAIStreamConverter) Close() error {
	c.wg.Wait()
	return c.pr.Close()
}

// convert 在 goroutine 中运行，读取上游 Anthropic SSE 并转换为 OpenAI SSE
func (c *OpenAIStreamConverter) convert() {
	defer c.pw.Close()
	defer c.upstream.Close()
	defer c.wg.Done()

	scanner := bufio.NewScanner(c.upstream)
	scanner.Split(bufio.ScanLines)

	for scanner.Scan() {
		line := scanner.Bytes()

		// SSE 分隔符，忽略
		if len(line) == 0 {
			continue
		}

		// 非 data: 行，忽略
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}

		payload := line[6:] // 去掉 "data: "

		// 解析 Anthropic SSE event
		var event map[string]interface{}
		if err := json.Unmarshal(payload, &event); err != nil {
			continue
		}

		eventType, _ := event["type"].(string)

		switch eventType {
		case "message_start":
			c.state = oaiStateStarted

		case "content_block_start":
			index, _ := event["index"].(float64)
			switch int(index) {
			case 0:
				c.state = oaiStateStreaming
				c.writeDelta(`{"role":"assistant","content":""}`)
			case 1:
				c.state = oaiStateTool
				c.inTool = true
				cb, _ := event["content_block"].(map[string]interface{})
				if id, ok := cb["id"].(string); ok {
					c.toolID = id
				} else {
					c.toolID = "call_" + uuid.New().String()[:8]
				}
				if name, ok := cb["name"].(string); ok {
					c.toolName = name
				}
				c.toolInput.Reset()
				c.writeToolStart()
			}

		case "content_block_delta":
			idx, _ := event["index"].(float64)
			d, _ := event["delta"].(map[string]interface{})

			switch int(idx) {
			case 0:
				c.state = oaiStateStreaming
				c.inTool = false
				if text, ok := d["text"].(string); ok {
					c.writeTextDelta(text)
				}
			case 1:
				c.state = oaiStateTool
				c.inTool = true
				if pj, ok := d["partial_json"].(string); ok {
					c.toolInput.WriteString(pj)
					c.writeToolArgsDelta(pj)
				}
			}

		case "content_block_stop":
			idx, _ := event["index"].(float64)
			switch int(idx) {
			case 0:
				c.state = oaiStateStarted
			case 1:
				c.inTool = false
			}

		case "message_delta":
			if c.state == oaiStateDone {
				break
			}
			c.state = oaiStateDone

			d, _ := event["delta"].(map[string]interface{})
			stopReason, _ := d["stop_reason"].(string)
			usage, _ := event["usage"].(map[string]interface{})

			finishReason := "stop"
			if stopReason == "tool_use" {
				finishReason = "tool_calls"
			}

			var outputTokens int
			if usage != nil {
				if v, ok := usage["input_tokens"].(float64); ok {
					c.promptTokens = int(v)
				}
				if v, ok := usage["output_tokens"].(float64); ok {
					outputTokens = int(v)
				}
			}

			if c.inTool && c.toolName != "" {
				argsBytes, _ := json.Marshal(c.toolInput.String())
				c.writeToolFinal(string(argsBytes), finishReason)
			} else {
				c.writeTextFinal(finishReason)
			}
			c.writeUsage(outputTokens)

		case "message_stop":
			if c.state != oaiStateDone {
				c.writeDone()
			}
			c.state = oaiStateDone
		}
	}

	// 扫描结束，确保完成
	if c.state != oaiStateDone {
		c.writeDone()
	}
}

// helpers — 每种输出封装为独立方法

// writeDelta 写第一条带 role 的 chunk
func (c *OpenAIStreamConverter) writeDelta(contentJSON string) {
	c.writeChunkPlain(`{"delta":` + contentJSON + `}`)
}

// writeToolStart 写 tool_use 第一个 chunk
func (c *OpenAIStreamConverter) writeToolStart() {
	c.writeChunkPlain(`{"role":"assistant","content":null,"tool_calls":[{"index":0,"id":"` + c.toolID + `","type":"function","function":{"name":"` + c.toolName + `","arguments":""}}]}`)
}

// writeTextDelta 写 text 增量 delta
func (c *OpenAIStreamConverter) writeTextDelta(text string) {
	c.writeChunk(`{"content":"` + escapeJSON(text) + `"}`)
}

// writeChunk 写一个包含 delta 的 OpenAI SSE chunk
func (c *OpenAIStreamConverter) writeChunk(deltaJSON string) {
	c.writeChunkPlain(`{"delta":` + deltaJSON + `}`)
}

// writeToolFinal 写 tool_use 最终 chunk（带完整 arguments 和 finish_reason）
func (c *OpenAIStreamConverter) writeToolFinal(argsJSON string, finishReason string) {
	c.writeChunkPlain(`{"content":null,"tool_calls":[{"index":0,"id":"` + c.toolID + `","type":"function","function":{"arguments":"` + escapeJSON(argsJSON) + `"}}],"finish_reason":"` + finishReason + `"}`)
}

// writeToolArgsDelta 写 tool_use 的 arguments 增量 delta
func (c *OpenAIStreamConverter) writeToolArgsDelta(partialJSON string) {
	c.writeChunk(`{"tool_calls":[{"index":0,"function":{"arguments":"` + escapeJSON(partialJSON) + `"}}]}`)
}

// writeTextFinal 写 text 最终 chunk（带 finish_reason）
func (c *OpenAIStreamConverter) writeTextFinal(finishReason string) {
	c.writeChunkPlain(`{"finish_reason":"` + finishReason + `"}`)
}

// writeUsage 写带 usage 的 final chunk
func (c *OpenAIStreamConverter) writeUsage(outputTokens int) {
	c.writeChunkPlain(`{"usage":{"prompt_tokens":` + fmt.Sprintf("%d", c.promptTokens) + `,"completion_tokens":` + fmt.Sprintf("%d", outputTokens) + `,"total_tokens":` + fmt.Sprintf("%d", c.promptTokens+outputTokens) + `}}`)
}

// writeDone 写 [DONE] 标记
func (c *OpenAIStreamConverter) writeDone() {
	fmt.Fprintf(c.pw, "data: [DONE]\n\n")
}

// writeSSE 将 event 以 SSE 格式写入 writer（AnthropicSSEConverter 用）
func writeSSE(w *io.PipeWriter, event map[string]interface{}) {
	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", string(data))
}

// writeChunkPlain 将 JSON 主体以 OpenAI SSE 格式写入
func (c *OpenAIStreamConverter) writeChunkPlain(jsonBody string) {
	chunk := `{"id":"msg_` + c.streamID + `","object":"chat.completion.chunk","created":` + fmt.Sprintf("%d", c.created) + `,"model":"` + c.model + `","choices":[{"index":0,` + jsonBody + `}]}`
	fmt.Fprintf(c.pw, "data: %s\n\n", chunk)
}

// escapeJSON 转义 JSON 值中的特殊字符（用于内嵌在 JSON 字符串中）
func escapeJSON(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\r", "\\r")
	s = strings.ReplaceAll(s, "\t", "\\t")
	return s
}
