package storage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// ==================== parseTime / inTimeRange ====================

func TestParseTime(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"2024-01-02T15:04:05Z", false},
		{"2024-01-02T15:04:05+07:00", false},
		{"2024-01-02 15:04:05", false},
		{"2024-01-02", false},
		{"not-a-time", true},
		{"", true},
	}
	for _, c := range cases {
		_, err := parseTime(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("parseTime(%q): err=%v, wantErr=%v", c.in, err, c.wantErr)
		}
	}
}

func TestInTimeRange(t *testing.T) {
	mid := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	// 无边界 → true
	if !inTimeRange(mid, "", "") {
		t.Error("expected true with no bounds")
	}
	// 仅起始边界，时间在其后 → true
	if !inTimeRange(mid, "2024-01-01", "") {
		t.Error("expected true when after start")
	}
	// 仅起始边界，时间在其前 → false
	if inTimeRange(mid, "2024-12-01", "") {
		t.Error("expected false when before start")
	}
	// 仅结束边界，时间在其前 → true
	if !inTimeRange(mid, "", "2024-12-01") {
		t.Error("expected true when before end")
	}
	// 仅结束边界，时间在其后 → false
	if inTimeRange(mid, "", "2024-01-01") {
		t.Error("expected false when after end")
	}
	// 非法边界被忽略 → true
	if !inTimeRange(mid, "garbage", "garbage") {
		t.Error("expected true when bounds unparseable")
	}
}

// ==================== FileStorage ====================

func newFileStorage(t *testing.T) *FileStorage {
	t.Helper()
	dir := t.TempDir()
	st := NewFileStorage(dir)
	if st == nil {
		t.Fatal("expected non-nil storage")
	}
	fs, ok := st.(*FileStorage)
	if !ok {
		t.Fatal("expected *FileStorage")
	}
	return fs
}

func sampleRecord(apiKey, model, provider string, created time.Time) UsageRecord {
	return UsageRecord{
		RequestID:    "req-" + apiKey + "-" + model,
		VirtualModel: "vm",
		RealModel:    model,
		Provider:     provider,
		InputTokens:  10,
		OutputTokens: 5,
		TotalTokens:  15,
		APIKey:       apiKey,
		CreatedAt:    created,
	}
}

func TestFileStorage_PersistAndQueryByAPIKey(t *testing.T) {
	fs := newFileStorage(t)
	now := time.Now()
	rec := sampleRecord("key1", "gpt-4", "p1", now)
	if err := fs.Persist(rec); err != nil {
		t.Fatal(err)
	}

	// 同一 API Key 命中
	got, err := fs.QueryByAPIKey("key1", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 record, got %d", len(got))
	}
	// 不同 API Key 不命中
	none, _ := fs.QueryByAPIKey("other", "", "", "")
	if len(none) != 0 {
		t.Errorf("expected 0 for other key, got %d", len(none))
	}
}

func TestFileStorage_QueryByAPIKey_ModelFilter(t *testing.T) {
	fs := newFileStorage(t)
	now := time.Now()
	fs.Persist(sampleRecord("k", "gpt-4", "p1", now))
	fs.Persist(sampleRecord("k", "gpt-3", "p1", now))

	got, _ := fs.QueryByAPIKey("k", "gpt-4", "", "")
	if len(got) != 1 || got[0].RealModel != "gpt-4" {
		t.Errorf("expected only gpt-4, got %v", got)
	}
}

func TestFileStorage_QueryByTimeRange(t *testing.T) {
	fs := newFileStorage(t)
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	fs.Persist(sampleRecord("k", "m", "p", old))
	fs.Persist(sampleRecord("k", "m", "p", recent))

	got, _ := fs.QueryByTimeRange("2025-01-01", "2035-01-01")
	if len(got) != 1 || !got[0].CreatedAt.Equal(recent) {
		t.Errorf("expected only recent record, got %d", len(got))
	}
}

