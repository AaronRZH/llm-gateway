package stream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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
	// AccumulatedReasoning 累计的推理文本（reasoning/reasoning_content，用于估算输出 token）
	AccumulatedReasoning string
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

// ToolRepairFunc performs one format-only repair request. It receives the
// malformed model output and must return validated, OpenAI-shaped tool calls.
type ToolRepairFunc func(raw string) ([]map[string]interface{}, error)

// RewriteAndForward 重写并转发 SSE 流，返回累计的响应内容与错误。
// openAIClient 指示下游客户端协议：异常结束（上游 stall / 单行超长）时补发对应的终止符，
// 避免客户端一直等待。OpenAI 客户端补发 [DONE]；Anthropic 客户端补发 message_stop。
//
// 返回错误用于上游侧异常结束或无法纠正的工具调用协议错误，供上层回报熔断；
// 客户端主动断开（context.Canceled）或正常 EOF 返回 nil，不惩罚上游。
func (h *Handler) RewriteAndForward(w http.ResponseWriter, upstream io.ReadCloser, virtualModel string, openAIClient bool) (*StreamResult, error) {
	return h.RewriteAndForwardWithToolRepair(w, upstream, virtualModel, openAIClient, nil, nil)
}

// scanState holds the mutable state needed during the scan-and-forward loop.
type scanState struct {
	result                   *StreamResult
	xmlSuppressed            bool
	structuredToolSuppressed bool
	suppressedOutput         []byte
	xmlDetectBuf             []byte
}

// scanAndForward performs the streaming scan loop: reading SSE lines from upstream,
// forwarding them (or suppressing XML tool call markers), and accumulating state.
// This method extracts the scan loop from RewriteAndForwardWithToolRepair to reduce
// its cyclomatic complexity. Post-processing logic remains in the caller to preserve
// exact error propagation semantics per branch.
func (h *Handler) scanAndForward(w http.ResponseWriter, flusher http.Flusher, scanner *bufio.Scanner, virtualModel string, definitions []toolcall.Definition, s *scanState) {
	for scanner.Scan() {
		line := scanner.Bytes()

		if len(line) == 0 {
			if s.xmlSuppressed {
				s.suppressedOutput = append(s.suppressedOutput, '\n')
			} else {
				w.Write([]byte("\n"))
				flusher.Flush()
			}
			continue
		}
		if bytes.HasPrefix(line, []byte("data: ")) {
			payload := line[6:]

			if bytes.Equal(payload, []byte("[DONE]")) {
				continue
			}

			content, reasoning := extractContent(payload)
			s.result.AccumulatedContent += content
			s.result.AccumulatedReasoning += reasoning
			beforeToolCalls := len(s.result.AccumulatedToolCalls)
			extractToolCalls(payload, s.result)
			if s.result.Usage == nil {
				if usage := extractUsage(payload); usage != nil {
					s.result.Usage = usage
				}
			} else {
				mergeUsage(s.result.Usage, extractUsage(payload))
			}
			if definitions != nil && (s.structuredToolSuppressed || len(s.result.AccumulatedToolCalls) > beforeToolCalls) {
				s.structuredToolSuppressed = true
				continue
			}

			rewritten := h.rewriteModelField(payload, virtualModel)

			if s.xmlSuppressed {
				s.suppressedOutput = append(s.suppressedOutput, []byte("data: ")...)
				s.suppressedOutput = append(s.suppressedOutput, rewritten...)
				s.suppressedOutput = append(s.suppressedOutput, '\n')
				continue
			}

			// 滚动缓冲：追加当前 content，保留最近 4KB
			s.xmlDetectBuf = append(s.xmlDetectBuf, content...)
			if len(s.xmlDetectBuf) > 4096 {
				s.xmlDetectBuf = s.xmlDetectBuf[len(s.xmlDetectBuf)-4096:]
			}

			if containsXMLToolCallStart(string(s.xmlDetectBuf)) {
				s.xmlSuppressed = true
				s.suppressedOutput = append(s.suppressedOutput, []byte("data: ")...)
				s.suppressedOutput = append(s.suppressedOutput, rewritten...)
				s.suppressedOutput = append(s.suppressedOutput, '\n')
				continue
			}

			w.Write([]byte("data: "))
			w.Write(rewritten)
			w.Write([]byte("\n"))
		} else {
			if s.xmlSuppressed {
				s.suppressedOutput = append(s.suppressedOutput, line...)
				s.suppressedOutput = append(s.suppressedOutput, '\n')
			} else {
				w.Write(line)
				w.Write([]byte("\n"))
			}
		}
		flusher.Flush()
	}
}

