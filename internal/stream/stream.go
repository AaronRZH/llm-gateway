package stream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"llm-gateway/internal/mapper"
)

// Handler SSE 流处理
type Handler struct {
	mapper *mapper.Service
}

// New 创建流处理器
func New(mapper *mapper.Service) *Handler {
	return &Handler{mapper: mapper}
}

// StreamResult 流式处理结果
type StreamResult struct {
	// AccumulatedContent 累计的响应内容文本（用于估算输出 token）
	AccumulatedContent string
	// AccumulatedToolCalls 累计的 tool_calls 列表
	AccumulatedToolCalls []ToolCallChunk
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

// RewriteAndForward 重写并转发 SSE 流，返回累计的响应内容
func (h *Handler) RewriteAndForward(w http.ResponseWriter, upstream io.ReadCloser, virtualModel string) *StreamResult {
	defer upstream.Close()

	result := &StreamResult{}
	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Error().Msg("response writer does not support flushing")
		return result
	}

	scanner := bufio.NewScanner(upstream)
	scanner.Split(bufio.ScanLines)

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

	if err := scanner.Err(); err != nil {
		log.Error().Err(err).Msg("stream scan error")
	}

	return result
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

// rewriteModelField 重写 JSON 中的 model 字段
func (h *Handler) rewriteModelField(payload []byte, virtualModel string) []byte {
	// 快速路径：如果 payload 不包含 "model"，直接返回
	if !bytes.Contains(payload, []byte(`"model"`)) {
		return payload
	}

	// 使用简单替换（生产环境建议用 json 解析）
	// 匹配 "model":"real-name" 替换为 "model":"virtual-name"

	// 找到 "model" 后面的值
	idx := bytes.Index(payload, []byte(`"model"`))
	if idx == -1 {
		return payload
	}

	// 找到 model 值的位置
	valueStart := bytes.IndexByte(payload[idx+7:], '"')
	if valueStart == -1 {
		return payload
	}
	valueStart += idx + 7 + 1

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
