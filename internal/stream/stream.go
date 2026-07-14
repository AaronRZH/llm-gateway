package stream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// Handler SSE 流处理
type Handler struct {
	// idleTimeout 上游读取空闲超时：超过该时间未收到数据，判定上游 stall 并终止流，避免客户端看到流"中途卡住"（<=0 表示不启用）
	idleTimeout time.Duration
}

// New 创建流处理器，idleTimeout 为上游读取空闲超时（<=0 表示不启用）
func New(idleTimeout time.Duration) *Handler {
	return &Handler{idleTimeout: idleTimeout}
}

// IdleTimeout 返回配置的上游读取空闲超时
func (h *Handler) IdleTimeout() time.Duration {
	if h == nil {
		return 0
	}
	return h.idleTimeout
}

// StreamResult 流式处理结果
type StreamResult struct {
	// AccumulatedContent 累计的响应内容文本（用于估算输出 token）
	AccumulatedContent string
	// AccumulatedToolCalls 累计的 tool_calls 列表
	AccumulatedToolCalls []ToolCallChunk
	// Usage 从 SSE 最后一个 chunk 提取的真实 token 用量
	Usage *StreamUsage
}

// StreamUsage 流式 token 用量（从最后一个 SSE chunk 的 usage 字段提取）
type StreamUsage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// ToolCallChunk 流式 tool_call chunk
type ToolCallChunk struct {
	Index    int
	ID       string
	Type     string
	Function FunctionChunk
}

// FunctionChunk 流式函数 chunk
type FunctionChunk struct {
	Name      string
	Arguments string
}

// RewriteAndForward 重写并转发 SSE 流，返回累计的响应内容。
// openAIClient 指示下游客户端协议：异常结束（上游 stall / 单行超长）时补发对应的终止符，
// 避免客户端一直等待。OpenAI 客户端补发 [DONE]；Anthropic 客户端补发 message_stop。
func (h *Handler) RewriteAndForward(w http.ResponseWriter, upstream io.ReadCloser, virtualModel string, openAIClient bool) *StreamResult {
	upstream = NewIdleTimeoutReader(upstream, h.idleTimeout)
	defer upstream.Close()

	result := &StreamResult{}
	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Error().Msg("response writer does not support flushing")
		return result
	}

	scanner := bufio.NewScanner(upstream)
	scanner.Split(bufio.ScanLines)
	// 放大行缓冲：默认 64KB，超长 SSE 行（如超长 tool_call arguments）会触发 "token too long" 提前断流
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()

		// 空行是 SSE 分隔符，直接转发
		if len(line) == 0 {
			w.Write([]byte("\n"))
			flusher.Flush()
			continue
		}

		// 重写 data: 行中的 model 字段，同时累计 content / tool_calls
		if bytes.HasPrefix(line, []byte("data: ")) {
			payload := line[6:] // 去掉 "data: " 前缀

			// 忽略 [DONE] 标记
			if bytes.Equal(payload, []byte("[DONE]")) {
				w.Write([]byte("data: [DONE]\n"))
				flusher.Flush()
				continue
			}

			// 从 JSON 中提取 delta.content 用于累计
			result.AccumulatedContent += extractContent(payload)

			// 提取并累计 tool_calls
			extractToolCalls(payload, result)

			// 提取 usage（OpenAI 在最后一个 chunk 的 usage 字段返回真实 token 数）
			if result.Usage == nil {
				if usage := extractUsage(payload); usage != nil {
					result.Usage = usage
				}
			} else {
				// 合并增量 usage（Anthropic 格式）
				mergeUsage(result.Usage, extractUsage(payload))
			}

			// 替换 model 字段
			rewritten := h.rewriteModelField(payload, virtualModel)

			w.Write([]byte("data: "))
			w.Write(rewritten)
			w.Write([]byte("\n"))
		} else {
			w.Write(line)
			w.Write([]byte("\n"))
		}

		flusher.Flush()
	}

	if err := scanner.Err(); err != nil && err != io.EOF && err != context.Canceled {
		log.Warn().Err(err).Msg("stream scan ended abnormally (upstream stall or line too long), sending terminator")
		// 异常结束：补发终止符，避免客户端一直等待（context.Canceled 表示客户端已断开，无需补发）
		if openAIClient {
			w.Write([]byte("data: [DONE]\n\n"))
		} else {
			w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
		}
		flusher.Flush()
	}

	return result
}

