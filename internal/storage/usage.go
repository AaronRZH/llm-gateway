package storage

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"
)

// UsageRecord 用量记录
type UsageRecord struct {
	RequestID    string    `json:"request_id"`
	VirtualModel string    `json:"virtual_model"`
	RealModel    string    `json:"real_model"`
	Provider     string    `json:"provider"`
	InputTokens  int       `json:"input_tokens"`
	OutputTokens int       `json:"output_tokens"`
	TotalTokens  int       `json:"total_tokens"`
	EstInput     int       `json:"est_input"`
	EstOutput    int       `json:"est_output"`
	OfficialIn   int       `json:"official_in"`
	OfficialOut  int       `json:"official_out"`
	APIKey       string    `json:"api_key"`
	CreatedAt    time.Time `json:"created_at"`
}

// UsageSummary 聚合统计
type UsageSummary struct {
	Date         string `json:"date"`
	Model        string `json:"model"`
	Provider     string `json:"provider"`
	TotalInput   int    `json:"total_input"`
	TotalOutput  int    `json:"total_output"`
	TotalTokens  int    `json:"total_tokens"`
	RequestCount int    `json:"request_count"`
}

// UsageStorage 用量持久化存储接口
type UsageStorage interface {
	Persist(record UsageRecord) error
	QueryByAPIKey(apiKey, model, startTime, endTime string) ([]UsageRecord, error)
	QueryByTimeRange(startTime, endTime string) ([]UsageRecord, error)
	QueryByRequestID(requestID string) (*UsageRecord, error)
	AggregateDaily(startTime, endTime string) ([]UsageSummary, error)
	AggregateWeekly(startTime, endTime string) ([]UsageSummary, error)
	AggregateMonthly(startTime, endTime string) ([]UsageSummary, error)
	AggregateByRealModel(startTime, endTime string) ([]UsageSummary, error)
	AggregateByAPIKey(apiKey, granularity, startTime, endTime string) ([]UsageSummary, error)
		SumTokensByAPIKey(apiKey, model, startTime, endTime string) (inputTokens, outputTokens, totalTokens, requestCount int, err error)
	SumTokensByTimeRange(startTime, endTime string) (inputTokens, outputTokens, totalTokens, requestCount int, err error)
	AdminTotalStats(startTime, endTime string) (map[string]int64, error)
	AdminDailyStats(startTime, endTime string) ([]UsageSummary, error)
	Close() error
}

// ===================== 文件存储（默认降级方案） =====================

// FileStorage 文件存储
type FileStorage struct {
	mu       sync.Mutex
	filePath string
	file     *os.File
	records  []UsageRecord
}

// NewFileStorage 创建文件存储
func NewFileStorage(dir string) UsageStorage {
	if dir == "" {
		dir = "data"
	}
	path := filepath.Join(dir, "usage.json")
	_ = os.MkdirAll(dir, 0755)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		log.Error().Err(err).Msg("failed to create usage file, persistence disabled")
		return nil
	}

	s := &FileStorage{filePath: path, file: f}

	// 读取现有数据，兼容 JSON 数组（旧格式）和 JSON Lines（新格式）
	data, err := os.ReadFile(path)
	if err == nil && len(data) > 0 {
		trimmed := bytes.TrimSpace(data)
		if len(trimmed) > 0 && trimmed[0] == '[' {
			// 旧格式：JSON 数组 [{...},{...},...]
			json.Unmarshal(data, &s.records)
			// 立即转换为 JSON Lines 格式
			f.Truncate(0)
			f.Seek(0, 0)
			for _, rec := range s.records {
				line, _ := json.Marshal(rec)
				f.Write(line)
				f.Write([]byte("\n"))
			}
		} else {
			// 新格式：JSON Lines，逐行读取
			scanner := bufio.NewScanner(bytes.NewReader(data))
			for scanner.Scan() {
				line := bytes.TrimSpace(scanner.Bytes())
				if len(line) == 0 {
					continue
				}
				var rec UsageRecord
				if json.Unmarshal(line, &rec) == nil {
					s.records = append(s.records, rec)
				}
			}
		}
	}
	// seek 到文件末尾准备追加
	f.Seek(0, 2)

	log.Info().Int("records", len(s.records)).Str("file", path).Msg("token usage file storage enabled")
	return s
}

