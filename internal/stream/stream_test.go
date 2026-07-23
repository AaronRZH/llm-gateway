package stream

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ==================== escapeJSON ====================

func TestEscapeJSON(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`a"b`, `a\"b`},
		{`a\b`, `a\\b`},
		{"a\nb", `a\nb`},
		{"a\rb", `a\rb`},
		{"a\tb", `a\tb`},
		{"plain", "plain"},
	}
	for _, c := range cases {
		if got := escapeJSON(c.in); got != c.want {
			t.Errorf("escapeJSON(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ==================== rewriteModelField ====================

func TestRewriteModelField(t *testing.T) {
	h := New(0)
	// 普通 model 键替换
	out := h.rewriteModelField([]byte(`{"model":"gpt-4","x":1}`), "virt")
	if !strings.Contains(string(out), `"virt"`) {
		t.Errorf("expected virt, got %s", string(out))
	}
	if strings.Contains(string(out), "gpt-4") {
		t.Errorf("expected gpt-4 removed, got %s", string(out))
	}

	// model_id / model_name 不应被误替换
	out2 := h.rewriteModelField([]byte(`{"model_id":"x","model":"real"}`), "virt")
	if !strings.Contains(string(out2), `"virt"`) {
		t.Errorf("expected real model replaced, got %s", string(out2))
	}
	if !strings.Contains(string(out2), `"model_id":"x"`) {
		t.Errorf("expected model_id preserved, got %s", string(out2))
	}

	// 带空格的 model 键
	out3 := h.rewriteModelField([]byte(`{"model" : "real"}`), "virt")
	if !strings.Contains(string(out3), `"virt"`) {
		t.Errorf("expected virt with spaced key, got %s", string(out3))
	}

	// 无 model 键 → 原样返回
	orig := []byte(`{"id":"1","object":"x"}`)
	if got := h.rewriteModelField(orig, "virt"); string(got) != string(orig) {
		t.Errorf("expected unchanged, got %s", string(got))
	}
}

// ==================== extractContent ====================

func TestExtractContent(t *testing.T) {
	if got := extractContent([]byte(`{"choices":[{"delta":{"content":"hello"}}]}`)); got != "hello" {
		t.Errorf("expected hello, got %q", got)
	}
	// 无 choices
	if got := extractContent([]byte(`{"id":"1"}`)); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
	// 非法 JSON
	if got := extractContent([]byte(`{bad`)); got != "" {
		t.Errorf("expected empty for bad json, got %q", got)
	}
}

// ==================== extractToolCalls ====================

func TestExtractToolCalls(t *testing.T) {
	payload := []byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call1","type":"function","function":{"name":"get","arguments":"{\"a\":1}"}}]}}]}`)
	res := &StreamResult{}
	extractToolCalls(payload, res)
	if len(res.AccumulatedToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(res.AccumulatedToolCalls))
	}
	tc := res.AccumulatedToolCalls[0]
	if tc.ID != "call1" || tc.Function.Name != "get" || tc.Function.Arguments != `{"a":1}` {
		t.Errorf("unexpected tool call: %+v", tc)
	}
}

func TestExtractToolCalls_NoToolCalls(t *testing.T) {
	res := &StreamResult{}
	extractToolCalls([]byte(`{"choices":[{"delta":{"content":"hi"}}]}`), res)
	if len(res.AccumulatedToolCalls) != 0 {
		t.Errorf("expected 0, got %d", len(res.AccumulatedToolCalls))
	}
}

// ==================== extractUsage ====================

func TestExtractUsage_OpenAI(t *testing.T) {
	payload := []byte(`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	u := extractUsage(payload)
	if u == nil || u.PromptTokens != 10 || u.CompletionTokens != 5 || u.TotalTokens != 15 {
		t.Errorf("unexpected usage: %+v", u)
	}
}

func TestExtractUsage_AnthropicStart(t *testing.T) {
	payload := []byte(`{"type":"message_start","message":{"input_tokens":7,"output_tokens":3}}`)
	u := extractUsage(payload)
	if u == nil || u.PromptTokens != 7 {
		t.Errorf("expected prompt 7, got %+v", u)
	}
}

func TestExtractUsage_AnthropicDelta(t *testing.T) {
	payload := []byte(`{"type":"message_delta","usage":{"input_tokens":1,"output_tokens":2}}`)
	u := extractUsage(payload)
	if u == nil || u.CompletionTokens != 2 {
		t.Errorf("expected completion 2, got %+v", u)
	}
}

func TestExtractUsage_None(t *testing.T) {
	if u := extractUsage([]byte(`{"choices":[]}`)); u != nil {
		t.Errorf("expected nil, got %+v", u)
	}
}

// ==================== mergeUsage ====================