// RewriteAndForwardWithToolRepair behaves like RewriteAndForward, but if an
// attempted XML tool call cannot be parsed or validated it performs at most
// one caller-provided format repair. Unknown formats are never replayed as
// ordinary assistant text.
func (h *Handler) RewriteAndForwardWithToolRepair(
	w http.ResponseWriter,
	upstream io.ReadCloser,
	virtualModel string,
	openAIClient bool,
	definitions []toolcall.Definition,
	repair ToolRepairFunc,
) (*StreamResult, error) {
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

	// 逐行实时转发，保持低首字节延迟（TTFB）。
	// 同时检测 XML 工具调用：一旦 content 中出现工具调用标签开头
	// （如 <invoke name=），进入抑制模式——后续 content 行不再转发，
	// 流结束后统一转换为结构化的 delta.tool_calls。
	// 普通文本响应完全不受影响，逐行实时透传（与旧行为一致）。
	scan := &scanState{result: result}
	h.scanAndForward(w, flusher, scanner, virtualModel, definitions, scan)
	xmlSuppressed := scan.xmlSuppressed
	structuredToolSuppressed := scan.structuredToolSuppressed
	suppressedOutput := scan.suppressedOutput

	if err := scanner.Err(); err != nil && err != io.EOF && err != context.Canceled {
		log.Warn().Err(err).Msg("stream scan ended abnormally")
		if openAIClient {
			w.Write([]byte("data: [DONE]\n\n"))
		} else {
			w.Write([]byte("event: message_stop\n"))
			w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
		}
		flusher.Flush()
		return result, fmt.Errorf("stream ended abnormally: %w", err)
	}

	// 后处理：将 XML 工具调用转换为标准 OpenAI tool_calls。
	normalized := toolcall.Normalize(result.AccumulatedContent)
	hasXMLToolCalls := len(normalized.ToolCalls) > 0
	if hasXMLToolCalls && definitions != nil {
		if err := toolcall.Validate(normalized.ToolCalls, definitions); err != nil {
			log.Warn().Err(err).Msg("rejected invalid XML-derived tool call")
			hasXMLToolCalls = false
		}
	}
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
		if len(toolCallsSlice) > 0 {
			if err := h.emitToolCallsChunk(w, flusher, toolCallsSlice); err != nil {
				log.Warn().Err(err).Msg("failed to emit XML-derived tool_calls chunk")
			}
		}
	} else if structuredToolSuppressed {
		calls := h.ExtractToolCalls(result)
		if err := toolcall.ValidateMaps(calls, definitions); err == nil {
			if openAIClient {
				if emitErr := h.emitToolCallsChunk(w, flusher, calls); emitErr != nil {
					return result, emitErr
				}
			} else {
				if emitErr := h.emitAnthropicToolCalls(w, flusher, calls); emitErr != nil {
					return result, emitErr
				}
			}
		} else {
			raw, _ := json.Marshal(calls)
			repaired, repairErr := attemptToolRepair(string(raw), repair)
			if repairErr != nil {
				h.emitToolCallError(w, flusher, openAIClient, repairErr)
				return result, repairErr
			}
			result.AccumulatedToolCalls = nil
			appendGenericToolCalls(result, repaired)
			if openAIClient {
				if emitErr := h.emitToolCallsChunk(w, flusher, repaired); emitErr != nil {
					return result, emitErr
				}
			} else {
				if emitErr := h.emitAnthropicToolCalls(w, flusher, repaired); emitErr != nil {
					return result, emitErr
				}
			}
		}
	} else if xmlSuppressed && toolcall.LooksLikeToolCall(result.AccumulatedContent) {
		// A tool-call marker was present but the payload was not parseable (or
		// failed schema validation). Never downgrade it to assistant text.
		if repair != nil {
			calls, repairErr := repair(result.AccumulatedContent)
			if repairErr == nil && len(calls) > 0 {
				appendGenericToolCalls(result, calls)
				if openAIClient {
					if err := h.emitToolCallsChunk(w, flusher, calls); err != nil {
						return result, err
					}
				} else {
					if err := h.emitAnthropicToolCalls(w, flusher, calls); err != nil {
						return result, err
					}
				}
				goto finish
			}
			if repairErr != nil {
				log.Warn().Err(repairErr).Msg("tool-call format repair failed")
			}
		}
		err := fmt.Errorf("%w: upstream emitted an unsupported tool-call format", toolcall.ErrMalformed)
		h.emitToolCallError(w, flusher, openAIClient, err)
		return result, err
	} else if xmlSuppressed {
		// A literal marker occurred in normal prose rather than a tool call.
		w.Write(suppressedOutput)
		flusher.Flush()
	}

