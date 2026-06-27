package tokenizer

import (
	"github.com/pkoukk/tiktoken-go"
)

// Estimator Token 估算器
type Estimator struct {
	encoders map[string]*tiktoken.Tiktoken
}

// NewEstimator 创建估算器
func NewEstimator() (*Estimator, error) {
	e := &Estimator{
		encoders: make(map[string]*tiktoken.Tiktoken),
	}

	// 加载常用 tokenizer
	encodings := []string{"cl100k_base", "p50k_base", "r50k_base"}
	for _, enc := range encodings {
		tk, err := tiktoken.GetEncoding(enc)
		if err != nil {
			continue
		}
		e.encoders[enc] = tk
	}

	return e, nil
}

// Count 计算文本 token 数
func (e *Estimator) Count(text string, encoding string) int {
	enc, ok := e.encoders[encoding]
	if !ok {
		// fallback
		return len(text) / 4
	}

	return len(enc.EncodeOrdinary(text))
}

// CountMessages 计算消息列表 token 数（OpenAI 格式）
func (e *Estimator) CountMessages(messages []Message, encoding string) int {
	enc, ok := e.encoders[encoding]
	if !ok {
		return roughEstimate(messages)
	}

	var total int
	for _, msg := range messages {
		// 固定开销
		total += 4

		// 角色
		total += len(enc.EncodeOrdinary(msg.Role))

		// 内容
		total += len(enc.EncodeOrdinary(msg.Content))
	}

	// 结束标记
	total += 2

	return total
}

// Message 消息结构
type Message struct {
	Role    string
	Content string
}

func roughEstimate(messages []Message) int {
	var total int
	for _, msg := range messages {
		total += len(msg.Content) / 4
		total += len(msg.Role) / 4
		total += 4
	}
	return total + 2
}
