package token

import (
	"testing"

	"llm-gateway/internal/config"
	"llm-gateway/internal/storage"
)

func TestNew_EmptyMapping(t *testing.T) {
	s := New(config.TokenConfig{})
	if s == nil {
		t.Fatal("expected non-nil service")
	}
	if len(s.encoders) != 0 {
		t.Errorf("expected no encoders for empty mapping, got %d", len(s.encoders))
	}
}

func TestEstimateInput_Fallback(t *testing.T) {
	s := New(config.TokenConfig{})
	// 无 encoder → 走 roughEstimate
	msgs := []Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
	}
	got := s.EstimateInput(msgs, "unknown-model")
	want := s.roughEstimate(msgs)
	if got != want {
		t.Errorf("expected %d (roughEstimate), got %d", want, got)
	}
	if got <= 0 {
		t.Errorf("expected positive estimate, got %d", got)
	}
}

func TestEstimateInput_EmptyMessages(t *testing.T) {
	s := New(config.TokenConfig{})
	// 即使无消息也应有对话格式开销（roughEstimate 返回 +2）
	got := s.EstimateInput(nil, "x")
	if got != s.roughEstimate(nil) {
		t.Errorf("expected 2 (format overhead), got %d", got)
	}
}

func TestEstimateOutput_Fallback(t *testing.T) {
	s := New(config.TokenConfig{})
	if got := s.EstimateOutput("hello world", "x"); got != 2 {
		t.Errorf("expected fallback len/4 = 2, got %d", got)
	}
	if got := s.EstimateOutput("", "x"); got != 0 {
		t.Errorf("expected 0 for empty, got %d", got)
	}
}

func TestEstimateToolCallsOutput(t *testing.T) {
	s := New(config.TokenConfig{})

	// 空列表 → 0
	if got := s.EstimateToolCallsOutput(nil, "x"); got != 0 {
		t.Errorf("expected 0 for empty, got %d", got)
	}

	// 字符串参数（fallback len/4 + 20 包装）
	tcs := []map[string]interface{}{
		{
			"function": map[string]interface{}{
				"arguments": "{\"location\":\"beijing\"}",
			},
		},
	}
	got := s.EstimateToolCallsOutput(tcs, "x")
	// 20 + len("{\"location\":\"beijing\"}")/4 = 20 + 22/4 = 20 + 5 = 25
	if got != 25 {
		t.Errorf("expected 25, got %d", got)
	}

	// 非字符串参数（fallback 经 json.Marshal 后 len/4 + 20）
	tcs2 := []map[string]interface{}{
		{"function": map[string]interface{}{"arguments": map[string]interface{}{"a": 1}}},
	}
	got2 := s.EstimateToolCallsOutput(tcs2, "x")
	if got2 <= 20 {
		t.Errorf("expected > 20 (20 + marshaled args/4), got %d", got2)
	}
}

func TestRoughEstimate(t *testing.T) {
	s := New(config.TokenConfig{})
	msgs := []Message{{Role: "user", Content: "abcd"}, {Role: "a", Content: "ef"}}
	// 每条: len(content)/4 + len(role)/4 + 4 ; 合计 +2
	// "abcd"=4/4=1, "user"=4/4=1, +4 =6 ; "ef"=2/4=0, "a"=1/4=0, +4=4 ; +2 = 12
	want := 6 + 4 + 2
	if got := s.roughEstimate(msgs); got != want {
		t.Errorf("expected %d, got %d", want, got)
	}
	if got := s.roughEstimate(nil); got != 2 {
		t.Errorf("expected 2 for nil, got %d", got)
	}
}

func TestCalcErrorPct(t *testing.T) {
	// real=0 → 0（避免除零）
	if got := calcErrorPct(100, 0); got != 0 {
		t.Errorf("expected 0 for zero real, got %v", got)
	}
	// estimated 120, real 100 → (120-100)*100/100 = 20
	if got := calcErrorPct(120, 100); got != 20 {
		t.Errorf("expected 20, got %v", got)
	}
}

func TestCalibrationRatio(t *testing.T) {
	s := New(config.TokenConfig{})
	// 初始未校准 → 1.0
	if r := s.CalibrationRatio(); r != 1.0 {
		t.Errorf("expected 1.0 before calibration, got %v", r)
	}
	// 直接注入校准统计（同包可访问未导出字段）
	s.totalEstimates = 200
	s.totalReal = 100
	s.calibrated = true
	if r := s.CalibrationRatio(); r != 2.0 {
		t.Errorf("expected 2.0, got %v", r)
	}
}

func TestCalibrationInfo(t *testing.T) {
	s := New(config.TokenConfig{})
	info := s.CalibrationInfo()
	if info["calibrated"] != false {
		t.Errorf("expected calibrated=false, got %v", info["calibrated"])
	}
	if info["calibration_ratio"] != 1.0 {
		t.Errorf("expected ratio 1.0, got %v", info["calibration_ratio"])
	}
}

func TestRecordUsageNow_NilStorage(t *testing.T) {
	s := New(config.TokenConfig{})
	s.SetStorage(nil)
	// 不应 panic，也不持久化
	s.RecordUsageNow("req1", "m", "vm", "p", 1, 2, 3, 10, 5, 15, 0, "k")
}

func TestRecordUsageNow_WithStorage(t *testing.T) {
	s := New(config.TokenConfig{})
	// 使用临时目录的文件存储，避免依赖外部基础设施
	st := storage.NewFileStorage(t.TempDir())
	s.SetStorage(st)
	s.RecordUsageNow("req1", "m", "vm", "p", 1, 2, 3, 10, 5, 15, 0, "k")

	rec, err := s.QueryByRequestID("req1")
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil {
		t.Fatal("expected record persisted")
	}
	if rec.RealModel != "m" || rec.TotalTokens != 15 {
		t.Errorf("unexpected record: %+v", rec)
	}
}
