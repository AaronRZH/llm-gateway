package protocol

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/config"
	"llm-gateway/internal/provider"
	"llm-gateway/internal/router"
	"llm-gateway/internal/stream"
)

// assertSSEJSONValid 校验 SSE 输出中每个 data: 行的负载都是合法 JSON（[DONE] 终止符除外）。
// 用于防止拼接式生成 chunk 时产出结构损坏的 JSON。
func assertSSEJSONValid(t *testing.T, sse string) {
	t.Helper()
	for _, line := range strings.Split(sse, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		if !json.Valid([]byte(payload)) {
			t.Fatalf("SSE data line is not valid JSON: %s", payload)
		}
	}
}

// openAIRespBody 返回一个最小可用的 OpenAI 非流式响应体。
func openAIRespBody() []byte {
	return []byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"real-model","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
}

// anthropicRespBody 返回一个最小可用的 Anthropic 非流式响应体。
func anthropicRespBody() []byte {
	return []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"real-model","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`)
}

// resolveTarget 基于 httptest server 构造路由 Target（真实 provider.Provider，走真实 HTTP）。
func resolveTarget(t *testing.T, baseURL, protocol string) *router.Target {
	t.Helper()
	p := provider.NewProvider(config.ProviderConfig{
		BaseURL:  baseURL,
		APIKey:   "test-key",
		Protocol: protocol,
		Timeout:  5 * time.Second,
	})
	return &router.Target{
		Provider:     p,
		ProviderName: "testprov",
		Model:        "real-model",
		Timeout:      5 * time.Second,
	}
}

// newUpstream 启动一个返回固定状态码/响应体的上游 server，并记录最后命中的路径。
func newUpstream(t *testing.T, status int, contentType string, body []byte) (*httptest.Server, *string) {
	t.Helper()
	lastPath := new(string)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*lastPath = r.URL.Path
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, lastPath
}

func openAIReq(stream bool) *ChatCompletionRequest {
	return &ChatCompletionRequest{
		Model:    "virtual-model",
		Messages: []Message{{Role: "user", Content: "hi"}},
		Stream:   stream,
	}
}

func anthropicReq(stream bool) *AnthropicRequest {
	return &AnthropicRequest{
		Model:     "virtual-model",
		Messages:  []map[string]interface{}{{"role": "user", "content": "hi"}},
		MaxTokens: 128,
		Stream:    stream,
	}
}

// ==================== Case 1: OpenAI 客户端 → OpenAI 上游 ====================