func (s *FileStorage) Persist(record UsageRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if record.RequestID == "" {
		record.RequestID = uuid.New().String()
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now()
	}

	s.records = append(s.records, record)

	// Append-only: 只写一条 JSON Line，不做全量重写
	line, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal record: %w", err)
	}
	if _, err := s.file.Write(line); err != nil {
		return fmt.Errorf("append record: %w", err)
	}
	if _, err := s.file.Write([]byte("\n")); err != nil {
		return fmt.Errorf("append newline: %w", err)
	}

	// 每 10000 条触发一次压缩（重写整个文件去除可能损坏的行）
	if len(s.records)%10000 == 0 {
		s.compact()
	}

	return nil
}

// compact 重写整个文件，去除可能损坏的行
func (s *FileStorage) compact() {
	s.file.Truncate(0)
	s.file.Seek(0, 0)
	for _, rec := range s.records {
		data, _ := json.Marshal(rec)
		s.file.Write(data)
		s.file.Write([]byte("\n"))
	}
}

func (s *FileStorage) QueryByAPIKey(apiKey, model, startTime, endTime string) ([]UsageRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.filter(s.records, apiKey, model, startTime, endTime)
}

func (s *FileStorage) QueryByTimeRange(startTime, endTime string) ([]UsageRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.filter(s.records, "", "", startTime, endTime)
}

func (s *FileStorage) QueryByRequestID(requestID string) (*UsageRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.records {
		if s.records[i].RequestID == requestID {
			return &s.records[i], nil
		}
	}
	return nil, nil
}

func (s *FileStorage) AggregateDaily(startTime, endTime string) ([]UsageSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, _ := s.filter(s.records, "", "", startTime, endTime)
	return s.summarizeDaily(records)
}

func (s *FileStorage) AggregateWeekly(startTime, endTime string) ([]UsageSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, _ := s.filter(s.records, "", "", startTime, endTime)
	return s.summarizeWeekly(records)
}

func (s *FileStorage) AggregateMonthly(startTime, endTime string) ([]UsageSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, _ := s.filter(s.records, "", "", startTime, endTime)
	return s.summarizeMonthly(records), nil
}

func (s *FileStorage) AdminTotalStats(startTime, endTime string) (map[string]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, _ := s.filter(s.records, "", "", startTime, endTime)
	var totalReq, totalTok, totalIn, totalOut int64
	for _, r := range records {
		totalReq++
		totalTok += int64(r.TotalTokens)
		totalIn += int64(r.InputTokens)
		totalOut += int64(r.OutputTokens)
	}
	return map[string]int64{
		"total_requests": totalReq,
		"total_tokens":   totalTok,
		"total_input":    totalIn,
		"total_output":   totalOut,
	}, nil
}

func (s *FileStorage) AdminDailyStats(startTime, endTime string) ([]UsageSummary, error) {
	return s.AggregateDaily(startTime, endTime)
}

// AggregateByAPIKey 按 API Key + 时间粒度聚合统计
func (s *FileStorage) AggregateByAPIKey(apiKey, granularity, startTime, endTime string) ([]UsageSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, _ := s.filter(s.records, apiKey, "", startTime, endTime)
	switch granularity {
	case "weekly":
		return s.summarizeWeekly(records)
	case "monthly":
		return s.summarizeMonthly(records), nil
	default:
		return s.summarizeDaily(records)
	}
}

// AggregateByRealModel 按 real_model 聚合 token 统计（所有 provider 分别统计）
func (s *FileStorage) AggregateByRealModel(startTime, endTime string) ([]UsageSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, _ := s.filter(s.records, "", "", startTime, endTime)

	type rk struct{ Model, Provider string }
	buckets := make(map[rk]*UsageSummary)
	for _, r := range records {
		key := rk{r.RealModel, r.Provider}
		b, ok := buckets[key]
		if !ok {
			buckets[key] = &UsageSummary{Model: key.Model, Provider: key.Provider}
			b = buckets[key]
		}
		b.TotalInput += r.InputTokens
		b.TotalOutput += r.OutputTokens
		b.TotalTokens += r.TotalTokens
		b.RequestCount++
	}
	out := make([]UsageSummary, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TotalTokens > out[j].TotalTokens })
	return out, nil
}

func (s *FileStorage) Close() error {
	if s.file != nil {
		return s.file.Close()
	}
	return nil
}