func TestFileStorage_QueryByRequestID(t *testing.T) {
	fs := newFileStorage(t)
	now := time.Now()
	rec := sampleRecord("k", "m", "p", now)
	fs.Persist(rec)

	got, err := fs.QueryByRequestID(rec.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.RequestID != rec.RequestID {
		t.Errorf("expected found record, got %+v", got)
	}
	none, _ := fs.QueryByRequestID("nope")
	if none != nil {
		t.Error("expected nil for missing request id")
	}
}

func TestFileStorage_AggregateDaily(t *testing.T) {
	fs := newFileStorage(t)
	now := time.Now()
	fs.Persist(sampleRecord("k", "m", "p", now))
	fs.Persist(sampleRecord("k", "m", "p", now))

	summaries, err := fs.AggregateDaily("", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 {
		t.Fatalf("expected 1 daily bucket, got %d", len(summaries))
	}
	if summaries[0].TotalTokens != 30 {
		t.Errorf("expected 30 total tokens, got %d", summaries[0].TotalTokens)
	}
	if summaries[0].RequestCount != 2 {
		t.Errorf("expected 2 requests, got %d", summaries[0].RequestCount)
	}
}

func TestFileStorage_AggregateWeeklyAndMonthly(t *testing.T) {
	fs := newFileStorage(t)
	now := time.Now()
	fs.Persist(sampleRecord("k", "m", "p", now))

	if s, err := fs.AggregateWeekly("", ""); err != nil || len(s) != 1 {
		t.Errorf("weekly: got %v err %v", s, err)
	}
	if s, err := fs.AggregateMonthly("", ""); err != nil || len(s) != 1 {
		t.Errorf("monthly: got %v err %v", s, err)
	}
}

func TestFileStorage_AggregateByRealModel(t *testing.T) {
	fs := newFileStorage(t)
	now := time.Now()
	fs.Persist(sampleRecord("k", "m1", "p1", now))
	fs.Persist(sampleRecord("k", "m1", "p1", now))
	fs.Persist(sampleRecord("k", "m2", "p2", now))

	summaries, err := fs.AggregateByRealModel("", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 2 {
		t.Fatalf("expected 2 buckets, got %d", len(summaries))
	}
}

func TestFileStorage_AggregateByAPIKey_Granularity(t *testing.T) {
	fs := newFileStorage(t)
	now := time.Now()
	fs.Persist(sampleRecord("k", "m", "p", now))

	for _, g := range []string{"daily", "weekly", "monthly", "garbage-defaults-to-daily"} {
		s, err := fs.AggregateByAPIKey("k", g, "", "")
		if err != nil {
			t.Fatalf("granularity %s: %v", g, err)
		}
		if len(s) != 1 {
			t.Errorf("granularity %s: expected 1 bucket, got %d", g, len(s))
		}
	}
	// 其他 API Key → 空
	none, _ := fs.AggregateByAPIKey("other", "daily", "", "")
	if len(none) != 0 {
		t.Errorf("expected 0 for other key")
	}
}

func TestFileStorage_SumTokensByAPIKey(t *testing.T) {
	fs := newFileStorage(t)
	now := time.Now()
	fs.Persist(sampleRecord("k", "m", "p", now))
	fs.Persist(sampleRecord("k", "m", "p", now))

	in, out, total, count, err := fs.SumTokensByAPIKey("k", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if in != 20 || out != 10 || total != 30 || count != 2 {
		t.Errorf("unexpected sums: in=%d out=%d total=%d count=%d", in, out, total, count)
	}
}

func TestFileStorage_SumTokensByTimeRange(t *testing.T) {
	fs := newFileStorage(t)
	now := time.Now()
	fs.Persist(sampleRecord("k", "m", "p", now))

	_, _, total, count, err := fs.SumTokensByTimeRange("", "")
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || total != 15 {
		t.Errorf("unexpected: count=%d total=%d", count, total)
	}
}

func TestFileStorage_AdminTotalStats(t *testing.T) {
	fs := newFileStorage(t)
	now := time.Now()
	fs.Persist(sampleRecord("k", "m", "p", now))

	stats, err := fs.AdminTotalStats("", "")
	if err != nil {
		t.Fatal(err)
	}
	if stats["total_requests"] != 1 || stats["total_tokens"] != 15 {
		t.Errorf("unexpected stats: %+v", stats)
	}
}

func TestFileStorage_PersistAutofill(t *testing.T) {
	fs := newFileStorage(t)
	// 无 RequestID / CreatedAt 应自动填充
	rec := UsageRecord{APIKey: "k", RealModel: "m", Provider: "p"}
	if err := fs.Persist(rec); err != nil {
		t.Fatal(err)
	}
	got, _ := fs.QueryByAPIKey("k", "", "", "")
	if len(got) != 1 {
		t.Fatalf("expected 1, got %d", len(got))
	}
	if got[0].RequestID == "" {
		t.Error("expected auto-generated RequestID")
	}
	if got[0].CreatedAt.IsZero() {
		t.Error("expected auto-filled CreatedAt")
	}
}

func TestFileStorage_JSONArrayToJSONL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")
	// 旧格式 JSON 数组
	old := `[{"request_id":"r1","api_key":"k","real_model":"m","provider":"p","input_tokens":1,"output_tokens":2,"total_tokens":3,"created_at":"2024-01-01T00:00:00Z"},{"request_id":"r2","api_key":"k","real_model":"m","provider":"p","input_tokens":4,"output_tokens":5,"total_tokens":9,"created_at":"2024-01-02T00:00:00Z"}]`
	if err := os.WriteFile(path, []byte(old), 0644); err != nil {
		t.Fatal(err)
	}

	fs := NewFileStorage(dir).(*FileStorage)
	if len(fs.records) != 2 {
		t.Fatalf("expected 2 records parsed from array, got %d", len(fs.records))
	}

	// 再追加一条，验证 JSONL 模式正常工作
	if err := fs.Persist(sampleRecord("k", "m", "p", time.Now())); err != nil {
		t.Fatal(err)
	}
	if len(fs.records) != 3 {
		t.Errorf("expected 3 records after persist, got %d", len(fs.records))
	}
}

func TestFileStorage_Close(t *testing.T) {
	fs := newFileStorage(t)
	if err := fs.Close(); err != nil {
		t.Errorf("Close returned error: %v", err)
	}
}

// ==================== RedisStorage ====================

func TestRedisStorage_NewRedisStorage(t *testing.T) {
	// nil client → 降级到文件存储
	fileStorage := NewRedisStorage(nil)
	if _, ok := fileStorage.(*FileStorage); !ok {
		t.Fatalf("expected FileStorage fallback, got %T", fileStorage)
	}

	// miniredis 测试实例
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()

	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer redisClient.Close()

	// 可用 Redis → RedisStorage
	redisStorage := NewRedisStorage(redisClient)
	if _, ok := redisStorage.(*RedisStorage); !ok {
		t.Fatalf("expected RedisStorage, got %T", redisStorage)
	}
}

func TestRedisStorage_Persist(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()

	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer redisClient.Close()

	ctx := context.Background()
	storage := NewRedisStorage(redisClient)
	record := UsageRecord{
		RequestID:    "req-123",
		APIKey:       "test-key",
		RealModel:    "gpt-3.5-turbo",
		Provider:     "openai",
		InputTokens:  100,
		OutputTokens: 50,
		TotalTokens:  150,
	}

	// Persist
	if err := storage.Persist(record); err != nil {
		t.Fatalf("persist failed: %v", err)
	}

	// 验证数据写入（通过 miniredis 直接查询）
	rawList, err := redisClient.LRange(ctx, "usage:recent:test-key", 0, -1).Result()
	if err != nil {
		t.Fatalf("failed to get recent data: %v", err)
	}
	if len(rawList) != 1 {
		t.Fatalf("expected 1 record in list, got %d", len(rawList))
	}

	var stored UsageRecord
	if err := json.Unmarshal([]byte(rawList[0]), &stored); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if stored.APIKey != "test-key" {
		t.Errorf("APIKey mismatch, got %q", stored.APIKey)
	}
	if stored.InputTokens != 100 {
		t.Errorf("InputTokens mismatch, got %d", stored.InputTokens)
	}
	if stored.TotalTokens != 150 {
		t.Errorf("TotalTokens mismatch, got %d", stored.TotalTokens)
	}

	// 验证 global list
	globalList, err := redisClient.LRange(ctx, "usage:recent:all", 0, -1).Result()
	if err != nil {
		t.Fatalf("failed to get global data: %v", err)
	}
	if len(globalList) != 1 {
		t.Fatalf("expected 1 record in global list, got %d", len(globalList))
	}
}

// newRedisStorageForTest 启动一个 miniredis 实例并返回可直接使用的 RedisStorage。
func newRedisStorageForTest(t *testing.T) (UsageStorage, *redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() {
		redisClient.Close()
		mr.Close()
	})
	storage := NewRedisStorage(redisClient)
	if _, ok := storage.(*RedisStorage); !ok {
		t.Fatalf("expected RedisStorage, got %T", storage)
	}
	return storage, redisClient, mr
}

// seedRedisRecords 写入一批带固定时间戳的记录，便于断言时间范围过滤。
func seedRedisRecords(t *testing.T, storage UsageStorage, records []UsageRecord) {
	t.Helper()
	for _, r := range records {
		if r.CreatedAt.IsZero() {
			r.CreatedAt = time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
		}
		if err := storage.Persist(r); err != nil {
			t.Fatalf("persist failed: %v", err)
		}
	}
}

func TestRedisStorage_QueryByAPIKey(t *testing.T) {
	storage, _, _ := newRedisStorageForTest(t)
	seedRedisRecords(t, storage, []UsageRecord{
		{RequestID: "r1", APIKey: "k1", RealModel: "m-a", Provider: "p1", InputTokens: 10, OutputTokens: 20, TotalTokens: 30},
		{RequestID: "r2", APIKey: "k2", RealModel: "m-b", Provider: "p1", InputTokens: 5, OutputTokens: 5, TotalTokens: 10},
		{RequestID: "r3", APIKey: "k1", RealModel: "m-b", Provider: "p2", InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
	})

	records, err := storage.QueryByAPIKey("k1", "", "", "")
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("expected 2 records for k1, got %d", len(records))
	}

	// 模型过滤
	filtered, err := storage.QueryByAPIKey("k1", "m-a", "", "")
	if err != nil {
		t.Fatalf("filtered query failed: %v", err)
	}
	if len(filtered) != 1 {
		t.Fatalf("expected 1 record after model filter, got %d", len(filtered))
	}

	// 不存在的 key
	empty, err := storage.QueryByAPIKey("missing", "", "", "")
	if err != nil {
		t.Fatalf("empty query failed: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("expected 0 records for missing key, got %d", len(empty))
	}
}

func TestRedisStorage_QueryByTimeRange(t *testing.T) {
	storage, _, _ := newRedisStorageForTest(t)
	seedRedisRecords(t, storage, []UsageRecord{
		{RequestID: "early", APIKey: "k1", TotalTokens: 1, CreatedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)},
		{RequestID: "mid", APIKey: "k1", TotalTokens: 2, CreatedAt: time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)},
		{RequestID: "late", APIKey: "k1", TotalTokens: 4, CreatedAt: time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)},
	})

	records, err := storage.QueryByTimeRange("2024-01-10", "2024-01-20")
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if len(records) != 1 || records[0].RequestID != "mid" {
		t.Fatalf("expected only 'mid' record, got %+v", records)
	}
}