func TestResolve_OpenAIToOpenAI_NonStream(t *testing.T) {
	srv, lastPath := newUpstream(t, http.StatusOK, "application/json", openAIRespBody())
	res, err := Resolve(Request{
		ClientProtocol: provider.ProtocolOpenAI,
		UpstreamTarget: resolveTarget(t, srv.URL, "openai"),
		ChatReq:        openAIReq(false),
		IsStream:       false,
		Ctx:            context.Background(),
		VirtualModel:   "virtual-model",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}
	if string(res.Body) != string(openAIRespBody()) {
		t.Fatalf("expected passthrough body, got %s", res.Body)
	}
	if *lastPath != "/chat/completions" {
		t.Fatalf("expected /chat/completions, got %s", *lastPath)
	}
}

func TestResolve_OpenAIToOpenAI_Stream(t *testing.T) {
	sse := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
	srv, _ := newUpstream(t, http.StatusOK, "text/event-stream", sse)
	res, err := Resolve(Request{
		ClientProtocol: provider.ProtocolOpenAI,
		UpstreamTarget: resolveTarget(t, srv.URL, "openai"),
		ChatReq:        openAIReq(true),
		IsStream:       true,
		Ctx:            context.Background(),
		VirtualModel:   "virtual-model",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.StreamBody == nil {
		t.Fatal("expected stream body")
	}
	got, _ := io.ReadAll(res.StreamBody)
	if !strings.Contains(string(got), "hi") {
		t.Fatalf("expected stream content, got %q", got)
	}
}

// ==================== Case 2: OpenAI 客户端 → Anthropic 上游 ====================

func TestResolve_OpenAIToAnthropic_NonStream(t *testing.T) {
	srv, lastPath := newUpstream(t, http.StatusOK, "application/json", anthropicRespBody())
	res, err := Resolve(Request{
		ClientProtocol: provider.ProtocolOpenAI,
		UpstreamTarget: resolveTarget(t, srv.URL, "anthropic"),
		ChatReq:        openAIReq(false),
		IsStream:       false,
		Ctx:            context.Background(),
		VirtualModel:   "virtual-model",
		StreamHandler:  stream.New(0),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}
	// Anthropic 响应应被转换为 OpenAI 格式。
	if !strings.Contains(string(res.Body), `"choices"`) {
		t.Fatalf("expected converted OpenAI response, got %s", res.Body)
	}
	if *lastPath != "/messages" {
		t.Fatalf("expected /messages, got %s", *lastPath)
	}
}

func TestResolve_OpenAIToAnthropic_Stream(t *testing.T) {
	// 上游需提供完整 content_block_delta 事件，转换器才会输出内容 delta。
	// （仅有 message_start + message_stop 时转换器只发送 [DONE]，无内容可转发。）
	sse := []byte(
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)
	srv, _ := newUpstream(t, http.StatusOK, "text/event-stream", sse)
	res, err := Resolve(Request{
		ClientProtocol: provider.ProtocolOpenAI,
		UpstreamTarget: resolveTarget(t, srv.URL, "anthropic"),
		ChatReq:        openAIReq(true),
		IsStream:       true,
		Ctx:            context.Background(),
		VirtualModel:   "virtual-model",
		StreamHandler:  stream.New(0),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.StreamBody == nil {
		t.Fatal("expected converted stream body")
	}
	got, err := io.ReadAll(res.StreamBody)
	if err != nil {
		t.Fatalf("reading converted stream failed: %v", err)
	}
	// 断言 OpenAI 格式的关键内容：内容增量、模型名重写为虚拟模型、以及 [DONE] 终止。
	for _, want := range []string{"chat.completion.chunk", `"content":"hi"`, `"model":"virtual-model"`, "[DONE]"} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("expected %q in converted stream, got %q", want, got)
		}
	}
	// Case 2 由 OpenAIStreamConverter 以字符串拼接生成 chunk，回归点在于拼接出的 JSON 必须合法。
	assertSSEJSONValid(t, string(got))
}

// ==================== Case 3: Anthropic 客户端 → OpenAI 上游 ====================

func TestResolve_AnthropicToOpenAI_NonStream(t *testing.T) {
	srv, lastPath := newUpstream(t, http.StatusOK, "application/json", openAIRespBody())
	res, err := Resolve(Request{
		ClientProtocol: provider.ProtocolAnthropic,
		UpstreamTarget: resolveTarget(t, srv.URL, "openai"),
		AnthropicReq:   anthropicReq(false),
		IsStream:       false,
		Ctx:            context.Background(),
		VirtualModel:   "virtual-model",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}
	if !strings.Contains(string(res.Body), `"type":"message"`) {
		t.Fatalf("expected converted Anthropic response, got %s", res.Body)
	}
	if *lastPath != "/chat/completions" {
		t.Fatalf("expected /chat/completions, got %s", *lastPath)
	}
}

func TestResolve_AnthropicToOpenAI_Stream(t *testing.T) {
	sse := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
	srv, _ := newUpstream(t, http.StatusOK, "text/event-stream", sse)
	res, err := Resolve(Request{
		ClientProtocol: provider.ProtocolAnthropic,
		UpstreamTarget: resolveTarget(t, srv.URL, "openai"),
		AnthropicReq:   anthropicReq(true),
		IsStream:       true,
		Ctx:            context.Background(),
		VirtualModel:   "virtual-model",
		StreamHandler:  stream.New(0),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.StreamBody == nil {
		t.Fatal("expected converted stream body")
	}
	got, err := io.ReadAll(res.StreamBody)
	if err != nil {
		t.Fatalf("reading converted stream failed: %v", err)
	}
	out := string(got)
	// 断言 Anthropic SSE 的关键结构：文本块生命周期、增量内容、结束事件。
	for _, want := range []string{"content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop", `"text":"hi"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in converted Anthropic stream, got %q", want, out)
		}
	}
	// 事件顺序：start < delta < stop < message_stop
	startIdx := strings.Index(out, "content_block_start")
	deltaIdx := strings.Index(out, "content_block_delta")
	stopIdx := strings.Index(out, "content_block_stop")
	msgStopIdx := strings.Index(out, "message_stop")
	if startIdx < 0 || deltaIdx < 0 || stopIdx < 0 || msgStopIdx < 0 ||
		startIdx > deltaIdx || deltaIdx > stopIdx || stopIdx > msgStopIdx {
		t.Fatalf("expected ordered Anthropic SSE events (start<delta<stop<message_stop), got %q", out)
	}
	// 每个 data 行都应是合法 JSON（[DONE] 除外）
	assertSSEJSONValid(t, out)
}

// ==================== Case 4: Anthropic 客户端 → Anthropic 上游 ====================

func TestResolve_AnthropicToAnthropic_NonStream(t *testing.T) {
	srv, lastPath := newUpstream(t, http.StatusOK, "application/json", anthropicRespBody())
	res, err := Resolve(Request{
		ClientProtocol: provider.ProtocolAnthropic,
		UpstreamTarget: resolveTarget(t, srv.URL, "anthropic"),
		AnthropicReq:   anthropicReq(false),
		ExtraParams:    map[string]interface{}{"max_tokens": 128},
		IsStream:       false,
		Ctx:            context.Background(),
		VirtualModel:   "virtual-model",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}
	if string(res.Body) != string(anthropicRespBody()) {
		t.Fatalf("expected passthrough body, got %s", res.Body)
	}
	if *lastPath != "/messages" {
		t.Fatalf("expected /messages, got %s", *lastPath)
	}
}

func TestResolve_AnthropicToAnthropic_Stream(t *testing.T) {
	// Case 4 为同协议透传，上游事件应原样到达客户端（含 event: 行）。
	sse := []byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	srv, _ := newUpstream(t, http.StatusOK, "text/event-stream", sse)
	res, err := Resolve(Request{
		ClientProtocol: provider.ProtocolAnthropic,
		UpstreamTarget: resolveTarget(t, srv.URL, "anthropic"),
		AnthropicReq:   anthropicReq(true),
		ExtraParams:    map[string]interface{}{"max_tokens": 128},
		IsStream:       true,
		Ctx:            context.Background(),
		VirtualModel:   "virtual-model",
		StreamHandler:  stream.New(0),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.StreamBody == nil {
		t.Fatal("expected stream body")
	}
	got, err := io.ReadAll(res.StreamBody)
	if err != nil {
		t.Fatalf("reading stream failed: %v", err)
	}
	out := string(got)
	// 透传路径应保留上游的 event 名与 data 负载。
	if !strings.Contains(out, "event: message_start") {
		t.Fatalf("expected passthrough event: message_start, got %q", out)
	}
	if !strings.Contains(out, "event: message_stop") {
		t.Fatalf("expected passthrough event: message_stop, got %q", out)
	}
	assertSSEJSONValid(t, out)
}

// ==================== 错误语义 ====================

func TestResolve_RateLimited429_NotTreatedAsBreakerError(t *testing.T) {
	srv, _ := newUpstream(t, http.StatusTooManyRequests, "application/json", []byte(`{"error":{"retry_after":1}}`))
	res, err := Resolve(Request{
		ClientProtocol: provider.ProtocolOpenAI,
		UpstreamTarget: resolveTarget(t, srv.URL, "openai"),
		ChatReq:        openAIReq(false),
		IsStream:       false,
		Ctx:            context.Background(),
		VirtualModel:   "virtual-model",
	})
	if err != nil {
		t.Fatalf("429 must not surface as breaker error, got %v", err)
	}
	if res == nil || res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 result, got %+v", res)
	}
}

func TestResolve_Upstream5xx_ReturnsError(t *testing.T) {
	srv, _ := newUpstream(t, http.StatusInternalServerError, "application/json", []byte(`{"error":"boom"}`))
	res, err := Resolve(Request{
		ClientProtocol: provider.ProtocolOpenAI,
		UpstreamTarget: resolveTarget(t, srv.URL, "openai"),
		ChatReq:        openAIReq(false),
		IsStream:       false,
		Ctx:            context.Background(),
		VirtualModel:   "virtual-model",
	})
	if err == nil {
		t.Fatal("expected breaker-counted error for 5xx")
	}
	if res != nil {
		t.Fatalf("expected nil result, got %+v", res)
	}
	if ue, ok := err.(*provider.UpstreamHTTPError); !ok || ue.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected UpstreamHTTPError 500, got %T %v", err, err)
	}
}

func TestResolve_UpstreamUnreachable_ReturnsError(t *testing.T) {
	target := resolveTarget(t, "http://127.0.0.1:1", "openai")
	res, err := Resolve(Request{
		ClientProtocol: provider.ProtocolOpenAI,
		UpstreamTarget: target,
		ChatReq:        openAIReq(false),
		IsStream:       false,
		Ctx:            context.Background(),
		VirtualModel:   "virtual-model",
	})
	if err == nil {
		t.Fatal("expected connection error")
	}
	if res != nil {
		t.Fatalf("expected nil result, got %+v", res)
	}
}

// buildReq 根据客户端协议填充 ChatReq 或 AnthropicReq，覆盖 Resolve 的入口分支。
func buildReq(client provider.ClientProtocol, target *router.Target, isStream bool) Request {
	r := Request{
		ClientProtocol: client,
		UpstreamTarget: target,
		IsStream:       isStream,
		Ctx:            context.Background(),
		VirtualModel:   "virtual-model",
		StreamHandler:  stream.New(0),
		ExtraParams:    map[string]interface{}{"max_tokens": 128},
	}
	if client == provider.ProtocolOpenAI {
		r.ChatReq = openAIReq(isStream)
	} else {
		r.AnthropicReq = anthropicReq(isStream)
	}
	return r
}

// TestResolve_UpstreamError_AllCases 覆盖 4 种协议组合在流式/非流式下的 5xx 错误分支。
func TestResolve_UpstreamError_AllCases(t *testing.T) {
	cases := []struct {
		name     string
		client   provider.ClientProtocol
		upstream string
		stream   bool
	}{
		{"case1-nonstream", provider.ProtocolOpenAI, "openai", false},
		{"case1-stream", provider.ProtocolOpenAI, "openai", true},
		{"case2-nonstream", provider.ProtocolOpenAI, "anthropic", false},
		{"case2-stream", provider.ProtocolOpenAI, "anthropic", true},
		{"case3-nonstream", provider.ProtocolAnthropic, "openai", false},
		{"case3-stream", provider.ProtocolAnthropic, "openai", true},
		{"case4-nonstream", provider.ProtocolAnthropic, "anthropic", false},
		{"case4-stream", provider.ProtocolAnthropic, "anthropic", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newUpstream(t, http.StatusInternalServerError, "application/json", []byte(`{"error":"boom"}`))
			res, err := Resolve(buildReq(tc.client, resolveTarget(t, srv.URL, tc.upstream), tc.stream))
			if err == nil {
				t.Fatal("expected breaker-counted error")
			}
			if res != nil {
				t.Fatalf("expected nil result, got %+v", res)
			}
		})
	}
}

// TestResolve_OpenAIClientWithAnthropicReq_ChatFallback 覆盖 ClientProtocol=openai 但仅提供
// AnthropicReq 的非标准路径（源码中 req.ChatReq == nil 分支），流式与非流式各一条。
func TestResolve_OpenAIClientWithAnthropicReq_ChatFallback(t *testing.T) {
	t.Run("nonstream", func(t *testing.T) {
		srv, lastPath := newUpstream(t, http.StatusOK, "application/json", openAIRespBody())
		req := buildReq(provider.ProtocolOpenAI, resolveTarget(t, srv.URL, "openai"), false)
		req.ChatReq = nil
		req.AnthropicReq = anthropicReq(false)
		res, err := Resolve(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", res.StatusCode)
		}
		if *lastPath != "/chat/completions" {
			t.Fatalf("expected /chat/completions, got %s", *lastPath)
		}
	})

	t.Run("stream", func(t *testing.T) {
		sse := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
		srv, _ := newUpstream(t, http.StatusOK, "text/event-stream", sse)
		req := buildReq(provider.ProtocolOpenAI, resolveTarget(t, srv.URL, "openai"), true)
		req.ChatReq = nil
		req.AnthropicReq = anthropicReq(true)
		res, err := Resolve(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.StreamBody == nil {
			t.Fatal("expected stream body")
		}
		_, _ = io.ReadAll(res.StreamBody)
	})
}

// TestResolve_ConverterFallback_InvalidJSON 覆盖上游返回非法 JSON 时转换失败、回退原始 body 的兜底路径。
func TestResolve_ConverterFallback_InvalidJSON(t *testing.T) {
	// Case2 非流式：Anthropic 上游返回非法 JSON。
	srv, _ := newUpstream(t, http.StatusOK, "application/json", []byte("not json"))
	res, err := Resolve(buildReq(provider.ProtocolOpenAI, resolveTarget(t, srv.URL, "anthropic"), false))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(res.Body) != "not json" {
		t.Fatalf("expected raw body fallback, got %s", res.Body)
	}

	// Case3 非流式：OpenAI 上游返回非法 JSON。
	srv2, _ := newUpstream(t, http.StatusOK, "application/json", []byte("not json"))
	res2, err := Resolve(buildReq(provider.ProtocolAnthropic, resolveTarget(t, srv2.URL, "openai"), false))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(res2.Body) != "not json" {
		t.Fatalf("expected raw body fallback, got %s", res2.Body)
	}
}
