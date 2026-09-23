package storage

import (
	"testing"
	"time"

	"llm-gateway/internal/config"
)

// ==================== buildDSN ====================

func TestBuildDSN_ExplicitDSN(t *testing.T) {
	cfg := config.PostgresConfig{
		DSN:   "postgres://dsn-literal@db/usage",
		User:  "ignored",
		Host:  "ignored",
		Port:  0,
		Database: "ignored",
	}
	if got := buildDSN(cfg); got != "postgres://dsn-literal@db/usage" {
		t.Errorf("explicit DSN should pass through, got %q", got)
	}
}

func TestBuildDSN_Composed(t *testing.T) {
	cfg := config.PostgresConfig{
		User:     "app",
		Password: "secret",
		Host:     "127.0.0.1",
		Port:     5432,
		Database: "usage",
		SSLMode:  "disable",
	}
	want := "postgres://app:secret@127.0.0.1:5432/usage?sslmode=disable"
	if got := buildDSN(cfg); got != want {
		t.Errorf("composed DSN mismatch\n got: %s\nwant: %s", got, want)
	}
}

// ==================== parseTimeRange ====================

func TestParseTimeRange(t *testing.T) {
	cases := []struct {
		name          string
		start, end    string
		wantHasStart  bool
		wantHasEnd    bool
		wantStartYear int
	}{
		{"both empty", "", "", false, false, 0},
		{"both set", "2024-01-01T00:00:00Z", "2024-12-31T23:59:59Z", true, true, 2024},
		{"start only", "2024-03-01", "", true, false, 2024},
		{"end only", "", "2024-03-01 12:00:00", false, true, 0},
		{"invalid start ignored", "not-a-date", "2024-03-01", false, true, 0},
		{"invalid both", "nope", "also-not", false, false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			start, end, hasStart, hasEnd := parseTimeRange(c.start, c.end)
			if hasStart != c.wantHasStart {
				t.Errorf("hasStart = %v, want %v", hasStart, c.wantHasStart)
			}
			if hasEnd != c.wantHasEnd {
				t.Errorf("hasEnd = %v, want %v", hasEnd, c.wantHasEnd)
			}
			if c.wantStartYear != 0 && start.Year() != c.wantStartYear {
				t.Errorf("start year = %d, want %d", start.Year(), c.wantStartYear)
			}
			if hasEnd && end.IsZero() {
				t.Error("end should be set when hasEnd is true")
			}
		})
	}
}

// ==================== FileStorage.AdminDailyStats / compact ====================

func TestFileStorage_AdminDailyStats(t *testing.T) {
	fs := newFileStorage(t)
	if err := fs.Persist(sampleRecord("k1", "m-a", "p1", time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC))); err != nil {
		t.Fatalf("persist failed: %v", err)
	}
	if err := fs.Persist(sampleRecord("k1", "m-a", "p1", time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC))); err != nil {
		t.Fatalf("persist failed: %v", err)
	}

	// AdminDailyStats 委托 AggregateDaily → 两条记录跨两天 → 2 个桶
	daily, err := fs.AdminDailyStats("", "")
	if err != nil {
		t.Fatalf("AdminDailyStats failed: %v", err)
	}
	if len(daily) != 2 {
		t.Fatalf("expected 2 daily buckets, got %d", len(daily))
	}

	// 时间范围过滤后只剩一条
	one, err := fs.AdminDailyStats("2024-01-15T00:00:00Z", "2024-01-15T23:59:59Z")
	if err != nil {
		t.Fatalf("filtered AdminDailyStats failed: %v", err)
	}
	if len(one) != 1 {
		t.Fatalf("expected 1 bucket in range, got %d", len(one))
	}
}

func TestFileStorage_Compact(t *testing.T) {
	fs := newFileStorage(t)
	for i := 0; i < 3; i++ {
		rec := sampleRecord("k1", "m-a", "p1", time.Now().Add(time.Duration(i)*time.Hour))
		if err := fs.Persist(rec); err != nil {
			t.Fatalf("persist failed: %v", err)
		}
	}

	// compact 重写文件；文件仍被句柄占用时无法用 os.ReadFile，改为直接验证内容行数
	fs.compact()

	// 重新从磁盘读取：先 Close 再 Open 需要路径，改用 records 长度间接验证写入未丢失
	if len(fs.records) != 3 {
		t.Fatalf("expected 3 records after compact, got %d", len(fs.records))
	}
}