func (s *FileStorage) filter(records []UsageRecord, apiKey, model, startTime, endTime string) ([]UsageRecord, error) {
	result := make([]UsageRecord, 0)
	for _, r := range records {
		if apiKey != "" && r.APIKey != apiKey {
			continue
		}
		if model != "" && r.RealModel != model {
			continue
		}
		if startTime != "" {
			t, err := parseTime(startTime)
			if err == nil && r.CreatedAt.Before(t) {
				continue
			}
		}
		if endTime != "" {
			t, err := parseTime(endTime)
			if err == nil && r.CreatedAt.After(t) {
				continue
			}
		}
		result = append(result, r)
	}
	return result, nil
}

// SumTokensByAPIKey 按 API Key 聚合 token 统计
func (s *FileStorage) SumTokensByAPIKey(apiKey, model, startTime, endTime string) (int, int, int, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, _ := s.filter(s.records, apiKey, model, startTime, endTime)
	var inputTokens, outputTokens, totalTokens, requestCount int
	for _, r := range records {
		inputTokens += r.InputTokens
		outputTokens += r.OutputTokens
		totalTokens += r.TotalTokens
		requestCount++
	}
	return inputTokens, outputTokens, totalTokens, requestCount, nil
}

// SumTokensByTimeRange 按时间范围聚合所有 token 统计
func (s *FileStorage) SumTokensByTimeRange(startTime, endTime string) (int, int, int, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, _ := s.filter(s.records, "", "", startTime, endTime)
	var inputTokens, outputTokens, totalTokens, requestCount int
	for _, r := range records {
		inputTokens += r.InputTokens
		outputTokens += r.OutputTokens
		totalTokens += r.TotalTokens
		requestCount++
	}
	return inputTokens, outputTokens, totalTokens, requestCount, nil
}

func (s *FileStorage) summarizeDaily(records []UsageRecord) ([]UsageSummary, error) {
	type k struct{ Date, Model, Provider string }
	buckets := make(map[k]*UsageSummary)
	for _, r := range records {
		key := k{r.CreatedAt.Format("2006-01-02"), r.RealModel, r.Provider}
		b, ok := buckets[key]
		if !ok {
			buckets[key] = &UsageSummary{Date: key.Date, Model: key.Model, Provider: key.Provider}
			b = buckets[key]
		}
		b.TotalInput += r.InputTokens
		b.TotalOutput += r.OutputTokens
		b.TotalTokens += r.TotalTokens
		b.RequestCount++
	}
	out := make([]UsageSummary, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date > out[j].Date })
	return out, nil
}

func (s *FileStorage) summarizeWeekly(records []UsageRecord) ([]UsageSummary, error) {
	type wk struct{ Week, Model, Provider string }
	buckets := make(map[wk]*UsageSummary)
	for _, r := range records {
		year, week := r.CreatedAt.ISOWeek()
		key := wk{fmt.Sprintf("%d-W%02d", year, week), r.RealModel, r.Provider}
		b, ok := buckets[key]
		if !ok {
			buckets[key] = &UsageSummary{Date: key.Week, Model: key.Model, Provider: key.Provider}
			b = buckets[key]
		}
		b.TotalInput += r.InputTokens
		b.TotalOutput += r.OutputTokens
		b.TotalTokens += r.TotalTokens
		b.RequestCount++
	}
	out := make([]UsageSummary, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date > out[j].Date })
	return out, nil
}

func (s *FileStorage) summarizeMonthly(records []UsageRecord) []UsageSummary {
	type mk struct{ Month, Model, Provider string }
	buckets := make(map[mk]*UsageSummary)
	for _, r := range records {
		key := mk{r.CreatedAt.Format("2006-01"), r.RealModel, r.Provider}
		b, ok := buckets[key]
		if !ok {
			buckets[key] = &UsageSummary{Date: key.Month, Model: key.Model, Provider: key.Provider}
			b = buckets[key]
		}
		b.TotalInput += r.InputTokens
		b.TotalOutput += r.OutputTokens
		b.TotalTokens += r.TotalTokens
		b.RequestCount++
	}
	out := make([]UsageSummary, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date > out[j].Date })
	return out
}

// ===================== Redis 存储（主方案） =====================

// RedisStorage 纯 Redis 存储
type RedisStorage struct {
	redis *redis.Client
	ctx   context.Context
}