func TestRedisStorage_QueryByRequestID(t *testing.T) {
	storage, _, _ := newRedisStorageForTest(t)
	seedRedisRecords(t, storage, []UsageRecord{
		{RequestID: "find-me", APIKey: "k1", InputTokens: 7, TotalTokens: 7},
		{RequestID: "other", APIKey: "k2", InputTokens: 8, TotalTokens: 8},
	})

	found, err := storage.QueryByRequestID("find-me")
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if found == nil || found.APIKey != "k1" || found.InputTokens != 7 {
		t.Fatalf("unexpected record: %+v", found)
	}

	missing, err := storage.QueryByRequestID("nope")
	if err != nil {
		t.Fatalf("missing query failed: %v", err)
	}
	if missing != nil {
		t.Fatalf("expected nil for missing id, got %+v", missing)
	}
}

func TestRedisStorage_SumTokensByAPIKey(t *testing.T) {
	storage, _, _ := newRedisStorageForTest(t)
	seedRedisRecords(t, storage, []UsageRecord{
		{RequestID: "a1", APIKey: "k1", RealModel: "m-a", InputTokens: 10, OutputTokens: 20, TotalTokens: 30},
		{RequestID: "a2", APIKey: "k1", RealModel: "m-b", InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
		{RequestID: "a3", APIKey: "k2", RealModel: "m-a", InputTokens: 99, OutputTokens: 99, TotalTokens: 198},
	})

	inTok, outTok, totalTok, count, err := storage.SumTokensByAPIKey("k1", "", "", "")
	if err != nil {
		t.Fatalf("sum failed: %v", err)
	}
	if inTok != 11 || outTok != 21 || totalTok != 32 || count != 2 {
		t.Errorf("unexpected sums: in=%d out=%d total=%d count=%d", inTok, outTok, totalTok, count)
	}

	inTok, outTok, totalTok, count, err = storage.SumTokensByAPIKey("k1", "m-a", "", "")
	if err != nil {
		t.Fatalf("filtered sum failed: %v", err)
	}
	if totalTok != 30 || count != 1 {
		t.Errorf("filtered sums wrong: total=%d count=%d", totalTok, count)
	}
}

func TestRedisStorage_SumTokensByTimeRange(t *testing.T) {
	storage, _, _ := newRedisStorageForTest(t)
	seedRedisRecords(t, storage, []UsageRecord{
		{RequestID: "t1", TotalTokens: 10, CreatedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)},
		{RequestID: "t2", TotalTokens: 20, CreatedAt: time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)},
	})

	inTok, outTok, totalTok, count, err := storage.SumTokensByTimeRange("2024-01-10", "2024-01-20")
	if err != nil {
		t.Fatalf("sum failed: %v", err)
	}
	if totalTok != 20 || count != 1 {
		t.Errorf("unexpected: total=%d count=%d", totalTok, count)
	}
	_ = inTok
	_ = outTok
}

