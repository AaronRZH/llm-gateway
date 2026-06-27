package token

import (
	"context"
	"fmt"
	"sync"

	"github.com/pkoukk/tiktoken-go"
	"github.com/rs/zerolog/log"

	"llm-gateway/internal/config"
)

// Service Token 计算服务
type Service struct {
	mu        sync.RWMutex
	encoders  map[string]*tiktoken.Tiktoken
	mapping   map[string]string // model -> tokenizer name
	syncQueue chan UsageRecord
}

// UsageRecord 用量记录
type UsageRecord struct {
	RequestID    string
	Model        string
	VirtualModel string
	InputTokens  int
	OutputTokens int
	TotalTokens  int
	Provider     string
	Timestamp    int64
}

// New 创建 Token 服务
func New(cfg config.TokenConfig) *Service {
	s := &Service{
		encoders:  make(map[string]*tiktoken.Tiktoken),
		mapping:   cfg.TokenizerMapping,
		syncQueue: make(chan UsageRecord, 1000),
	}

	// 初始化 tiktoken encoders
	for model, encName := range cfg.TokenizerMapping {
		enc, err := tiktoken.GetEncoding(encName)
		if err != nil {
			log.Warn().Str("model", model).Str("encoder", encName).Msg("failed to load encoder")
			continue
		}
		s.encoders[model] = enc
	}

	// 启动后台同步 worker
	go s.syncWorker()

	return s
}

// EstimateInput 估算输入 token 数
func (s *Service) EstimateInput(messages []Message, model string) int {
	s.mu.RLock()
	enc, ok := s.encoders[model]
	s.mu.RUnlock()

	if !ok {
		// fallback: 粗略估算 ~4 chars per token
		return s.roughEstimate(messages)
	}

	var total int
	for _, msg := range messages {
		// 每条消息的固定开销（OpenAI 格式）
		total += 4 // <|start|>{role}\n{content}<|end|>\n

		// 内容 token
		total += len(enc.EncodeOrdinary(msg.Content))

		// 角色 token
		total += len(enc.EncodeOrdinary(msg.Role))
	}

	// 对话格式开销
	total += 2 // every reply is primed with <|start|>assistant<|message|>

	return total
}

// EstimateOutput 估算输出 token 数（流式中实时更新）
func (s *Service) EstimateOutput(text string, model string) int {
	s.mu.RLock()
	enc, ok := s.encoders[model]
	s.mu.RUnlock()

	if !ok {
		return len(text) / 4
	}

	return len(enc.EncodeOrdinary(text))
}

// RecordUsage 记录用量（异步）
func (s *Service) RecordUsage(requestID, model string, inputTokens, outputTokens int) {
	record := UsageRecord{
		RequestID:    requestID,
		Model:        model,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  inputTokens + outputTokens,
	}

	select {
	case s.syncQueue <- record:
	default:
		log.Warn().Msg("usage sync queue full, dropping record")
	}
}

// syncWorker 后台同步官网用量
func (s *Service) syncWorker() {
	for record := range s.syncQueue {
		// TODO: 调用各平台官方 Usage API 同步真实数据
		// 1. 查询官方 API 获取精确用量
		// 2. 对比本地估算 vs 官方数据
		// 3. 校准偏差系数

		log.Debug().
			Str("request_id", record.RequestID).
			Str("model", record.Model).
			Int("estimated_input", record.InputTokens).
			Int("estimated_output", record.OutputTokens).
			Msg("usage recorded")
	}
}

// roughEstimate 粗略估算（fallback）
func (s *Service) roughEstimate(messages []Message) int {
	var total int
	for _, msg := range messages {
		total += len(msg.Content) / 4
		total += len(msg.Role) / 4
		total += 4 // 格式开销
	}
	return total + 2
}

// Message 消息结构（复用 handler 中的定义）
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// SyncOfficialUsage 从官网同步用量（供外部调用）
func (s *Service) SyncOfficialUsage(ctx context.Context, provider, requestID string) error {
	// TODO: 实现各平台官方用量查询
	// OpenAI: GET /v1/usage
	// Anthropic: 控制台 API
	// DeepSeek: 官方用量接口
	return fmt.Errorf("not implemented: sync official usage for %s", provider)
}
