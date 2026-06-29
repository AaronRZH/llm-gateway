package token

import (
	"sync"
	"time"

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

	// 校准统计
	totalEstimates int64
	totalReal      int64
	calibrated     bool
}

// UsageRecord 用量记录（包含估算值和上游返回的真实值）
type UsageRecord struct {
	RequestID           string
	Model               string
	VirtualModel        string
	EstimatedInput      int // 本地 tiktoken 估算输入
	EstimatedOutput     int // 本地 tiktoken 估算输出
	RealInput           int // 上游 API 返回的 prompt_tokens
	RealOutput          int // 上游 API 返回的 completion_tokens
	RealTotal           int // 上游 API 返回的 total_tokens
	Provider            string
	ToolCalls           int // tool call 次数
	Timestamp           int64
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

	// 启动后台 worker：记录用量并校准估算偏差
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

// RecordUsage 记录用量（异步），包含估算值和上游真实值
func (s *Service) RecordUsage(requestID, model, virtualModel, provider string,
	estimatedInput, estimatedOutput int,
	realInput, realOutput, realTotal int, toolCalls int) {

	record := UsageRecord{
		RequestID:       requestID,
		Model:           model,
		VirtualModel:    virtualModel,
		EstimatedInput:  estimatedInput,
		EstimatedOutput: estimatedOutput,
		RealInput:       realInput,
		RealOutput:      realOutput,
		RealTotal:       realTotal,
		Provider:        provider,
		ToolCalls:       toolCalls,
		Timestamp:       time.Now().Unix(),
	}

	select {
	case s.syncQueue <- record:
	default:
		log.Warn().Msg("usage sync queue full, dropping record")
	}
}

// syncWorker 后台处理用量记录：持久化日志 + 估算校准
func (s *Service) syncWorker() {
	for record := range s.syncQueue {
		// 1. 记录结构化日志（可接入日志采集系统做后续分析）
		logEvent := log.Info().
			Str("request_id", record.RequestID).
			Str("model", record.Model).
			Str("virtual_model", record.VirtualModel).
			Str("provider", record.Provider).
			Int("estimated_input", record.EstimatedInput).
			Int("estimated_output", record.EstimatedOutput)

		if record.RealTotal > 0 {
			// 有上游真实用量，记录并与估算对比
			logEvent.
				Int("real_input", record.RealInput).
				Int("real_output", record.RealOutput).
				Int("real_total", record.RealTotal).
				Float64("estimation_error_pct", calcErrorPct(
					record.EstimatedInput+record.EstimatedOutput,
					record.RealTotal,
				)).
				Msg("usage recorded (with real data)")

			// 累计校准统计
			s.mu.Lock()
			s.totalEstimates += int64(record.EstimatedInput + record.EstimatedOutput)
			s.totalReal += int64(record.RealTotal)
			s.calibrated = true
			s.mu.Unlock()
		} else {
			// 仅估算值（流式场景或上游未返回 usage）
			logEvent.
				Int("estimated_total", record.EstimatedInput+record.EstimatedOutput).
				Msg("usage recorded (estimated only)")
		}
	}
}

// CalibrationRatio 返回估算/真实用量的累计比例
// 返回值 > 1 表示估算偏高，< 1 表示估算偏低
func (s *Service) CalibrationRatio() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.calibrated || s.totalReal == 0 {
		return 1.0
	}
	return float64(s.totalEstimates) / float64(s.totalReal)
}

func calcErrorPct(estimated, real int) float64 {
	if real == 0 {
		return 0
	}
	return float64(estimated-real) * 100.0 / float64(real)
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

// CalibrationInfo 返回当前估算校准信息
func (s *Service) CalibrationInfo() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return map[string]any{
		"calibrated":       s.calibrated,
		"total_estimated":  s.totalEstimates,
		"total_real":       s.totalReal,
		"calibration_ratio": func() float64 {
			if !s.calibrated || s.totalReal == 0 {
				return 1.0
			}
			return float64(s.totalEstimates) / float64(s.totalReal)
		}(),
	}
}

