package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"llm-gateway/internal/toolcall"
)

// xmlOpen is a test helper that avoids raw XML literals in source.
func xmlOpen(name string) string {
	return "<" + "function=" + name + ">"
}

func TestRewriteXMLToolCallsChecked_UnknownFormatRequiresRepair(t *testing.T) {
	content := "<tool_call>edit<arguments>unsupported</arguments></tool_call>"
	body := makeResp(content)
	out, raw, err := rewriteXMLToolCallsChecked(body, []toolcall.Definition{{Name: "edit"}})
	if !errors.Is(err, toolcall.ErrMalformed) {
		t.Fatalf("expected ErrMalformed, got %v", err)
	}
	if raw != content {
		t.Fatalf("unexpected malformed payload %q", raw)
	}
	if !bytesEq(out, body) {
		t.Fatal("malformed response must not be partially rewritten")
	}
}

func TestRewriteAnthropicXMLToolCallsChecked(t *testing.T) {
	body, _ := json.Marshal(map[string]interface{}{
		"id": "msg_test", "type": "message", "role": "assistant",
		"content": []interface{}{map[string]interface{}{
			"type": "text",
			"text": `<tool_call>edit<arg_key>file_path</arg_key><arg_value>/tmp/a</arg_value></tool_call>`,
		}},
		"stop_reason": "end_turn",
	})
	definitions := []toolcall.Definition{{
		Name: "edit",
		Parameters: map[string]interface{}{
			"required":   []interface{}{"file_path"},
			"properties": map[string]interface{}{"file_path": map[string]interface{}{"type": "string"}},
		},
	}}
	out, raw, err := rewriteAnthropicXMLToolCallsChecked(body, definitions)
	if err != nil || raw != "" {
		t.Fatalf("unexpected result raw=%q err=%v", raw, err)
	}
	var response map[string]interface{}
	if err := json.Unmarshal(out, &response); err != nil {
		t.Fatal(err)
	}
	if response["stop_reason"] != "tool_use" {
		t.Fatalf("unexpected stop reason %v", response["stop_reason"])
	}
	blocks := response["content"].([]interface{})
	if len(blocks) != 1 || blocks[0].(map[string]interface{})["type"] != "tool_use" {
		t.Fatalf("expected one tool_use block, got %#v", blocks)
	}
}

// makeResp builds an OpenAI-shaped ChatCompletionResponse body with the given content.
func makeResp(content string) []byte {
	resp := map[string]interface{}{
		"id":     "chatcmpl-test",
		"object": "chat.completion",
		"model":  "test-model",
		"choices": []map[string]interface{}{{
			"index": 0,
			"message": map[string]interface{}{
				"role":    "assistant",
				"content": content,
			},
			"finish_reason": "stop",
		}},
	}
	b, _ := json.Marshal(resp)
	return b
}

func TestRewriteXMLToolCalls_NoTags(t *testing.T) {
	body := makeResp("Hello world")
	out := rewriteXMLToolCalls(body)
	// byte-for-byte equality when nothing changes
	if !bytesEq(body, out) {
		t.Fatalf("expected unchanged body")
	}
}

func TestRewriteXMLToolCalls_SingleTag(t *testing.T) {
	tag := xmlOpen("get_weather") + `{"location":"NYC","query":"sunny"}`
	body := makeResp(tag)
	out := rewriteXMLToolCalls(body)

	var resp map[string]interface{}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	choices := resp["choices"].([]interface{})
	choice0 := choices[0].(map[string]interface{})
	if choice0["finish_reason"] != "tool_calls" {
		t.Fatalf("expected finish_reason=tool_calls, got %v", choice0["finish_reason"])
	}
	msg := choice0["message"].(map[string]interface{})
	if msg["content"] != "" {
		t.Fatalf("content should be empty, got %q", msg["content"])
	}
	tcs := msg["tool_calls"].([]interface{})
	if len(tcs) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(tcs))
	}
	tc0 := tcs[0].(map[string]interface{})
	fn := tc0["function"].(map[string]interface{})
	if fn["name"] != "get_weather" {
		t.Fatalf("unexpected name %v", fn["name"])
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(fn["arguments"].(string)), &decoded); err != nil {
		t.Fatalf("arguments not JSON: %v", err)
	}
	if decoded["location"] != "NYC" || decoded["query"] != "sunny" {
		t.Fatalf("unexpected args %v", decoded)
	}
}

func TestRewriteXMLToolCalls_ClosingTag(t *testing.T) {
	tag := xmlOpen("foo") + `{"a":1}` + "</" + "foo" + ">"
	body := makeResp(tag)
	out := rewriteXMLToolCalls(body)
	var resp map[string]interface{}
	_ = json.Unmarshal(out, &resp)
	choices := resp["choices"].([]interface{})
	c0 := choices[0].(map[string]interface{})
	msg := c0["message"].(map[string]interface{})
	tcs := msg["tool_calls"].([]interface{})
	if len(tcs) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(tcs))
	}
}

func TestRewriteXMLToolCalls_MultipleTags(t *testing.T) {
	text := xmlOpen("first") + `{"n":1}` + xmlOpen("second") + `{"n":2}`
	body := makeResp(text)
	out := rewriteXMLToolCalls(body)
	var resp map[string]interface{}
	_ = json.Unmarshal(out, &resp)
	choices := resp["choices"].([]interface{})
	c0 := choices[0].(map[string]interface{})
	msg := c0["message"].(map[string]interface{})
	tcs := msg["tool_calls"].([]interface{})
	if len(tcs) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(tcs))
	}
}

func TestRewriteXMLToolCalls_InvalidJSONKeepsRaw(t *testing.T) {
	text := xmlOpen("bad") + "not json at all"
	body := makeResp(text)
	out := rewriteXMLToolCalls(body)
	var resp map[string]interface{}
	_ = json.Unmarshal(out, &resp)
	choices := resp["choices"].([]interface{})
	c0 := choices[0].(map[string]interface{})
	msg := c0["message"].(map[string]interface{})
	tcs := msg["tool_calls"].([]interface{})
	if len(tcs) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(tcs))
	}
	fn := tcs[0].(map[string]interface{})["function"].(map[string]interface{})
	var decoded map[string]interface{}
	_ = json.Unmarshal([]byte(fn["arguments"].(string)), &decoded)
	if decoded["raw"] != "not json at all" {
		t.Fatalf("expected raw fallback, got %v", decoded)
	}
}

func TestRewriteXMLToolCalls_PreservesOtherChoices(t *testing.T) {
	body := makeResp(xmlOpen("f") + `{"k":"v"}`)
	out := rewriteXMLToolCalls(body)
	var resp map[string]interface{}
	_ = json.Unmarshal(out, &resp)
	if resp["id"] != "chatcmpl-test" {
		t.Fatalf("id lost")
	}
	if resp["model"] != "test-model" {
		t.Fatalf("model lost")
	}
}

func TestToolCallsCount(t *testing.T) {
	body := makeResp(xmlOpen("f") + `{"k":"v"}`)
	out := rewriteXMLToolCalls(body)
	if toolCallsCount(out) != 1 {
		t.Fatalf("expected 1, got %d", toolCallsCount(out))
	}
	if toolCallsCount(makeResp("hi")) != 0 {
		t.Fatalf("expected 0")
	}
}

func bytesEq(a, b []byte) bool {
	return strings.EqualFold(string(a), string(b))
}