func TestRedisStorage_AggregateByGranularity(t *testing.T) {
	storage, _, _ := newRedisStorageForTest(t)
	seedRedisRecords(t, storage, []UsageRecord{
		{RequestID: "g1", APIKey: "k1", RealModel: "m-a", Provider: "p1", InputTokens: 1, OutputTokens: 1, TotalTokens: 2,
			CreatedAt: time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)},
		{RequestID: "g2", APIKey: "k1", RealModel: "m-a", Provider: "p1", InputTokens: 2, OutputTokens: 2, TotalTokens: 4,
			CreatedAt: time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC)},
		{RequestID: "g3", APIKey: "k1", RealModel: "m-a", Provider: "p1", InputTokens: 4, OutputTokens: 4, TotalTokens: 8,
			CreatedAt: time.Date(2024, 1, 22, 0, 0, 0, 0, time.UTC)},
	})

	daily, err := storage.AggregateByAPIKey("k1", "daily", "", "")
	if err != nil {
		t.Fatalf("daily aggregate failed: %v", err)
	}
	if len(daily) != 3 {
		t.Fatalf("expected 3 daily buckets (3 distinct dates), got %d", len(daily))
	}

	// Jan 15/16 同属 ISO week 3，Jan 22 进入 week 4 → 2 个周桶
	weekly, err := storage.AggregateByAPIKey("k1", "weekly", "", "")
	if err != nil {
		t.Fatalf("weekly aggregate failed: %v", err)
	}
	if len(weekly) != 2 {
		t.Fatalf("expected 2 weekly buckets, got %d: %+v", len(weekly), weekly)
	}
	var weeklyTotal int
	for _, w := range weekly {
		weeklyTotal += w.TotalTokens
	}
	if weeklyTotal != 14 {
		t.Fatalf("expected weekly total 14, got %d", weeklyTotal)
	}

	// 三条记录同属 2024-01 → 1 个月桶
	monthly, err := storage.AggregateByAPIKey("k1", "monthly", "", "")
	if err != nil {
		t.Fatalf("monthly aggregate failed: %v", err)
	}
	if len(monthly) != 1 || monthly[0].TotalTokens != 14 {
		t.Fatalf("unexpected monthly result: %+v", monthly)
	}
}