// NewRedisStorage 创建 Redis 存储，如果 redis 不可用则降级到文件存储
func NewRedisStorage(redisClient *redis.Client) UsageStorage {
	if redisClient == nil {
		log.Info().Msg("redis not configured, falling back to file storage")
		return NewFileStorage("")
	}
	// 测试连通性
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := redisClient.Ping(ctx).Err(); err != nil {
		log.Warn().Err(err).Msg("redis unreachable, falling back to file storage")
		return NewFileStorage("")
	}
	log.Info().Msg("token usage redis storage enabled")
	return &RedisStorage{
		redis: redisClient,
		ctx:   context.Background(),
	}
}

func (s *RedisStorage) Persist(record UsageRecord) error {
	if record.RequestID == "" {
		record.RequestID = uuid.New().String()
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now()
	}

	data, _ := json.Marshal(record)

	// go-redis v9: Pipeline() no args, Exec(ctx) for execution
	pipe := s.redis.Pipeline()
	pipe.LPush(s.ctx, "usage:recent:"+record.APIKey, data)
	pipe.LTrim(s.ctx, "usage:recent:"+record.APIKey, 0, 9999)
	pipe.LPush(s.ctx, "usage:recent:all", data)
	pipe.LTrim(s.ctx, "usage:recent:all", 0, 99999)
	_, _ = pipe.Exec(s.ctx)
	return nil
}

func (s *RedisStorage) QueryByAPIKey(apiKey, model, startTime, endTime string) ([]UsageRecord, error) {
	raw, err := s.redis.LRange(s.ctx, "usage:recent:"+apiKey, 0, -1).Result()
	if err != nil {
		return nil, err
	}
	return s.parseAndFilter(raw, model, startTime, endTime)
}

func (s *RedisStorage) QueryByTimeRange(startTime, endTime string) ([]UsageRecord, error) {
	raw, err := s.redis.LRange(s.ctx, "usage:recent:all", 0, -1).Result()
	if err != nil {
		return nil, err
	}
	return s.parseAndFilterTimeRange(raw, startTime, endTime)
}

func (s *RedisStorage) QueryByRequestID(requestID string) (*UsageRecord, error) {
	keys, err := s.redis.Keys(s.ctx, "usage:recent:*").Result()
	if err != nil {
		return nil, err
	}
	for _, key := range keys {
		raw, err := s.redis.LRange(s.ctx, key, 0, -1).Result()
		if err != nil {
			continue
		}
		for _, r := range raw {
			var rec UsageRecord
			if json.Unmarshal([]byte(r), &rec) == nil && rec.RequestID == requestID {
				return &rec, nil
			}
		}
	}
	return nil, nil
}

// SumTokensByAPIKey 按 API Key 聚合 token 统计
func (s *RedisStorage) SumTokensByAPIKey(apiKey, model, startTime, endTime string) (int, int, int, int, error) {
	raw, err := s.redis.LRange(s.ctx, "usage:recent:"+apiKey, 0, -1).Result()
	if err != nil {
		return 0, 0, 0, 0, err
	}
	var inputTokens, outputTokens, totalTokens, requestCount int
	for _, r := range raw {
		var rec UsageRecord
		if json.Unmarshal([]byte(r), &rec) != nil {
			continue
		}
		if model != "" && rec.RealModel != model {
			continue
		}
		if !inTimeRange(rec.CreatedAt, startTime, endTime) {
			continue
		}
		inputTokens += rec.InputTokens
		outputTokens += rec.OutputTokens
		totalTokens += rec.TotalTokens
		requestCount++
	}
	return inputTokens, outputTokens, totalTokens, requestCount, nil
}

// SumTokensByTimeRange 按时间范围聚合所有 token 统计
func (s *RedisStorage) SumTokensByTimeRange(startTime, endTime string) (int, int, int, int, error) {
	raw, err := s.redis.LRange(s.ctx, "usage:recent:all", 0, -1).Result()
	if err != nil {
		return 0, 0, 0, 0, err
	}
	var inputTokens, outputTokens, totalTokens, requestCount int
	for _, r := range raw {
		var rec UsageRecord
		if json.Unmarshal([]byte(r), &rec) != nil {
			continue
		}
		if !inTimeRange(rec.CreatedAt, startTime, endTime) {
			continue
		}
		inputTokens += rec.InputTokens
		outputTokens += rec.OutputTokens
		totalTokens += rec.TotalTokens
		requestCount++
	}
	return inputTokens, outputTokens, totalTokens, requestCount, nil
}