finish:
	// 统一补发终止符：上游 [DONE] 已在扫描时跳过，这里始终补发一个。
	if openAIClient {
		w.Write([]byte("data: [DONE]\n\n"))
	} else {
		w.Write([]byte("event: message_stop\n"))
		w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	}
	flusher.Flush()
	return result, nil
}

func attemptToolRepair(raw string, repair ToolRepairFunc) ([]map[string]interface{}, error) {
	if repair == nil {
		return nil, fmt.Errorf("%w: no repair function configured", toolcall.ErrMalformed)
	}
	calls, err := repair(raw)
	if err != nil {
		return nil, err
	}
	if len(calls) == 0 {
		return nil, fmt.Errorf("%w: repair returned no calls", toolcall.ErrMalformed)
	}
	return calls, nil
}

func appendGenericToolCalls(result *StreamResult, calls []map[string]interface{}) {
	for _, tc := range calls {
		fn, _ := tc["function"].(map[string]interface{})
		name, _ := fn["name"].(string)
		arguments, _ := fn["arguments"].(string)
		id, _ := tc["id"].(string)
		if id == "" {
			id = "call_" + uuid.New().String()[:8]
		}
		result.AccumulatedToolCalls = append(result.AccumulatedToolCalls, ToolCallChunk{
			Index: -1, ID: id, Type: "function",
			Function: FunctionChunk{Name: name, Arguments: arguments},
		})
	}
}

func (h *Handler) emitToolCallError(w http.ResponseWriter, flusher http.Flusher, openAIClient bool, err error) {
	data, _ := json.Marshal(map[string]interface{}{
		"error": map[string]interface{}{
			"type":    "malformed_tool_call",
			"message": err.Error(),
		},
	})
	if openAIClient {
		_, _ = w.Write(append(append([]byte("data: "), data...), []byte("\n\n")...))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	} else {
		_, _ = w.Write([]byte("event: error\n"))
		_, _ = w.Write(append(append([]byte("data: "), data...), []byte("\n\n")...))
		_, _ = w.Write([]byte("event: message_stop\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	}
	flusher.Flush()
}

func (h *Handler) emitAnthropicToolCalls(w http.ResponseWriter, flusher http.Flusher, calls []map[string]interface{}) error {
	for i, tc := range calls {
		fn, _ := tc["function"].(map[string]interface{})
		name, _ := fn["name"].(string)
		arguments, _ := fn["arguments"].(string)
		id, _ := tc["id"].(string)
		if id == "" {
			id = "call_" + uuid.New().String()[:8]
		}
		input := map[string]interface{}{}
		_ = json.Unmarshal([]byte(arguments), &input)
		start, _ := json.Marshal(map[string]interface{}{
			"type": "content_block_start", "index": i,
			"content_block": map[string]interface{}{"type": "tool_use", "id": id, "name": name, "input": input},
		})
		if _, err := fmt.Fprintf(w, "event: content_block_start\ndata: %s\n\n", start); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", i); err != nil {
			return err
		}
	}
	_, err := w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":0}}\n\n"))
	flusher.Flush()
	return err
}

// containsXMLToolCallStart 判断 content 是否包含已知或疑似的标记式工具调用。
func containsXMLToolCallStart(content string) bool {
	patterns := []string{
		"<function=",
		"<tool_name=",
		"<tool_call=",
		"<invoke name=",
		// 外层包裹标签（无等号），如 ursal_call  / ursal_call
		// 用 ">" 结尾区分 "<tool_call=NAME>"（带等号）
		"<tool_call>",
		"</tool_call>",
	}
	for _, p := range patterns {
		if strings.Contains(content, p) {
			return true
		}
	}
	return toolcall.LooksLikeToolCall(content)
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
		"choices": []map[string]interface{}{{
			"index":         0,
			"delta":         delta,
			"finish_reason": "tool_calls",
		}},
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

// extractContent 从 SSE chunk JSON 中提取 delta.content 和推理文本。
// 推理字段同时支持 reasoning_content（DeepSeek/llama.cpp）与 reasoning（sensenova 等），
// 取首个非空值，避免重复计数。reasoning 不参与 XML 工具调用检测，故单独返回；
// 上游 usage 缺失时用于补全输出 token 估算。
func extractContent(payload []byte) (content, reasoning string) {
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				Reasoning        string `json:"reasoning"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return "", ""
	}
	if len(chunk.Choices) == 0 {
		return "", ""
	}
	d := chunk.Choices[0].Delta
	if d.ReasoningContent != "" {
		return d.Content, d.ReasoningContent
	}
	return d.Content, d.Reasoning
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