func TestMergeUsage(t *testing.T) {
	existing := &StreamUsage{PromptTokens: 5}
	mergeUsage(existing, &StreamUsage{CompletionTokens: 3, TotalTokens: 8})
	if existing.CompletionTokens != 3 || existing.TotalTokens != 8 {
		t.Errorf("unexpected merge: %+v", existing)
	}
	// nil incoming 安全
	mergeUsage(existing, nil)
	// 0 值不覆盖已有
	mergeUsage(existing, &StreamUsage{PromptTokens: 0, CompletionTokens: 0})
	if existing.PromptTokens != 5 {
		t.Errorf("expected 5 preserved, got %d", existing.PromptTokens)
	}
}

// ==================== Handler.ExtractToolCalls ====================

func TestHandlerExtractToolCalls(t *testing.T) {
	h := New(0)
	res := &StreamResult{
		AccumulatedToolCalls: []ToolCallChunk{
			{Index: 0, ID: "call1", Type: "function", Function: FunctionChunk{Name: "get", Arguments: `{"a":`}},
			{Index: 0, ID: "call1", Type: "function", Function: FunctionChunk{Name: "get", Arguments: `1}`}},
		},
	}
	tcs := h.ExtractToolCalls(res)
	if len(tcs) != 1 {
		t.Fatalf("expected 1 grouped tool call, got %d", len(tcs))
	}
	if tcs[0]["id"] != "call1" {
		t.Errorf("expected id call1, got %v", tcs[0]["id"])
	}
	fn, _ := tcs[0]["function"].(map[string]interface{})
	if fn["name"] != "get" {
		t.Errorf("expected name get, got %v", fn["name"])
	}
}

func TestHandlerExtractToolCalls_Empty(t *testing.T) {
	h := New(0)
	if tcs := h.ExtractToolCalls(&StreamResult{}); tcs != nil {
		t.Errorf("expected nil, got %v", tcs)
	}
}

// ==================== RewriteAndForward ====================

type flushWriter struct {
	buf    bytes.Buffer
	status int
	header http.Header
	flushed int
}

func (f *flushWriter) Header() http.Header {
	if f.header == nil {
		f.header = http.Header{}
	}
	return f.header
}
func (f *flushWriter) Write(p []byte) (int, error) { return f.buf.Write(p) }
func (f *flushWriter) WriteHeader(s int)            { f.status = s }
func (f *flushWriter) Flush()                       { f.flushed++ }

type rc struct{ io.Reader }

func (rc) Close() error { return nil }

func TestRewriteAndForward_OpenAI(t *testing.T) {
	h := New(0)
	upstream := rc{strings.NewReader(
		"data: {\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
			"data: [DONE]\n\n",
	)}
	fw := &flushWriter{}
	result, err := h.RewriteAndForward(fw, upstream, "virt", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := fw.buf.String()
	if !strings.Contains(out, `"virt"`) {
		t.Errorf("expected virt model rewritten, got %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Errorf("expected [DONE] forwarded, got %s", out)
	}
	if result.AccumulatedContent != "hi" {
		t.Errorf("expected accumulated 'hi', got %q", result.AccumulatedContent)
	}
}

func TestRewriteAndForward_AnthropicAbnormalEnd(t *testing.T) {
	h := New(0)
	// 读取数据后返回错误（模拟上游 stall）
	upstream := &errorAfterDataReader{
		data: "data: {\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n",
		err:  errors.New("stall"),
	}
	fw := &flushWriter{}
	_, err := h.RewriteAndForward(fw, upstream, "virt", false)
	if err == nil {
		t.Error("expected error on abnormal end")
	}
	out := fw.buf.String()
	// 异常结束应补发 Anthropic message_stop 终止符（openAIClient=false）
	if !strings.Contains(out, "message_stop") {
		t.Errorf("expected message_stop terminator, got %s", out)
	}
}

type errorAfterDataReader struct {
	data string
	err  error
	done bool
}

func (r *errorAfterDataReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	r.done = true
	return copy(p, r.data), nil
}
func (r *errorAfterDataReader) Close() error { return nil }

// sse 将若干 JSON 行包装为 SSE 流文本（每行 "data: <json>\n\n"）
func sse(lines ...string) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString("data: ")
		b.WriteString(l)
		b.WriteString("\n\n")
	}
	return b.String()
}

// ==================== AnthropicSSEConverter ====================

func TestAnthropicSSEConverter_Text(t *testing.T) {
	upstream := rc{strings.NewReader(
		"data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hello\"}}]}\n\n" +
			"data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" world\"},\"finish_reason\":\"stop\"}]}\n\n" +
			"data: [DONE]\n\n",
	)}
	c := NewAnthropicSSEConverter(upstream, "virt", 0)
	out, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("read error: %v", err)
	}
	s := string(out)
	for _, want := range []string{"message_start", "content_block_delta", "message_stop", `"model":"virt"`} {
		if !strings.Contains(s, want) {
			t.Errorf("expected %q in output, got:\n%s", want, s)
		}
	}
	c.Close()
}