func (s *RedisStorage) AggregateDaily(startTime, endTime string) ([]UsageSummary, error) {
	raw, err := s.redis.LRange(s.ctx, "usage:recent:all", 0, -1).Result()
	if err != nil {
		return nil, err
	}
	return s.summarizeRecordsDaily(raw, startTime, endTime)
}

func (s *RedisStorage) AggregateWeekly(startTime, endTime string) ([]UsageSummary, error) {
	raw, err := s.redis.LRange(s.ctx, "usage:recent:all", 0, -1).Result()
	if err != nil {
		return nil, err
	}
	return s.summarizeRecordsWeekly(raw, startTime, endTime)
}

func (s *RedisStorage) AggregateMonthly(startTime, endTime string) ([]UsageSummary, error) {
	raw, err := s.redis.LRange(s.ctx, "usage:recent:all", 0, -1).Result()
	if err != nil {
		return nil, err
	}
	return s.summarizeRecordsMonthly(raw, startTime, endTime), nil
}

func (s *RedisStorage) AggregateByRealModel(startTime, endTime string) ([]UsageSummary, error) {
	raw, err := s.redis.LRange(s.ctx, "usage:recent:all", 0, -1).Result()
	if err != nil {
		return nil, err
	}
	return s.summarizeRecordsByRealModel(raw, startTime, endTime), nil
}

func (s *RedisStorage) AdminTotalStats(startTime, endTime string) (map[string]int64, error) {
	raw, err := s.redis.LRange(s.ctx, "usage:recent:all", 0, -1).Result()
	if err != nil {
		return nil, err
	}
	var totalReq, totalTok, totalIn, totalOut int64
	for _, r := range raw {
		var rec UsageRecord
		if json.Unmarshal([]byte(r), &rec) != nil {
			continue
		}
		if !inTimeRange(rec.CreatedAt, startTime, endTime) {
			continue
		}
		totalReq++
		totalTok += int64(rec.TotalTokens)
		totalIn += int64(rec.InputTokens)
		totalOut += int64(rec.OutputTokens)
	}
	return map[string]int64{
		"total_requests": totalReq,
		"total_tokens":   totalTok,
		"total_input":    totalIn,
		"total_output":   totalOut,
	}, nil
}

func (s *RedisStorage) AdminDailyStats(startTime, endTime string) ([]UsageSummary, error) {
	return s.AggregateDaily(startTime, endTime)
}

// AggregateByAPIKey 按 API Key + 时间粒度聚合统计
func (s *RedisStorage) AggregateByAPIKey(apiKey, granularity, startTime, endTime string) ([]UsageSummary, error) {
	raw, err := s.redis.LRange(s.ctx, "usage:recent:"+apiKey, 0, -1).Result()
	if err != nil {
		return nil, err
	}
	switch granularity {
	case "weekly":
		return s.summarizeRecordsWeekly(raw, startTime, endTime)
	case "monthly":
		return s.summarizeRecordsMonthly(raw, startTime, endTime), nil
	default:
		return s.summarizeRecordsDaily(raw, startTime, endTime)
	}
}

func (s *RedisStorage) Close() error { return nil }

func (s *RedisStorage) parseAndFilter(raw []string, model, startTime, endTime string) ([]UsageRecord, error) {
	result := make([]UsageRecord, 0)
	for _, r := range raw {
		var rec UsageRecord
		if json.Unmarshal([]byte(r), &rec) != nil {
			continue
		}
		if model != "" && rec.RealModel != model {
			continue
		}
		if !inTimeRange(rec.CreatedAt, startTime, endTime) {
			continue
		}
		result = append(result, rec)
	}
	return result, nil
}

func (s *RedisStorage) parseAndFilterTimeRange(raw []string, startTime, endTime string) ([]UsageRecord, error) {
	result := make([]UsageRecord, 0)
	for _, r := range raw {
		var rec UsageRecord
		if json.Unmarshal([]byte(r), &rec) != nil {
			continue
		}
		if !inTimeRange(rec.CreatedAt, startTime, endTime) {
			continue
		}
		result = append(result, rec)
	}
	return result, nil
}

