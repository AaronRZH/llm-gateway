package stream

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"llm-gateway/internal/toolcall"
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

func TestStream_UnknownToolFormatRepairsOnce(t *testing.T) {
	unknown := "<tool_call>edit<arguments>not-supported</arguments></tool_call>"
	chunk, _ := json.Marshal(map[string]interface{}{
		"choices": []interface{}{map[string]interface{}{
			"delta": map[string]interface{}{"content": unknown},
		}},
	})
	upstream := "data: " + string(chunk) + "\n\ndata: [DONE]\n\n"

	definitions := []toolcall.Definition{{Name: "edit"}}
	repairCount := 0
	repair := func(raw string) ([]map[string]interface{}, error) {
		repairCount++
		if raw != unknown {
			t.Fatalf("unexpected repair input %q", raw)
		}
		return []map[string]interface{}{{
			"id": "call_repaired", "type": "function",
			"function": map[string]interface{}{"name": "edit", "arguments": `{"file_path":"/tmp/a"}`},
		}}, nil
	}

	h := New(0)
	rr := httptest.NewRecorder()
	result, err := h.RewriteAndForwardWithToolRepair(
		rr, io.NopCloser(strings.NewReader(upstream)), "virt", true, definitions, repair,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if repairCount != 1 {
		t.Fatalf("expected exactly one repair, got %d", repairCount)
	}
	if len(result.AccumulatedToolCalls) != 1 || result.AccumulatedToolCalls[0].Function.Name != "edit" {
		t.Fatalf("unexpected repaired calls: %#v", result.AccumulatedToolCalls)
	}
	body := rr.Body.String()
	if strings.Contains(body, unknown) || !strings.Contains(body, `"tool_calls"`) || !strings.Contains(body, "call_repaired") {
		t.Fatalf("unexpected forwarded body: %s", body)
	}
}

func TestStream_UnknownToolFormatFailsExplicitlyWithoutRepair(t *testing.T) {
	unknown := "<tool_call>edit<arguments>not-supported</arguments></tool_call>"
	chunk, _ := json.Marshal(map[string]interface{}{
		"choices": []interface{}{map[string]interface{}{
			"delta": map[string]interface{}{"content": unknown},
		}},
	})
	upstream := "data: " + string(chunk) + "\n\ndata: [DONE]\n\n"

	h := New(0)
	rr := httptest.NewRecorder()
	_, err := h.RewriteAndForwardWithToolRepair(
		rr, io.NopCloser(strings.NewReader(upstream)), "virt", true,
		[]toolcall.Definition{{Name: "edit"}}, nil,
	)
	if err == nil {
		t.Fatal("expected malformed tool-call error")
	}
	body := rr.Body.String()
	if strings.Contains(body, unknown) || !strings.Contains(body, "malformed_tool_call") {
		t.Fatalf("expected explicit error without raw replay, got %s", body)
	}
}

func TestStream_NativeToolCallIsBufferedAndValidated(t *testing.T) {
	chunks := []map[string]interface{}{
		{"choices": []interface{}{map[string]interface{}{"delta": map[string]interface{}{
			"tool_calls": []interface{}{map[string]interface{}{
				"index": 0, "id": "call_native", "type": "function",
				"function": map[string]interface{}{"name": "edit", "arguments": `{"file_`},
			}},
		}}}},
		{"choices": []interface{}{map[string]interface{}{"delta": map[string]interface{}{
			"tool_calls": []interface{}{map[string]interface{}{
				"index": 0, "function": map[string]interface{}{"arguments": `path":"/tmp/a"}`},
			}},
		}}}},
	}
	var upstream strings.Builder
	for _, chunk := range chunks {
		data, _ := json.Marshal(chunk)
		upstream.WriteString("data: " + string(data) + "\n\n")
	}
	upstream.WriteString("data: [DONE]\n\n")

	h := New(0)
	rr := httptest.NewRecorder()
	result, err := h.RewriteAndForwardWithToolRepair(
		rr, io.NopCloser(strings.NewReader(upstream.String())), "virt", true,
		[]toolcall.Definition{{Name: "edit", Parameters: map[string]interface{}{
			"required":   []interface{}{"file_path"},
			"properties": map[string]interface{}{"file_path": map[string]interface{}{"type": "string"}},
		}}}, nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.AccumulatedToolCalls) != 2 {
		t.Fatalf("expected two accumulated chunks, got %d", len(result.AccumulatedToolCalls))
	}
	body := rr.Body.String()
	if strings.Count(body, `"tool_calls":[`) != 1 || !strings.Contains(body, "call_native") {
		t.Fatalf("expected one consolidated tool-call chunk, got %s", body)
	}
}