func TestRedisStorage_AggregateAndAdmin(t *testing.T) {
	storage, _, _ := newRedisStorageForTest(t)
	seedRedisRecords(t, storage, []UsageRecord{
		{RequestID: "s1", APIKey: "k1", RealModel: "m-a", Provider: "p1", InputTokens: 10, OutputTokens: 5, TotalTokens: 15,
			CreatedAt: time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)},
		{RequestID: "s2", APIKey: "k2", RealModel: "m-b", Provider: "p2", InputTokens: 20, OutputTokens: 10, TotalTokens: 30,
			CreatedAt: time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC)},
	})

	if _, err := storage.AggregateDaily("", ""); err != nil {
		t.Fatalf("AggregateDaily failed: %v", err)
	}
	if _, err := storage.AggregateWeekly("", ""); err != nil {
		t.Fatalf("AggregateWeekly failed: %v", err)
	}
	if _, err := storage.AggregateMonthly("", ""); err != nil {
		t.Fatalf("AggregateMonthly failed: %v", err)
	}

	models, err := storage.AggregateByRealModel("", "")
	if err != nil {
		t.Fatalf("AggregateByRealModel failed: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("expected 2 model buckets, got %d", len(models))
	}

	stats, err := storage.AdminTotalStats("", "")
	if err != nil {
		t.Fatalf("AdminTotalStats failed: %v", err)
	}
	if stats["total_requests"] != 2 || stats["total_tokens"] != 45 {
		t.Fatalf("unexpected stats: %+v", stats)
	}

	daily, err := storage.AdminDailyStats("", "")
	if err != nil {
		t.Fatalf("AdminDailyStats failed: %v", err)
	}
	if len(daily) != 2 {
		t.Fatalf("expected 2 daily buckets, got %d", len(daily))
	}

	if err := storage.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
}
