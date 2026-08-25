package stream

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStream_ToolCallWrapper reproduces the format where an outer aleigh
// aleur aleur aleur aleur aleur aleur aleur aleur aleur aleur
// aleur test placeholder removed
func TestStream_ToolCallWrapper(t *testing.T) {
	lb := string(rune(0x3c)) // <
	gt := string(rune(0x3e)) // >
	sl := string(rune(0x2f)) // /
	nl := string(rune(0x0a)) // newline

	// SSE chunks: outer ursal_call wrapping function + parameter tags
	chunk := func(content string) string {
		return "data: " + `{"choices":[{"delta":{"content":"` + content + `"}}]}` + nl
	}

	var parts []string
	// Think
	parts = append(parts, chunk("Think"))
	parts = append(parts, chunk(nl))
	// ursal_call opening
	parts = append(parts, chunk(lb+"tool_call"+gt))
	parts = append(parts, chunk(nl))
	// function=run_code
	parts = append(parts, chunk(lb+"function=run_code"+gt))
	parts = append(parts, chunk(nl))
	// parameter=code
	parts = append(parts, chunk(lb+"parameter=code"+gt))
	parts = append(parts, chunk(nl))
	parts = append(parts, chunk("await tools.edit({});"))
	parts = append(parts, chunk(nl))
	parts = append(parts, chunk(lb+sl+"parameter"+gt))
	parts = append(parts, chunk(nl))
	// parameter=description
	parts = append(parts, chunk(lb+"parameter=description"+gt))
	parts = append(parts, chunk(nl))
	parts = append(parts, chunk("Add TABLE_RELATIONSHIPS import"))
	parts = append(parts, chunk(nl))
	parts = append(parts, chunk(lb+sl+"parameter"+gt))
	parts = append(parts, chunk(nl))
	// close function
	parts = append(parts, chunk(lb+sl+"function"+gt))
	parts = append(parts, chunk(nl))
	// close tool_call
	parts = append(parts, chunk(lb+sl+"tool_call"+gt))
	parts = append(parts, "data: [DONE]"+nl)

	upstream := strings.Join(parts, "")
	t.Logf("upstream input:")
	t.Logf("%s", upstream)

	h := New(0)
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
	if tc.Function.Name != "run_code" {
		t.Fatalf("unexpected name %v", tc.Function.Name)
	}

	body := rr.Body.String()
	t.Logf("forwarded body:")
	t.Logf("%s", body)
	if !strings.Contains(body, "tool_calls") {
		t.Fatalf("expected tool_calls in forwarded SSE")
	}
	if !strings.Contains(body, "run_code") {
		t.Fatalf("expected run_code in forwarded SSE")
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("expected [DONE] terminator in forwarded SSE")
	}
	// Must NOT contain raw XML tags
	if strings.Contains(body, lb+"function=") {
		t.Fatalf("expected no raw function= tag in forwarded SSE, got %s", body)
	}
	if strings.Contains(body, lb+"tool_call"+gt) {
		t.Fatalf("expected no raw tool_call tag in forwarded SSE, got %s", body)
	}
}