func (s *RedisStorage) summarizeRecordsDaily(raw []string, startTime, endTime string) ([]UsageSummary, error) {
	type k struct{ Date, Model, Provider string }
	buckets := make(map[k]*UsageSummary)
	for _, r := range raw {
		var rec UsageRecord
		if json.Unmarshal([]byte(r), &rec) != nil {
			continue
		}
		if !inTimeRange(rec.CreatedAt, startTime, endTime) {
			continue
		}
		key := k{rec.CreatedAt.Format("2006-01-02"), rec.RealModel, rec.Provider}
		b, ok := buckets[key]
		if !ok {
			buckets[key] = &UsageSummary{Date: key.Date, Model: key.Model, Provider: key.Provider}
			b = buckets[key]
		}
		b.TotalInput += rec.InputTokens
		b.TotalOutput += rec.OutputTokens
		b.TotalTokens += rec.TotalTokens
		b.RequestCount++
	}
	out := make([]UsageSummary, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date > out[j].Date })
	return out, nil
}

func (s *RedisStorage) summarizeRecordsWeekly(raw []string, startTime, endTime string) ([]UsageSummary, error) {
	type wk struct{ Week, Model, Provider string }
	buckets := make(map[wk]*UsageSummary)
	for _, r := range raw {
		var rec UsageRecord
		if json.Unmarshal([]byte(r), &rec) != nil {
			continue
		}
		if !inTimeRange(rec.CreatedAt, startTime, endTime) {
			continue
		}
		year, week := rec.CreatedAt.ISOWeek()
		key := wk{fmt.Sprintf("%d-W%02d", year, week), rec.RealModel, rec.Provider}
		b, ok := buckets[key]
		if !ok {
			buckets[key] = &UsageSummary{Date: key.Week, Model: key.Model, Provider: key.Provider}
			b = buckets[key]
		}
		b.TotalInput += rec.InputTokens
		b.TotalOutput += rec.OutputTokens
		b.TotalTokens += rec.TotalTokens
		b.RequestCount++
	}
	out := make([]UsageSummary, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date > out[j].Date })
	return out, nil
}

func (s *RedisStorage) summarizeRecordsMonthly(raw []string, startTime, endTime string) []UsageSummary {
	type mk struct{ Month, Model, Provider string }
	buckets := make(map[mk]*UsageSummary)
	for _, r := range raw {
		var rec UsageRecord
		if json.Unmarshal([]byte(r), &rec) != nil {
			continue
		}
		if !inTimeRange(rec.CreatedAt, startTime, endTime) {
			continue
		}
		key := mk{rec.CreatedAt.Format("2006-01"), rec.RealModel, rec.Provider}
		b, ok := buckets[key]
		if !ok {
			buckets[key] = &UsageSummary{Date: key.Month, Model: key.Model, Provider: key.Provider}
			b = buckets[key]
		}
		b.TotalInput += rec.InputTokens
		b.TotalOutput += rec.OutputTokens
		b.TotalTokens += rec.TotalTokens
		b.RequestCount++
	}
	out := make([]UsageSummary, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date > out[j].Date })
	return out
}

func (s *RedisStorage) summarizeRecordsByRealModel(raw []string, startTime, endTime string) []UsageSummary {
	type rk struct{ Model, Provider string }
	buckets := make(map[rk]*UsageSummary)
	for _, r := range raw {
		var rec UsageRecord
		if json.Unmarshal([]byte(r), &rec) != nil {
			continue
		}
		if !inTimeRange(rec.CreatedAt, startTime, endTime) {
			continue
		}
		key := rk{rec.RealModel, rec.Provider}
		b, ok := buckets[key]
		if !ok {
			buckets[key] = &UsageSummary{Model: key.Model, Provider: key.Provider}
		}
		b.TotalInput += rec.InputTokens
		b.TotalOutput += rec.OutputTokens
		b.TotalTokens += rec.TotalTokens
		b.RequestCount++
	}
	out := make([]UsageSummary, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TotalTokens > out[j].TotalTokens })
	return out
}

// ===================== 工具函数 =====================

var timeLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05Z",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

func parseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse time: %s", s)
}

func inTimeRange(t time.Time, startTime, endTime string) bool {
	if startTime != "" {
		if start, err := parseTime(startTime); err == nil && t.Before(start) {
			return false
		}
	}
	if endTime != "" {
		if end, err := parseTime(endTime); err == nil && t.After(end) {
			return false
		}
	}
	return true
}