func TestAnthropicSSEConverter_ToolUse(t *testing.T) {
	// OpenAI 风格 SSE，携带 tool_calls delta，期望 Anthropic 转换器输出
	// content_block_start(tool_use) + content_block_delta(input_json_delta)。
	// 注意：arguments 必须是合法 JSON 字符串，否则 json.Unmarshal 失败会被静默跳过。
	in := sse(
		`{"id":"1","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call1","type":"function","function":{"name":"get","arguments":"123"}}]}}]}`,
		`{"id":"1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`[DONE]`,
	)
	upstream := rc{strings.NewReader(in)}
	c := NewAnthropicSSEConverter(upstream, "virt", 0)
	out, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("read error: %v", err)
	}
	s := string(out)
	for _, want := range []string{"content_block_start", "tool_use", "input_json_delta", "call1", "get", "message_stop"} {
		if !strings.Contains(s, want) {
			t.Errorf("expected %q in output, got:\n%s", want, s)
		}
	}
	c.Close()
}

func TestAnthropicSSEConverter_ToolUseInvalidJSON(t *testing.T) {
	// 上游发送无法解析的非法 JSON 行，转换器应跳过该行且不 panic，
	// 仍以 message_stop 正常结束。
	in := sse(
		`{not valid json`,
		`{"id":"1","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`,
		`[DONE]`,
	)
	upstream := rc{strings.NewReader(in)}
	c := NewAnthropicSSEConverter(upstream, "virt", 0)
	out, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("read error: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "message_stop") {
		t.Errorf("expected message_stop terminator, got:\n%s", s)
	}
	c.Close()
}

// ==================== OpenAIStreamConverter ====================

func TestOpenAIStreamConverter_Text(t *testing.T) {
	upstream := rc{strings.NewReader(
		"data: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"model\":\"claude\",\"usage\":{\"input_tokens\":5}}}\n\n" +
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n" +
			"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n" +
			"data: {\"type\":\"message_stop\"}\n\n",
	)}
	c := NewOpenAIStreamConverter(upstream, "virt", 0)
	out, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("read error: %v", err)
	}
	s := string(out)
	for _, want := range []string{"chat.completion.chunk", "Hi", `"model":"virt"`, "data: [DONE]"} {
		if !strings.Contains(s, want) {
			t.Errorf("expected %q in output, got:\n%s", want, s)
		}
	}
	c.Close()
}

func TestOpenAIStreamConverter_PingForwarded(t *testing.T) {
	upstream := rc{strings.NewReader(
		": ping\n\n" +
			"data: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"model\":\"claude\"}}\n\n" +
			"data: {\"type\":\"message_stop\"}\n\n",
	)}
	c := NewOpenAIStreamConverter(upstream, "virt", 0)
	out, _ := io.ReadAll(c)
	s := string(out)
	if !strings.Contains(s, ": ping") {
		t.Errorf("expected ping comment forwarded, got:\n%s", s)
	}
	c.Close()
}

// ==================== IdleTimeoutReader ====================

func TestNewIdleTimeoutReader_NoTimeout(t *testing.T) {
	r := rc{strings.NewReader("hello")}
	got := NewIdleTimeoutReader(r, 0)
	if got != r {
		t.Error("expected same reader when timeout<=0")
	}
}

func TestNewIdleTimeoutReader_ReadAndClose(t *testing.T) {
	r := rc{strings.NewReader("hello world")}
	it := NewIdleTimeoutReader(r, 100*time.Millisecond)
	buf := make([]byte, 11)
	n, err := it.Read(buf)
	if err != nil || n != 11 {
		t.Fatalf("read: n=%d err=%v", n, err)
	}
	if string(buf) != "hello world" {
		t.Errorf("unexpected data: %q", string(buf))
	}
	// Close 幂等
	if err := it.Close(); err != nil {
		t.Errorf("first Close error: %v", err)
	}
	if err := it.Close(); err != nil {
		t.Errorf("second Close error: %v", err)
	}
}

// ==================== errWriter ====================

type failAfterN struct {
	n      int
	written int
}

func (f *failAfterN) Write(p []byte) (int, error) {
	f.written += len(p)
	if f.written > f.n {
		return 0, errors.New("write failed")
	}
	return len(p), nil
}

func TestErrWriter(t *testing.T) {
	fw := &failAfterN{n: 5}
	ew := &errWriter{w: fw}
	if _, err := ew.Write([]byte("abc")); err != nil {
		t.Fatalf("first write error: %v", err)
	}
	// 第二次写入超过阈值 → 记录错误
	if _, err := ew.Write([]byte("defg")); err == nil {
		t.Fatal("expected error from underlying writer")
	}
	if ew.err == nil {
		t.Error("expected err recorded")
	}
	// 后续写入直接返回已记录的错误
	if _, err := ew.Write([]byte("hij")); err == nil {
		t.Error("expected subsequent write to fail fast")
	}
}
