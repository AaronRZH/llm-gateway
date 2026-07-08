package token

import (
	"encoding/json"
	"math"
	"sync"
	"time"

	"github.com/pkoukk/tiktoken-go"
	"github.com/rs/zerolog/log"

	"llm-gateway/internal/config"
	"llm-gateway/internal/storage"
)

// Service Token 计算服务
type Service struct {
	mu        sync.RWMutex
	encoders  map[string]*tiktoken.Tiktoken
	mapping   map[string]string // model -> tokenizer name
	syncQueue chan UsageRecord

	// 持久化存储
	storage storage.UsageStorage

	// 校准统计
	totalEstimates int64
	totalReal      int64
	calibrated     bool
}

// SetStorage 设置持久化存储（graceful degradation: storage 可为 nil）
func (s *Service) SetStorage(st storage.UsageStorage) {
	s.storage = st
	log.Info().Bool("enabled", st != nil).Msg("token storage configured")
}

// UsageRecord 用量记录（包含估算值和上游返回的真实值）
type UsageRecord struct {
	RequestID           string
	Model               string
	VirtualModel        string
	EstimatedInput      int // 本地 tiktoken 估算输入
	EstimatedOutput     int // 本地 tiktoken 估算输出
	EstimatedToolCalls  int // 本地 tiktoken 估算 tool_calls 输出 token
	RealInput           int // 上游 API 返回的 prompt_tokens
	RealOutput          int // 上游 API 返回的 completion_tokens
	RealTotal           int // 上游 API 返回的 total_tokens
	Provider            string
	ToolCalls           int // tool call 次数
	APIKey              string // 请求使用的 API Key
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

// EstimateToolCallsOutput 估算 tool calls 的 token 开销
func (s *Service) EstimateToolCallsOutput(toolCalls []map[string]interface{}, model string) int {
	s.mu.RLock()
	enc, encOk := s.encoders[model]
	s.mu.RUnlock()

	total := 0
	for _, tc := range toolCalls {
		// 每个 tool_call 的 JSON 包装 ~20 tokens
		total += 20
		if fn, ok := tc["function"]; ok {
			if fnMap, ok := fn.(map[string]interface{}); ok {
				if args, ok := fnMap["arguments"]; ok {
					argsStr := ""
					switch v := args.(type) {
 					case string:
						argsStr = v
					default:
						bytes, _ := json.Marshal(v)
						argsStr = string(bytes)
					}
					if encOk {
						total += len(enc.EncodeOrdinary(argsStr))
					} else {
						total += len(argsStr) / 4
					}
				}
			}
		}
	}
	return total
}

// RecordUsage 记录用量（异步），包含估算值和上游真实值
func (s *Service) RecordUsage(requestID, model, virtualModel, provider string,
	estimatedInput, estimatedOutput, estimatedToolCalls int,
	realInput, realOutput, realTotal int, toolCalls int, apiKey string) {

	record := UsageRecord{
		RequestID:         requestID,
		Model:             model,
		VirtualModel:      virtualModel,
		EstimatedInput:    estimatedInput,
		EstimatedOutput:   estimatedOutput,
		EstimatedToolCalls: estimatedToolCalls,
		RealInput:         realInput,
		RealOutput:        realOutput,
		RealTotal:         realTotal,
		Provider:          provider,
		ToolCalls:         toolCalls,
		APIKey:            apiKey,
		Timestamp:         time.Now().Unix(),
	}

	select {
	case s.syncQueue <- record:
	default:
		log.Warn().Msg("usage sync queue full, dropping record")
	}

	// 实时写入 Prometheus 指标（仅当有真实 token 数据时）
	if realInput > 0 || realOutput > 0 {
	}
}

// RecordUsageNow 同步记录用量 + 持久化（适用于非流式，直接写存储）
func (s *Service) RecordUsageNow(requestID, model, virtualModel, provider string,
	estimatedInput, estimatedOutput, estimatedToolCalls int,
	realInput, realOutput, realTotal int, toolCalls int, apiKey string) {

	// 先通过异步队列记录日志和指标
	s.RecordUsage(requestID, model, virtualModel, provider,
		estimatedInput, estimatedOutput, estimatedToolCalls, realInput, realOutput, realTotal, toolCalls, apiKey)

	// 同步持久化到存储层
	if s.storage != nil {
		rec := storage.UsageRecord{
			RequestID:    requestID,
			VirtualModel: virtualModel,
			RealModel:    model,
			Provider:     provider,
			InputTokens:  realInput,
			OutputTokens: realOutput,
			TotalTokens:  realTotal,
			EstInput:     estimatedInput,
			EstOutput:    estimatedOutput,
			APIKey:       apiKey,
			CreatedAt:    time.Now(),
		}
		if err := s.storage.Persist(rec); err != nil {
			log.Error().Err(err).Str("request_id", requestID).Msg("persist usage failed")
		}
	}
}

// ---- 查询方法（全部委托给 storage） ----

// QueryByAPIKey 按 API Key 查询用量
func (s *Service) QueryByAPIKey(apiKey, model, startTime, endTime string) ([]storage.UsageRecord, error) {
	if s.storage == nil {
		return nil, nil
	}
	return s.storage.QueryByAPIKey(apiKey, model, startTime, endTime)
}

// QueryByTimeRange 查询所有用量记录
func (s *Service) QueryByTimeRange(startTime, endTime string) ([]storage.UsageRecord, error) {
	if s.storage == nil {
		return nil, nil
	}
	return s.storage.QueryByTimeRange(startTime, endTime)
}

// QueryByRequestID 按请求 ID 查询
func (s *Service) QueryByRequestID(requestID string) (*storage.UsageRecord, error) {
	if s.storage == nil {
		return nil, nil
	}
	return s.storage.QueryByRequestID(requestID)
}

// SumTokensByAPIKey 按 API Key 聚合 token 统计
func (s *Service) SumTokensByAPIKey(apiKey, model, startTime, endTime string) (inputTokens, outputTokens, totalTokens, requestCount int, err error) {
	if s.storage == nil {
		return 0, 0, 0, 0, nil
	}
	return s.storage.SumTokensByAPIKey(apiKey, model, startTime, endTime)
}

// SumTokensByTimeRange 按时间范围聚合所有 token 统计
func (s *Service) SumTokensByTimeRange(startTime, endTime string) (inputTokens, outputTokens, totalTokens, requestCount int, err error) {
	if s.storage == nil {
		return 0, 0, 0, 0, nil
	}
	return s.storage.SumTokensByTimeRange(startTime, endTime)
}

// AggregateDaily 按日聚合统计
func (s *Service) AggregateDaily(startTime, endTime string) ([]storage.UsageSummary, error) {
	if s.storage == nil {
		return nil, nil
	}
	return s.storage.AggregateDaily(startTime, endTime)
}

// AggregateWeekly 按周聚合统计
func (s *Service) AggregateWeekly(startTime, endTime string) ([]storage.UsageSummary, error) {
	if s.storage == nil {
		return nil, nil
	}
	return s.storage.AggregateWeekly(startTime, endTime)
}

// AggregateMonthly 按月聚合统计
func (s *Service) AggregateMonthly(startTime, endTime string) ([]storage.UsageSummary, error) {
	if s.storage == nil {
		return nil, nil
	}
	return s.storage.AggregateMonthly(startTime, endTime)
}

// AdminTotalStats 管理员总统计
func (s *Service) AdminTotalStats(startTime, endTime string) (map[string]int64, error) {
	if s.storage == nil {
		return nil, nil
	}
	return s.storage.AdminTotalStats(startTime, endTime)
}

// AdminDailyStats 管理员按日统计
func (s *Service) AdminDailyStats(startTime, endTime string) ([]storage.UsageSummary, error) {
	if s.storage == nil {
		return nil, nil
	}
	return s.storage.AdminDailyStats(startTime, endTime)
}

// AggregateByRealModel 按 real_model 聚合 token 统计
func (s *Service) AggregateByRealModel(startTime, endTime string) ([]storage.UsageSummary, error) {
	if s.storage == nil {
		return nil, nil
	}
	return s.storage.AggregateByRealModel(startTime, endTime)
}

// AggregateByAPIKey 按 API Key + 时间粒度聚合统计
func (s *Service) AggregateByAPIKey(apiKey, granularity, startTime, endTime string) ([]storage.UsageSummary, error) {
	if s.storage == nil {
		return nil, nil
	}
	return s.storage.AggregateByAPIKey(apiKey, granularity, startTime, endTime)
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
			estimatedTotal := record.EstimatedInput + record.EstimatedOutput
			errorPct := calcErrorPct(estimatedTotal, record.RealTotal)
			logEvent.
				Int("real_input", record.RealInput).
				Int("real_output", record.RealOutput).
				Int("real_total", record.RealTotal).
				Float64("estimation_error_pct", errorPct).
				Msg("usage recorded (with real data)")

			// 估算偏差超过 50% 时打 warn 日志，便于后续校准
			if math.Abs(errorPct) > 50 {
				log.Warn().
					Str("request_id", record.RequestID).
					Str("model", record.Model).
					Str("provider", record.Provider).
					Int("estimated_total", estimatedTotal).
					Int("real_total", record.RealTotal).
					Float64("error_pct", errorPct).
					Msg("large estimation deviation")
			}

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
			// 持久化到存储层（如果已配置）
			if s.storage != nil {
				stoRec := storage.UsageRecord{
					RequestID:    record.RequestID,
					VirtualModel: record.VirtualModel,
					RealModel:    record.Model,
					Provider:     record.Provider,
					InputTokens:  record.RealInput,
					OutputTokens: record.RealOutput,
					TotalTokens:  record.RealTotal,
					EstInput:     record.EstimatedInput,
					EstOutput:    record.EstimatedOutput,
					APIKey:       record.APIKey,
					CreatedAt:    time.Unix(record.Timestamp, 0),
				}
				if err := s.storage.Persist(stoRec); err != nil {
					log.Error().Err(err).Str("request_id", record.RequestID).Msg("storage persist failed")
				}
			}
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

