package tokenizer

import "testing"

func TestNewEstimator(t *testing.T) {
	e, err := NewEstimator()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e == nil {
		t.Fatal("expected non-nil estimator")
	}
}

func TestCount_Fallback(t *testing.T) {
	e, _ := NewEstimator()
	// 未知 encoding → 退化为 len/4
	if got := e.Count("hello world", "nonexistent-encoding"); got != 2 {
		t.Errorf("expected fallback 2, got %d", got)
	}
	// 空字符串 → 0
	if got := e.Count("", "nonexistent-encoding"); got != 0 {
		t.Errorf("expected 0 for empty, got %d", got)
	}
}

func TestCountMessages_Fallback(t *testing.T) {
	e, _ := NewEstimator()
	msgs := []Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
	}
	// 未知 encoding → roughEstimate: 每条 4 + len/4 + len(role)/4, +2
	got := e.CountMessages(msgs, "nonexistent")
	want := 4 + len("hello")/4 + len("user")/4 + 4 + len("hi")/4 + len("assistant")/4 + 2
	if got != want {
		t.Errorf("expected %d, got %d", want, got)
	}
}

func TestCountMessages_Empty(t *testing.T) {
	e, _ := NewEstimator()
	// 空消息列表：仅对话格式开销 2（fallback 路径）
	got := e.CountMessages(nil, "nonexistent")
	if got != 2 {
		t.Errorf("expected 2, got %d", got)
	}
}
