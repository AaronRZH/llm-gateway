package storage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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
		RequestID:   "req-" + apiKey + "-" + model,
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
