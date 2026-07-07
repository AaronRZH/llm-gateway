package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"

	"llm-gateway/internal/config"
)

// PostgresStorage PostgreSQL 存储实现
type PostgresStorage struct {
	pool *pgxpool.Pool
	ctx  context.Context
}

// NewPostgresStorage 创建 PostgreSQL 存储，如果连接失败则降级到文件存储
func NewPostgresStorage(cfg config.PostgresConfig) UsageStorage {
	ctx, cancel := context.WithTimeout(context.Background(), cfg.ConnTimeout)
	defer cancel()

	dsn := buildDSN(cfg)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Warn().Err(err).Msg("postgres connection failed, falling back to file storage")
		return NewFileStorage("")
	}

	// 测试连通性
	if err := pool.Ping(ctx); err != nil {
		log.Warn().Err(err).Msg("postgres ping failed, falling back to file storage")
		pool.Close()
		return NewFileStorage("")
	}

	// 自动创建表
	if err := createTable(pool); err != nil {
		log.Warn().Err(err).Msg("postgres table creation failed, falling back to file storage")
		pool.Close()
		return NewFileStorage("")
	}

	log.Info().Str("host", cfg.Host).Int("port", cfg.Port).Str("database", cfg.Database).Msg("postgres storage enabled")
	return &PostgresStorage{
		pool: pool,
		ctx:  context.Background(),
	}
}

func buildDSN(cfg config.PostgresConfig) string {
	if cfg.DSN != "" {
		return cfg.DSN
	}
	return fmt.Sprintf(
		"postgres://%s:%s@%s:%d/%s?sslmode=%s",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, cfg.Database, cfg.SSLMode,
	)
}

const createTableSQL = `
CREATE TABLE IF NOT EXISTS usage_records (
    id              BIGSERIAL PRIMARY KEY,
    request_id      VARCHAR(255) NOT NULL,
    virtual_model   VARCHAR(255) NOT NULL,
    real_model      VARCHAR(255) NOT NULL,
    provider        VARCHAR(255) NOT NULL,
    input_tokens    INTEGER NOT NULL DEFAULT 0,
    output_tokens   INTEGER NOT NULL DEFAULT 0,
    total_tokens    INTEGER NOT NULL DEFAULT 0,
    est_input       INTEGER NOT NULL DEFAULT 0,
    est_output      INTEGER NOT NULL DEFAULT 0,
    official_in     INTEGER NOT NULL DEFAULT 0,
    official_out    INTEGER NOT NULL DEFAULT 0,
    api_key         VARCHAR(255) NOT NULL,
    created_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    UNIQUE (request_id)
);

CREATE INDEX IF NOT EXISTS idx_usage_created_at ON usage_records (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_usage_api_key ON usage_records (api_key);
CREATE INDEX IF NOT EXISTS idx_usage_real_model ON usage_records (real_model);
CREATE INDEX IF NOT EXISTS idx_usage_apikey_model_time ON usage_records (api_key, real_model, created_at DESC);
`

func createTable(pool *pgxpool.Pool) error {
	_, err := pool.Exec(context.Background(), createTableSQL)
	return err
}

func (s *PostgresStorage) Persist(record UsageRecord) error {
	if record.RequestID == "" {
		record.RequestID = uuid.New().String()
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now()
	}

	const sql = `
		INSERT INTO usage_records (request_id, virtual_model, real_model, provider,
			input_tokens, output_tokens, total_tokens, est_input, est_output,
			official_in, official_out, api_key, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (request_id) DO NOTHING
	`
	_, err := s.pool.Exec(s.ctx, sql,
		record.RequestID, record.VirtualModel, record.RealModel, record.Provider,
		record.InputTokens, record.OutputTokens, record.TotalTokens,
		record.EstInput, record.EstOutput, record.OfficialIn, record.OfficialOut,
		record.APIKey, record.CreatedAt,
	)
	return err
}

// parseTimeRange 解析时间范围字符串为 time.Time，返回 (start, end, hasStart, hasEnd)
func parseTimeRange(startTime, endTime string) (time.Time, time.Time, bool, bool) {
	var start, end time.Time
	var hasStart, hasEnd bool
	if startTime != "" {
		if t, err := parseTime(startTime); err == nil {
			start = t
			hasStart = true
		}
	}
	if endTime != "" {
		if t, err := parseTime(endTime); err == nil {
			end = t
			hasEnd = true
		}
	}
	return start, end, hasStart, hasEnd
}

func (s *PostgresStorage) QueryByAPIKey(apiKey, model, startTime, endTime string) ([]UsageRecord, error) {
	start, end, hasStart, hasEnd := parseTimeRange(startTime, endTime)
	args := []interface{}{apiKey}
	sql := `
		SELECT request_id, virtual_model, real_model, provider,
			input_tokens, output_tokens, total_tokens, est_input, est_output,
			official_in, official_out, api_key, created_at
		FROM usage_records
		WHERE api_key = $1
	`
	argNum := 2
	if model != "" {
		sql += fmt.Sprintf(" AND real_model = $%d", argNum)
		args = append(args, model)
		argNum++
	}
	if hasStart {
		sql += fmt.Sprintf(" AND created_at >= $%d", argNum)
		args = append(args, start)
		argNum++
	}
	if hasEnd {
		sql += fmt.Sprintf(" AND created_at <= $%d", argNum)
		args = append(args, end)
	}
	sql += " ORDER BY created_at DESC"
	return queryRecords(s.pool, s.ctx, sql, args...)
}

