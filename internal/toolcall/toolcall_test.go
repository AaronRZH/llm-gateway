package toolcall

import (
	"encoding/json"
	"testing"
)

// xmlOpen builds an opening tag piece by piece to keep this test free of
// raw XML literals.
func xmlOpen(name string) string {
	return "<" + "function=" + name + ">"
}

func TestNormalize_NoTags(t *testing.T) {
	text := "Hello world"
	r := Normalize(text)
	if r.CleanContent != text {
		t.Fatalf("expected unchanged content, got %q", r.CleanContent)
	}
	if len(r.ToolCalls) != 0 {
		t.Fatalf("expected 0 tool calls, got %d", len(r.ToolCalls))
	}
}

func TestNormalize_SingleTagSingleLine(t *testing.T) {
	args := `{"location":"NYC","query":"sunny"}`
	text := xmlOpen("get_weather") + args
	r := Normalize(text)
	if r.CleanContent != "" {
		t.Fatalf("expected no leftover content, got %q", r.CleanContent)
	}
	if len(r.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(r.ToolCalls))
	}
	tc := r.ToolCalls[0]
	if tc.Function["name"] != "get_weather" {
		t.Fatalf("unexpected name %v", tc.Function["name"])
	}
	// arguments must be valid JSON equal to the original args object
	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(tc.Function["arguments"].(string)), &decoded); err != nil {
		t.Fatalf("arguments not valid JSON: %v", err)
	}
	if decoded["location"] != "NYC" || decoded["query"] != "sunny" {
		t.Fatalf("unexpected args %v", decoded)
	}
}

func TestNormalize_WithClosingTag(t *testing.T) {
	args := `{"a":1}`
	text := "prefix " + xmlOpen("foo") + args + "</" + "foo" + "> suffix"
	r := Normalize(text)
	if r.CleanContent != "prefix  suffix" {
		t.Fatalf("expected cleaned content, got %q", r.CleanContent)
	}
	if len(r.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(r.ToolCalls))
	}
	if r.ToolCalls[0].Function["name"] != "foo" {
		t.Fatalf("unexpected name %v", r.ToolCalls[0].Function["name"])
	}
}

func TestNormalize_MultipleTags(t *testing.T) {
	text := xmlOpen("first") + `{"n":1}` + xmlOpen("second") + `{"n":2}`
	r := Normalize(text)
	if len(r.ToolCalls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(r.ToolCalls))
	}
	if r.ToolCalls[0].Function["name"] != "first" || r.ToolCalls[1].Function["name"] != "second" {
		t.Fatalf("unexpected names: %v %v", r.ToolCalls[0].Function["name"], r.ToolCalls[1].Function["name"])
	}
}

func TestNormalize_InvalidJSONKeepsRaw(t *testing.T) {
	raw := "not json at all"
	text := xmlOpen("bad") + raw
	r := Normalize(text)
	if len(r.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(r.ToolCalls))
	}
	args, ok := r.ToolCalls[0].Function["arguments"].(string)
	if !ok {
		t.Fatalf("arguments missing")
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(args), &decoded); err != nil {
		t.Fatalf("expected fallback raw object, got err %v (val %q)", err, args)
	}
	if decoded["raw"] != raw {
		t.Fatalf("expected raw fallback, got %v", decoded)
	}
}

func TestNormalize_EmptyPayloadSkipped(t *testing.T) {
	text := "hi " + xmlOpen("a") + " " + xmlOpen("b") + `{"x":1}`
	r := Normalize(text)
	if len(r.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call (empty payload skipped), got %d", len(r.ToolCalls))
	}
	if r.ToolCalls[0].Function["name"] != "b" {
		t.Fatalf("expected the second tag to survive, got %v", r.ToolCalls[0].Function["name"])
	}
}

func TestToOpenAISlice(t *testing.T) {
	r := Normalize(xmlOpen("f") + `{"k":"v"}`)
	slice := r.ToOpenAISlice()
	if len(slice) != 1 {
		t.Fatalf("expected 1 item, got %d", len(slice))
	}
	fn, _ := slice[0]["function"].(map[string]interface{})
	if fn["name"] != "f" {
		t.Fatalf("unexpected name %v", fn["name"])
	}
}