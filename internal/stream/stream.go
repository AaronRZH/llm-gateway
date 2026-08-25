package stream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"llm-gateway/internal/toolcall"
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

// RewriteAndForward 重写并转发 SSE 流，返回累计的响应内容与错误。
// openAIClient 指示下游客户端协议：异常结束（上游 stall / 单行超长）时补发对应的终止符，
// 避免客户端一直等待。OpenAI 客户端补发 [DONE]；Anthropic 客户端补发 message_stop。
//
// 返回错误仅用于"上游侧异常结束"（空闲超时触发 / 单行超长 / 其它非 EOF 错误），供上层回报熔断；
// 客户端主动断开（context.Canceled）或正常 EOF 返回 nil，不惩罚上游。
func (h *Handler) RewriteAndForward(w http.ResponseWriter, upstream io.ReadCloser, virtualModel string, openAIClient bool) (*StreamResult, error) {
	upstream = NewIdleTimeoutReader(upstream, h.idleTimeout)
	defer upstream.Close()

	result := &StreamResult{}
	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Error().Msg("response writer does not support flushing")
		return result, nil
	}

	scanner := bufio.NewScanner(upstream)
	scanner.Split(bufio.ScanLines)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	// 缓存所有上游行，直到流结束后统一决定转发策略。
	// 原因：部分上游会把 tool_calls 以 XML 形式嵌在 content 里并跨多个
	// SSE chunk 流出来（如 <invoke name=add_clarification>）。
	// 若逐行透传，客户端会先收到一堆原始 XML，事后又收到我们转换出的
	// tool_calls，两种形式并存会让下游 agent 困惑。所以先缓存，结束后：
	//   - 无 XML tool calls       -> 原样回放所有行
	//   - 有 XML tool calls 且 CleanContent 为空 -> 丢弃全部行，只发 tool_calls
	//   - 有 XML tool calls 且 CleanContent 非空 -> 回放行 + 追加 tool_calls
	type bufferedLine struct {
		data     bool
		line     []byte
		payload  []byte
		output   []byte
	}
	var buf []bufferedLine

	for scanner.Scan() {
		line := scanner.Bytes()
		entry := bufferedLine{line: line}

		if len(line) == 0 {
			buf = append(buf, entry)
			continue
		}
		if bytes.HasPrefix(line, []byte(`data: `)) {
			payload := line[6:]
			entry.data = true
			entry.payload = payload
			entry.output = append([]byte("data: "), h.rewriteModelField(payload, virtualModel)...)
			entry.output = append(entry.output, byte(0x0a))

			if bytes.Equal(payload, []byte(`[DONE]`)) {
				buf = append(buf, entry)
				continue
			}
			result.AccumulatedContent += extractContent(payload)
			extractToolCalls(payload, result)
			if result.Usage == nil {
				if usage := extractUsage(payload); usage != nil {
					result.Usage = usage
				}
			} else {
				mergeUsage(result.Usage, extractUsage(payload))
			}
			buf = append(buf, entry)
		} else {
			entry.output = append(line, byte(0x0a))
			buf = append(buf, entry)
		}
	}

	if err := scanner.Err(); err != nil && err != io.EOF && err != context.Canceled {
		log.Warn().Err(err).Msg("stream scan ended abnormally")
		if openAIClient {
			w.Write([]byte(`data: [DONE]\n\n`))
		} else {
			w.Write([]byte(`event: message_stop\ndata: "type":"message_stop"}\n\n`))
		}
		flusher.Flush()
		return result, fmt.Errorf("stream ended abnormally: %w", err)
	}

	// 后处理：将 XML 工具调用转换为标准 OpenAI tool_calls。
	normalized := toolcall.Normalize(result.AccumulatedContent)
	hasXMLToolCalls := len(normalized.ToolCalls) > 0
	if hasXMLToolCalls {
		for _, tc := range normalized.ToolCalls {
			result.AccumulatedToolCalls = append(result.AccumulatedToolCalls, ToolCallChunk{
				Index: -1,
				ID:    tc.ID,
				Type:  tc.Type,
				Function: FunctionChunk{
					Name:      tc.Function["name"].(string),
					Arguments: tc.Function["arguments"].(string),
				},
			})
		}
		result.AccumulatedContent = normalized.CleanContent
	}

	if hasXMLToolCalls {
		toolCallsSlice := h.ExtractToolCalls(result)

		if len(result.AccumulatedContent) == 0 {
			// 整段 content 都是 XML 工具调用，没有真正的对话文本。
			// 丢弃全部 content 行，只发一个结构化的 tool_calls chunk。
			if len(toolCallsSlice) > 0 {
				if err := h.emitToolCallsChunk(w, flusher, toolCallsSlice); err != nil {
					log.Warn().Err(err).Msg("failed to emit XML-derived tool_calls chunk")
				}
			}
			if openAIClient {
				w.Write([]byte(`data: [DONE]\n\n`))
			} else {
				w.Write([]byte(`event: message_stop\ndata: "type":"message_stop"}\n\n`))
			}
			flusher.Flush()
			return result, nil
		}

		// content 中既有对话文本也有 XML tool calls：先回放全部上游行，再追加 tool_calls。
		for _, entry := range buf {
			if entry.data && bytes.Equal(entry.payload, []byte(`[DONE]`)) {
				w.Write([]byte(`data: [DONE]\n\n`))
			} else {
				w.Write(entry.output)
			}
			flusher.Flush()
		}
		if len(toolCallsSlice) > 0 {
			if err := h.emitToolCallsChunk(w, flusher, toolCallsSlice); err != nil {
				log.Warn().Err(err).Msg("failed to emit XML-derived tool_calls chunk")
			}
		}
		if openAIClient {
			w.Write([]byte(`data: [DONE]\n\n`))
		} else {
			w.Write([]byte(`event: message_stop\ndata: "type":"message_stop"}\n\n`))
		}
		flusher.Flush()
		return result, nil
	}

	// 无 XML tool calls：原样回放所有上游行。
	for _, entry := range buf {
		if entry.data && bytes.Equal(entry.payload, []byte(`[DONE]`)) {
			w.Write([]byte(`data: [DONE]\n\n`))
		} else {
			w.Write(entry.output)
		}
		flusher.Flush()
	}
	return result, nil
}
// extractUsage 从 SSE chunk 提取真实 token 用量
// OpenAI 格式: {"choices":[...],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}
// Anthropic 格式: message_start 同时含 "input_tokens" 和 "output_tokens"，message_delta 同理
// emitToolCallsChunk writes a single SSE chunk carrying a complete
// delta.tool_calls payload (all arguments, not incremental) plus
// finish_reason="tool_calls", then flushes the writer. Used when the
// upstream sent XML-encoded tool calls in content and we converted them.
func (h *Handler) emitToolCallsChunk(w http.ResponseWriter, flusher http.Flusher, toolCalls []map[string]interface{}) error {
	delta := map[string]interface{}{
		"role":       "assistant",
		"content":    "",
		"tool_calls": toolCalls,
	}
	chunk := map[string]interface{}{
		"choices":       []map[string]interface{}{{"index": 0, "delta": delta}},
		"finish_reason": "tool_calls",
	}
	data, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	sep := []byte{0x0a, 0x0a}
	buf := make([]byte, 0, 6+len(data)+len(sep))
	buf = append(buf, []byte("data: ")...)
	buf = append(buf, data...)
	buf = append(buf, sep...)
	_, err = w.Write(buf)
	if err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

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

func (h *Handler) ExtractToolCalls(result *StreamResult) []map[string]interface{} {
	if len(result.AccumulatedToolCalls) == 0 {
		return nil
	}

	// 按 index 分组，合并 arguments。对 Index<0 的条目（XML 后置提取的 tool
	// calls），在写入 map 前替换成一个不会碰撞到有效 index 的 key，避免
	// 用负数做切片下标访问 result.AccumulatedToolCalls[idx].
	type mergedEntry struct {
		ID        string
		Type      string
		Name      string
		Arguments string
	}
	callMap := make(map[int]*mergedEntry)
	negSeq := 0
	for _, tc := range result.AccumulatedToolCalls {
		key := tc.Index
		if key < 0 {
			negSeq--
			key = negSeq
		}
		if me, ok := callMap[key]; ok {
			me.Arguments += tc.Function.Arguments
		} else {
			callMap[key] = &mergedEntry{
				ID:        tc.ID,
				Type:      tc.Type,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			}
		}
	}

	var toolCalls []map[string]interface{}
	for _, me := range callMap {
		argsBytes, _ := json.Marshal(me.Arguments)
		var argsObj interface{}
		json.Unmarshal(argsBytes, &argsObj)

		toolCalls = append(toolCalls, map[string]interface{}{
			"id":   me.ID,
			"type": me.Type,
			"function": map[string]interface{}{
				"name":      me.Name,
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