func (s *PostgresStorage) QueryByTimeRange(startTime, endTime string) ([]UsageRecord, error) {
	start, end, hasStart, hasEnd := parseTimeRange(startTime, endTime)
	args := []interface{}{}
	argNum := 1
	sql := `
		SELECT request_id, virtual_model, real_model, provider,
			input_tokens, output_tokens, total_tokens, est_input, est_output,
			official_in, official_out, api_key, created_at
		FROM usage_records
		WHERE 1=1
	`
	if hasStart {
		sql += fmt.Sprintf(" AND created_at >= $%d", argNum)
		args = append(args, start)
		argNum++
	}
	if hasEnd {
		sql += fmt.Sprintf(" AND created_at <= $%d", argNum)
		args = append(args, end)
	}
	sql += " ORDER BY created_at DESC"
	return queryRecords(s.pool, s.ctx, sql, args...)
}

func (s *PostgresStorage) QueryByRequestID(requestID string) (*UsageRecord, error) {
	const sql = `
		SELECT request_id, virtual_model, real_model, provider,
			input_tokens, output_tokens, total_tokens, est_input, est_output,
			official_in, official_out, api_key, created_at
		FROM usage_records WHERE request_id = $1 LIMIT 1
	`
	var rec UsageRecord
	err := s.pool.QueryRow(s.ctx, sql, requestID).Scan(
		&rec.RequestID, &rec.VirtualModel, &rec.RealModel, &rec.Provider,
		&rec.InputTokens, &rec.OutputTokens, &rec.TotalTokens,
		&rec.EstInput, &rec.EstOutput, &rec.OfficialIn, &rec.OfficialOut,
		&rec.APIKey, &rec.CreatedAt,
	)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

func (s *PostgresStorage) AggregateDaily(startTime, endTime string) ([]UsageSummary, error) {
	return s.aggregateByTimeUnit("TO_CHAR(created_at, 'YYYY-MM-DD')", startTime, endTime)
}

func (s *PostgresStorage) AggregateWeekly(startTime, endTime string) ([]UsageSummary, error) {
	return s.aggregateByTimeUnit("TO_CHAR(DATE_TRUNC('week', created_at), 'YYYY-\"W\"WW')", startTime, endTime)
}

func (s *PostgresStorage) AggregateMonthly(startTime, endTime string) ([]UsageSummary, error) {
	return s.aggregateByTimeUnit("TO_CHAR(created_at, 'YYYY-MM')", startTime, endTime)
}

func (s *PostgresStorage) aggregateByTimeUnit(dateExpr, startTime, endTime string) ([]UsageSummary, error) {
	start, end, hasStart, hasEnd := parseTimeRange(startTime, endTime)
	args := []interface{}{}
	argNum := 1
	sql := `
		SELECT ` + dateExpr + ` AS date, real_model, provider,
			COALESCE(SUM(input_tokens), 0) AS total_input,
			COALESCE(SUM(output_tokens), 0) AS total_output,
			COALESCE(SUM(total_tokens), 0) AS total_tokens,
			COUNT(*) AS request_count
		FROM usage_records
		WHERE 1=1
	`
	if hasStart {
		sql += fmt.Sprintf(" AND created_at >= $%d", argNum)
		args = append(args, start)
		argNum++
	}
	if hasEnd {
		sql += fmt.Sprintf(" AND created_at <= $%d", argNum)
		args = append(args, end)
	}
	sql += ` GROUP BY date, real_model, provider ORDER BY date DESC`
	return querySummaries(s.pool, s.ctx, sql, args...)
}

func (s *PostgresStorage) SumTokensByAPIKey(apiKey, model, startTime, endTime string) (int, int, int, int, error) {
	start, end, hasStart, hasEnd := parseTimeRange(startTime, endTime)
	args := []interface{}{apiKey}
	argNum := 2
	sql := `
		SELECT COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(total_tokens), 0),
			COUNT(*)
		FROM usage_records
		WHERE api_key = $1
	`
	if model != "" {
		sql += fmt.Sprintf(" AND real_model = $%d", argNum)
		args = append(args, model)
		argNum++
	}
	if hasStart {
		sql += fmt.Sprintf(" AND created_at >= $%d", argNum)
		args = append(args, start)
		argNum++
	}
	if hasEnd {
		sql += fmt.Sprintf(" AND created_at <= $%d", argNum)
		args = append(args, end)
	}
	var inputTokens, outputTokens, totalTokens, requestCount int
	err := s.pool.QueryRow(s.ctx, sql, args...).Scan(
		&inputTokens, &outputTokens, &totalTokens, &requestCount,
	)
	return inputTokens, outputTokens, totalTokens, requestCount, err
}

func (s *PostgresStorage) SumTokensByTimeRange(startTime, endTime string) (int, int, int, int, error) {
	start, end, hasStart, hasEnd := parseTimeRange(startTime, endTime)
	args := []interface{}{}
	argNum := 1
	sql := `
		SELECT COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(total_tokens), 0),
			COUNT(*)
		FROM usage_records
		WHERE 1=1
	`
	if hasStart {
		sql += fmt.Sprintf(" AND created_at >= $%d", argNum)
		args = append(args, start)
		argNum++
	}
	if hasEnd {
		sql += fmt.Sprintf(" AND created_at <= $%d", argNum)
		args = append(args, end)
	}
	var inputTokens, outputTokens, totalTokens, requestCount int
	err := s.pool.QueryRow(s.ctx, sql, args...).Scan(
		&inputTokens, &outputTokens, &totalTokens, &requestCount,
	)
	return inputTokens, outputTokens, totalTokens, requestCount, err
}

func (s *PostgresStorage) AdminTotalStats(startTime, endTime string) (map[string]int64, error) {
	start, end, hasStart, hasEnd := parseTimeRange(startTime, endTime)
	args := []interface{}{}
	argNum := 1
	sql := `
		SELECT COUNT(*),
			COALESCE(SUM(total_tokens), 0),
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0)
		FROM usage_records
		WHERE 1=1
	`
	if hasStart {
		sql += fmt.Sprintf(" AND created_at >= $%d", argNum)
		args = append(args, start)
		argNum++
	}
	if hasEnd {
		sql += fmt.Sprintf(" AND created_at <= $%d", argNum)
		args = append(args, end)
	}
	var totalReq, totalTok, totalIn, totalOut int64
	err := s.pool.QueryRow(s.ctx, sql, args...).Scan(&totalReq, &totalTok, &totalIn, &totalOut)
	if err != nil {
		return nil, err
	}
	return map[string]int64{
		"total_requests": totalReq,
		"total_tokens":   totalTok,
		"total_input":    totalIn,
		"total_output":   totalOut,
	}, nil
}

func (s *PostgresStorage) AggregateByRealModel(startTime, endTime string) ([]UsageSummary, error) {
	start, end, hasStart, hasEnd := parseTimeRange(startTime, endTime)
	args := []interface{}{}
	argNum := 1
	sql := `
		SELECT real_model, provider,
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(total_tokens), 0),
			COUNT(*)
		FROM usage_records
		WHERE 1=1
	`
	if hasStart {
		sql += fmt.Sprintf(" AND created_at >= $%d", argNum)
		args = append(args, start)
		argNum++
	}
	if hasEnd {
		sql += fmt.Sprintf(" AND created_at <= $%d", argNum)
		args = append(args, end)
	}
	sql += ` GROUP BY real_model, provider ORDER BY total_tokens DESC`
	return querySummariesNoDate(s.pool, s.ctx, sql, args...)
}

func (s *PostgresStorage) AdminDailyStats(startTime, endTime string) ([]UsageSummary, error) {
	return s.AggregateDaily(startTime, endTime)
}

func (s *PostgresStorage) Close() error {
	if s.pool != nil {
		s.pool.Close()
	}
	return nil
}

// queryRecords 执行查询并返回 UsageRecord 列表
func queryRecords(pool *pgxpool.Pool, ctx context.Context, sql string, args ...interface{}) ([]UsageRecord, error) {
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []UsageRecord
	for rows.Next() {
		var rec UsageRecord
		err := rows.Scan(
			&rec.RequestID, &rec.VirtualModel, &rec.RealModel, &rec.Provider,
			&rec.InputTokens, &rec.OutputTokens, &rec.TotalTokens,
			&rec.EstInput, &rec.EstOutput, &rec.OfficialIn, &rec.OfficialOut,
			&rec.APIKey, &rec.CreatedAt,
		)
		if err != nil {
			return nil, err
		}
		records = append(records, rec)
	}
	return records, rows.Err()
}

// querySummaries 执行聚合查询并返回 UsageSummary 列表
func querySummaries(pool *pgxpool.Pool, ctx context.Context, sql string, args ...interface{}) ([]UsageSummary, error) {
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var summaries []UsageSummary
	for rows.Next() {
		var s UsageSummary
		err := rows.Scan(&s.Date, &s.Model, &s.Provider, &s.TotalInput, &s.TotalOutput, &s.TotalTokens, &s.RequestCount)
		if err != nil {
			return nil, err
		}
		summaries = append(summaries, s)
	}
	return summaries, rows.Err()
}

// querySummariesNoDate 执行聚合查询（不带日期字段）并返回 UsageSummary 列表
func querySummariesNoDate(pool *pgxpool.Pool, ctx context.Context, sql string, args ...interface{}) ([]UsageSummary, error) {
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var summaries []UsageSummary
	for rows.Next() {
		var s UsageSummary
		err := rows.Scan(&s.Model, &s.Provider, &s.TotalInput, &s.TotalOutput, &s.TotalTokens, &s.RequestCount)
		if err != nil {
			return nil, err
		}
		summaries = append(summaries, s)
	}
	return summaries, rows.Err()
}