// extractUsage 从 SSE chunk 提取真实 token 用量
// OpenAI 格式: {"choices":[...],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}
// Anthropic 格式: message_start 同时含 "input_tokens" 和 "output_tokens"，message_delta 同理
func extractUsage(payload []byte) *StreamUsage {
	// 尝试 OpenAI 格式: {"choices":[...],"usage":{"prompt_tokens":...}}
	var resp struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &resp); err == nil {
		if resp.Usage.PromptTokens > 0 || resp.Usage.CompletionTokens > 0 {
			return &StreamUsage{
				PromptTokens:     resp.Usage.PromptTokens,
				CompletionTokens: resp.Usage.CompletionTokens,
				TotalTokens:      resp.Usage.TotalTokens,
			}
		}
	}

	// 尝试 Anthropic 格式: message_start 同时含 "input_tokens" 和 "output_tokens"
	var startChunk struct {
		Type    string `json:"type"`
		Message struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"message"`
	}
	if err := json.Unmarshal(payload, &startChunk); err == nil && startChunk.Type == "message_start" {
		usage := &StreamUsage{}
		if startChunk.Message.InputTokens > 0 {
			usage.PromptTokens = startChunk.Message.InputTokens
		}
		if startChunk.Message.OutputTokens > 0 {
			usage.CompletionTokens = startChunk.Message.OutputTokens
		}
		if usage.PromptTokens > 0 || usage.CompletionTokens > 0 {
			return usage
		}
	}

	// 尝试 Anthropic 格式: message_delta 中的 "usage" 同时含 "input_tokens" 和 "output_tokens"
	var deltaChunk struct {
		Type  string `json:"type"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &deltaChunk); err == nil && deltaChunk.Type == "message_delta" {
		usage := &StreamUsage{}
		if deltaChunk.Usage.InputTokens > 0 {
			usage.PromptTokens = deltaChunk.Usage.InputTokens
		}
		if deltaChunk.Usage.OutputTokens > 0 {
			usage.CompletionTokens = deltaChunk.Usage.OutputTokens
		}
		if usage.PromptTokens > 0 || usage.CompletionTokens > 0 {
			return usage
		}
	}

	return nil
}

// mergeUsage 合并增量 usage（Anthropic 流式拆分 input/output）
func mergeUsage(existing *StreamUsage, incoming *StreamUsage) {
	if incoming == nil {
		return
	}
	if incoming.PromptTokens > 0 {
		existing.PromptTokens = incoming.PromptTokens
	}
	if incoming.CompletionTokens > 0 {
		existing.CompletionTokens = incoming.CompletionTokens
	}
	if incoming.TotalTokens > 0 {
		existing.TotalTokens = incoming.TotalTokens
	}
}

// ExtractToolCalls 从 StreamResult 中提取 tool_calls
func (h *Handler) ExtractToolCalls(result *StreamResult) []map[string]interface{} {
	if len(result.AccumulatedToolCalls) == 0 {
		return nil
	}

	// 按 index 分组，合并 function arguments
	callMap := make(map[int]*FunctionChunk)
	for _, tc := range result.AccumulatedToolCalls {
		if fc, ok := callMap[tc.Index]; ok {
			fc.Arguments += tc.Function.Arguments
		} else {
			callMap[tc.Index] = &FunctionChunk{
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			}
		}
	}

	var toolCalls []map[string]interface{}
	for idx, fc := range callMap {
		// 取第一个 chunk 的 ID 和 Type
		id := result.AccumulatedToolCalls[idx].ID
		typ := result.AccumulatedToolCalls[idx].Type
		argsBytes, _ := json.Marshal(fc.Arguments)
		// 反序列化为对象
		var argsObj interface{}
		json.Unmarshal(argsBytes, &argsObj)

		toolCalls = append(toolCalls, map[string]interface{}{
			"id":   id,
			"type": typ,
			"function": map[string]interface{}{
				"name":      fc.Name,
				"arguments": argsObj,
			},
		})
	}

	return toolCalls
}

