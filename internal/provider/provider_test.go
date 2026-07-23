package provider

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"llm-gateway/internal/config"
)

// ==================== ConvertAnthropicToolChoiceToOpenAI ====================

func TestConvertAnthropicToolChoiceToOpenAI(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]interface{}
		want interface{}
	}{
		{"nil", nil, nil},
		{"auto", map[string]interface{}{"type": "auto"}, "auto"},
		{"any", map[string]interface{}{"type": "any"}, "required"},
		{"tool with name", map[string]interface{}{"type": "tool", "name": "get"}, map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "get"}}},
		{"tool without name", map[string]interface{}{"type": "tool"}, "required"},
		{"unknown", map[string]interface{}{"type": "weird"}, nil},
	}
	for _, c := range cases {
		got := ConvertAnthropicToolChoiceToOpenAI(c.in)
		switch w := c.want.(type) {
		case string:
			if got != w {
				t.Errorf("%s: expected %v, got %v", c.name, w, got)
			}
		case nil:
			if got != nil {
				t.Errorf("%s: expected nil, got %v", c.name, got)
			}
		default:
			// map 比较
			if got == nil {
				t.Errorf("%s: expected map, got nil", c.name)
			}
		}
	}
}

// ==================== convertOpenAIToolChoiceToAnthropic ====================

func TestConvertOpenAIToolChoiceToAnthropic(t *testing.T) {
	// 字符串
	if got := convertOpenAIToolChoiceToAnthropic("auto"); got["type"] != "auto" {
		t.Errorf("expected auto, got %v", got)
	}
	if got := convertOpenAIToolChoiceToAnthropic("none"); got["type"] != "none" {
		t.Errorf("expected none, got %v", got)
	}
	if got := convertOpenAIToolChoiceToAnthropic("required"); got["type"] != "any" {
		t.Errorf("expected any, got %v", got)
	}
	if got := convertOpenAIToolChoiceToAnthropic("garbage"); got["type"] != "auto" {
		t.Errorf("expected default auto, got %v", got)
	}
	// map function 带 name
	got := convertOpenAIToolChoiceToAnthropic(map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{"name": "get"},
	})
	if got["type"] != "tool" || got["name"] != "get" {
		t.Errorf("expected tool/get, got %v", got)
	}
	// 未知 map → auto
	if got := convertOpenAIToolChoiceToAnthropic(map[string]interface{}{"type": "x"}); got["type"] != "auto" {
		t.Errorf("expected default auto for unknown map, got %v", got)
	}
	// nil → nil
	if got := convertOpenAIToolChoiceToAnthropic(nil); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

// ==================== getDefaultPath / FullURL ====================

func TestGetDefaultPath(t *testing.T) {
	openai := NewProvider(config.ProviderConfig{BaseURL: "http://x", Protocol: "openai"})
	if got := openai.getDefaultPath(); got != "/chat/completions" {
		t.Errorf("expected /chat/completions, got %s", got)
	}
	anthropic := NewProvider(config.ProviderConfig{BaseURL: "http://x", Protocol: "anthropic"})
	if got := anthropic.getDefaultPath(); got != "/messages" {
		t.Errorf("expected /messages, got %s", got)
	}
}

func TestFullURL(t *testing.T) {
	p := NewProvider(config.ProviderConfig{BaseURL: "http://example.com", Protocol: "openai"})
	if got := p.FullURL(); got != "http://example.com/chat/completions" {
		t.Errorf("unexpected FullURL: %s", got)
	}
}

func TestGetProtocol(t *testing.T) {
	p := NewProvider(config.ProviderConfig{Protocol: "anthropic"})
	if p.GetProtocol() != ProtocolAnthropic {
		t.Errorf("expected anthropic protocol")
	}
}

// ==================== UpstreamHTTPError ====================

