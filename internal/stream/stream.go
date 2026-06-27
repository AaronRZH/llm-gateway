package stream

import (
	"bufio"
	"bytes"
	"io"
	"net/http"

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

// RewriteAndForward 重写并转发 SSE 流
func (h *Handler) RewriteAndForward(w http.ResponseWriter, upstream io.ReadCloser, virtualModel string) {
	defer upstream.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Error().Msg("response writer does not support flushing")
		return
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

		// 重写 data: 行中的 model 字段
		if bytes.HasPrefix(line, []byte("data: ")) {
			payload := line[6:] // 去掉 "data: " 前缀

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