// extractContent 从 SSE chunk JSON 中提取 delta.content
func extractContent(payload []byte) string {
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return ""
	}
	if len(chunk.Choices) > 0 {
		return chunk.Choices[0].Delta.Content
	}
	return ""
}

// extractToolCalls 从 SSE chunk 中提取 tool_calls
func extractToolCalls(payload []byte, result *StreamResult) {
	var chunk struct {
		Choices []struct {
			Delta struct {
				ToolCalls []map[string]interface{} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return
	}
	if len(chunk.Choices) == 0 {
		return
	}

	for _, tc := range chunk.Choices[0].Delta.ToolCalls {
		idx, _ := tc["index"].(float64)
		tcIndex := int(idx)
		tcType, _ := tc["type"].(string)

		// 尝试获取 id（只在第一个 chunk 中出现）
		tcID := ""
		if rawID, ok := tc["id"]; ok {
			tcID = rawID.(string)
		}
		if tcID == "" {
			tcID = "call_" + uuid.New().String()[:8]
		}

		// 提取 function name 和 arguments
		if fnRaw, ok := tc["function"]; ok {
			fn, ok := fnRaw.(map[string]interface{})
			if !ok {
				continue
			}
			fnName, _ := fn["name"].(string)
			fnArgs, _ := fn["arguments"].(string)

			tcChunk := ToolCallChunk{
				Index: tcIndex,
				ID:    tcID,
				Type:  tcType,
				Function: FunctionChunk{
					Name:      fnName,
					Arguments: fnArgs,
				},
			}
			result.AccumulatedToolCalls = append(result.AccumulatedToolCalls, tcChunk)
		}
	}
}

// rewriteModelField 重写 JSON 中的 model 字段（精确匹配 "model" 键，避免误中 "model_id" 等）
//
// 采用纯字节扫描，不做 json.Unmarshal，零解析开销。仅当 "model" 闭合引号后紧跟
// （允许空白）':' 时才视为真正的 model 键，从而与 "model_id"/"model_name" 等区分开。
func (h *Handler) rewriteModelField(payload []byte, virtualModel string) []byte {
	key := []byte(`"model"`)
	searchFrom := 0
	for {
		idx := bytes.Index(payload[searchFrom:], key)
		if idx == -1 {
			return payload
		}
		abs := searchFrom + idx

		// 跳过 "model" 闭合引号后的空白，必须紧跟 ':' 才是真正的 model 键；
		// 否则为 "model_id"/"model_name" 等，继续向后查找。
		rest := payload[abs+len(key):]
		i := 0
		for i < len(rest) && (rest[i] == ' ' || rest[i] == '\t' || rest[i] == '\n' || rest[i] == '\r') {
			i++
		}
		if i >= len(rest) || rest[i] != ':' {
			searchFrom = abs + len(key)
			continue
		}

		// 找到 model 值（下一个 "..." 字符串）的位置
		valueStart := bytes.IndexByte(payload[abs+len(key):], '"')
		if valueStart == -1 {
			return payload
		}
		valueStart += abs + len(key) + 1

		valueEnd := bytes.IndexByte(payload[valueStart:], '"')
		if valueEnd == -1 {
			return payload
		}
		valueEnd += valueStart

		// 替换 model 值
		result := make([]byte, 0, len(payload)-valueEnd+valueStart+len(virtualModel)+2)
		result = append(result, payload[:valueStart]...)
		result = append(result, virtualModel...)
		result = append(result, payload[valueEnd:]...)
		return result
	}
}