func TestUpstreamHTTPError_Error(t *testing.T) {
	e := &UpstreamHTTPError{StatusCode: 429, Provider: "p1", Body: []byte("rate")}
	msg := e.Error()
	if !strings.Contains(msg, "p1") || !strings.Contains(msg, "429") {
		t.Errorf("unexpected error message: %s", msg)
	}
}

// ==================== checkHTTPResponse ====================

func TestCheckHTTPResponse(t *testing.T) {
	// 2xx → 原样返回
	okResp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}
	if _, err := (&Provider{}).checkHTTPResponse(okResp); err != nil {
		t.Errorf("expected no error for 200, got %v", err)
	}
	// 4xx/5xx → UpstreamHTTPError
	for _, code := range []int{400, 429, 500} {
		resp := &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader("err"))}
		_, err := (&Provider{}).checkHTTPResponse(resp)
		ue, ok := err.(*UpstreamHTTPError)
		if !ok {
			t.Fatalf("code %d: expected UpstreamHTTPError, got %v", code, err)
		}
		if ue.StatusCode != code {
			t.Errorf("code %d: wrong status %d", code, ue.StatusCode)
		}
	}
}

// ==================== ConvertResponse (Anthropic→Anthropic passthrough w/ rewrite) ====================

func TestConvertResponse(t *testing.T) {
	body := []byte(`{"id":"msg_1","model":"claude-3","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":3}}`)
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body))}
	out, err := (&Provider{}).ConvertResponse(resp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"type":"message"`) {
		t.Errorf("expected message type, got %s", s)
	}
	if !strings.Contains(s, `"text":"hi"`) {
		t.Errorf("expected content text, got %s", s)
	}
}

func TestConvertResponse_InvalidJSON(t *testing.T) {
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader([]byte(`not json`)))}
	if _, err := (&Provider{}).ConvertResponse(resp); err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestConvertResponseWithModel(t *testing.T) {
	body := []byte(`{"id":"msg_1","model":"claude-3","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":1,"output_tokens":2}}`)
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body))}
	out, err := (&Provider{}).ConvertResponseWithModel(resp, "virtual-model")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(out), `"model":"virtual-model"`) {
		t.Errorf("expected virtual-model, got %s", string(out))
	}
}

// ==================== ConvertAnthropicToOpenAIResponse ====================

func TestConvertAnthropicToOpenAIResponse(t *testing.T) {
	body := []byte(`{"id":"msg_1","model":"claude-3","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":3}}`)
	out, err := (&Provider{}).ConvertAnthropicToOpenAIResponse(body, "virtual-model")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s := string(out)
	for _, want := range []string{`"object":"chat.completion"`, `"model":"virtual-model"`, `"finish_reason":"stop"`, `"content":"hi"`} {
		if !strings.Contains(s, want) {
			t.Errorf("expected %q in output, got %s", want, s)
		}
	}
}

func TestConvertAnthropicToOpenAIResponse_ToolUse(t *testing.T) {
	body := []byte(`{"id":"msg_1","content":[{"type":"tool_use","id":"tu1","name":"get","input":{"a":1}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":2}}`)
	out, err := (&Provider{}).ConvertAnthropicToOpenAIResponse(body, "vm")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"finish_reason":"tool_calls"`) {
		t.Errorf("expected tool_calls finish, got %s", s)
	}
	if !strings.Contains(s, `"name":"get"`) {
		t.Errorf("expected tool name, got %s", s)
	}
}

func TestConvertAnthropicToOpenAIResponse_EmptyID(t *testing.T) {
	body := []byte(`{"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":0,"output_tokens":0}}`)
	out, err := (&Provider{}).ConvertAnthropicToOpenAIResponse(body, "vm")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(out), "chatcmpl-") {
		t.Errorf("expected generated id with chatcmpl- prefix, got %s", string(out))
	}
}

// ==================== ConvertRequest ====================

func TestConvertRequest(t *testing.T) {
	req := &AnthropicRequest{
		Model:    "claude-3",
		Messages: []map[string]interface{}{{"role": "user", "content": "hi"}},
		MaxTokens: 100,
		Temperature: 0.5,
		Tools: []map[string]interface{}{
			{"name": "get", "description": "Get", "input_schema": map[string]interface{}{"type": "object"}},
		},
	}
	out, err := NewAnthropicConverter().ConvertRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out["model"] != "claude-3" {
		t.Errorf("expected model claude-3, got %v", out["model"])
	}
	if out["max_tokens"] != float64(100) {
		t.Errorf("expected max_tokens 100, got %v", out["max_tokens"])
	}
	if _, ok := out["messages"]; !ok {
		t.Errorf("expected messages field in converted request")
	}
}

// ==================== buildRequest ====================

func TestBuildRequest_OpenAIHeaders(t *testing.T) {
	p := NewProvider(config.ProviderConfig{BaseURL: "http://x", Protocol: "openai", APIKey: "sk-test"})
	req, err := p.buildRequest(context.Background(), "POST", "http://x/chat/completions", "gpt-4", nil, nil, ChatParams{Temperature: 0.7}, false)
	if err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Authorization") != "Bearer sk-test" {
		t.Errorf("expected Bearer auth, got %s", req.Header.Get("Authorization"))
	}
	if req.Header.Get("Content-Type") != "application/json" {
		t.Errorf("expected json content type")
	}
}

func TestBuildRequest_AnthropicHeaders(t *testing.T) {
	p := NewProvider(config.ProviderConfig{BaseURL: "http://x", Protocol: "anthropic", APIKey: "ak-test"})
	req, err := p.buildRequest(context.Background(), "POST", "http://x/messages", "claude", nil, nil, ChatParams{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("x-api-key") != "ak-test" {
		t.Errorf("expected x-api-key, got %s", req.Header.Get("x-api-key"))
	}
	if req.Header.Get("anthropic-version") != "2023-06-01" {
		t.Errorf("expected anthropic-version header")
	}
}

func TestBuildRequest_StreamOptions(t *testing.T) {
	p := NewProvider(config.ProviderConfig{BaseURL: "http://x", Protocol: "openai"})
	req, err := p.buildRequest(context.Background(), "POST", "http://x/chat/completions", "gpt-4", nil, nil, ChatParams{}, true)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(req.Body)
	if !strings.Contains(string(body), `"stream":true`) {
		t.Errorf("expected stream true, got %s", string(body))
	}
	if !strings.Contains(string(body), "include_usage") {
		t.Errorf("expected stream_options include_usage, got %s", string(body))
	}
}

// ==================== newBaseProvider ====================

func TestNewBaseProvider(t *testing.T) {
	baseURL, apiKey, proto, client := newBaseProvider(config.ProviderConfig{
		BaseURL: "http://x", APIKey: "k", Protocol: "anthropic",
	})
	if baseURL != "http://x" || apiKey != "k" || proto != ProtocolAnthropic {
		t.Errorf("unexpected base fields: %s %s %s", baseURL, apiKey, proto)
	}
	if client == nil {
		t.Error("expected non-nil http client")
	}

	// 默认响应头超时（protocol 为 openai，未设 response_header_timeout）
	_, _, proto2, client2 := newBaseProvider(config.ProviderConfig{BaseURL: "http://x", Protocol: "openai"})
	if proto2 != ProtocolOpenAI {
		t.Errorf("expected openai protocol")
	}
	if client2 == nil {
		t.Error("expected client")
	}
}

func TestNewBaseProvider_ResponseHeaderTimeout(t *testing.T) {
	_, _, _, client := newBaseProvider(config.ProviderConfig{
		BaseURL: "http://x", Protocol: "openai", ResponseHeaderTimeout: 5 * 1e9,
	})
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatal("expected *http.Transport")
	}
	if tr.ResponseHeaderTimeout.Seconds() != 5 {
		t.Errorf("expected 5s response header timeout, got %v", tr.ResponseHeaderTimeout)
	}
}
