package stream

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

var (
	lb2 = string(rune(0x3c)) // <
	gt2 = string(rune(0x3e)) // >
	sl2 = string(rune(0x2f)) // /
	tb2 = string(rune(0x09)) // tab
	nl2 = string(rune(0x0a)) // newline
)

func sseChunk(content string) string {
	return "data: " + `{"choices":[{"delta":{"content":"` + content + `"}}]}` + nl2
}

func TestStream_InvokeClarifyXML(t *testing.T) {
	h := New(0)

	var parts []string
	parts = append(parts, sseChunk(lb2+"invoke name=add_clarification"+gt2))
	parts = append(parts, sseChunk(tb2+lb2+"parameter=question"+gt2))
	parts = append(parts, sseChunk("gaolinzhi"))
	parts = append(parts, sseChunk(lb2+sl2+"parameter"+gt2+tb2))
	parts = append(parts, sseChunk(lb2+"parameter=next_action"+gt2))
	parts = append(parts, sseChunk("await_user_confirmation"+lb2+sl2+"parameter"+gt2))
	parts = append(parts, sseChunk(lb2+sl2+"invoke"+gt2))
	parts = append(parts, "data: [DONE]\n")
	upstream := strings.Join(parts, "")

	t.Logf("upstream input:\n%s", upstream)

	rr := httptest.NewRecorder()
	result, err := h.RewriteAndForward(rr, io.NopCloser(strings.NewReader(upstream)), "virt", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Logf("AccumulatedContent=%q ToolCalls=%d", result.AccumulatedContent, len(result.AccumulatedToolCalls))
	if len(result.AccumulatedToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(result.AccumulatedToolCalls))
	}
	tc := result.AccumulatedToolCalls[0]
	if tc.Function.Name != "add_clarification" {
		t.Fatalf("unexpected name %v", tc.Function.Name)
	}
	t.Logf("args=%q", tc.Function.Arguments)

	body := rr.Body.String()
	t.Logf("forwarded body:\n%s", body)
	if !strings.Contains(body, "tool_calls") {
		t.Fatalf("expected tool_calls in forwarded SSE")
	}
	if !strings.Contains(body, "add_clarification") {
		t.Fatalf("expected add_clarification in forwarded SSE")
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("expected [DONE] terminator in forwarded SSE")
	}
}
