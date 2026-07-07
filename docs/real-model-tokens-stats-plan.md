# Plan: Add /admin/usage/by-real-model Endpoint

## Context
The user wants a new admin endpoint that shows a summary of tokens forwarded through each `real_model` (upstream model). Currently, the existing `/admin/usage` and `/admin/usage/daily` endpoints aggregate by time (and optionally by model) but don't provide a clean "by real_model" leaderboard. The storage layer already records `RealModel` on each usage record and indexes on it (PostgreSQL has `idx_usage_real_model`).

## Changes

### 1. `internal/storage/usage.go` — Add `AggregateByRealModel` to interface + implement
- **UsageStorage interface**: add method `AggregateByRealModel(startTime, endTime string) ([]UsageSummary, error)`
- **FileStorage**: iterate records, group by `(RealModel, Provider)`, sum tokens, sort by total_tokens desc
- **RedisStorage**: same logic over `parseAndFilterTimeRange(raw, startTime, endTime)`
- Returns `[]UsageSummary` with `Date` field left empty (or set to `"total"`), `Model` = real_model, `Provider` = provider

### 2. `internal/storage/postgres.go` — Implement `AggregateByRealModel`
- SQL: `SELECT real_model, provider, SUM(input_tokens), SUM(output_tokens), SUM(total_tokens), COUNT(*) FROM usage_records [WHERE time filter] GROUP BY real_model, provider ORDER BY SUM(total_tokens) DESC`
- Reuses existing `querySummaries` pattern

### 3. `internal/token/token.go` — Add delegating method
- `func (s *Service) AggregateByRealModel(startTime, endTime string) ([]storage.UsageSummary, error)` — delegates to `s.storage.AggregateByRealModel`

### 4. `cmd/gateway/handlers.go` — Add handler
- `handleAdminUsageByRealModel(tokenService *token.Service) gin.HandlerFunc`
- Accepts query params: `start_time`, `end_time` (optional time range)
- Calls `tokenService.AggregateByRealModel(startTime, endTime)`
- Returns `{"data": [...], "model_count": N}` where items have real_model, provider, total_tokens, input_tokens, output_tokens, request_count

### 5. `cmd/gateway/main.go` — Register route
- `r.GET("/admin/usage/by-real-model", handleAdminUsageByRealModel(tokenService))`

## Files to Modify
1. `internal/storage/usage.go` — interface + FileStorage impl + RedisStorage impl
2. `internal/storage/postgres.go` — PostgresStorage impl
3. `internal/token/token.go` — delegating method
4. `cmd/gateway/handlers.go` — new handler
5. `cmd/gateway/main.go` — new route

## Verification
- `go build ./...` compiles successfully
- Manual test: send a few requests through the gateway, then `GET /admin/usage/by-real-model` returns per-real_model token summaries
- With `?start_time=&end_time=` filters to a time range