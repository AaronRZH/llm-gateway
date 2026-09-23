package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"llm-gateway/internal/config"
	"llm-gateway/internal/mapper"
	"llm-gateway/internal/provider"
	"llm-gateway/internal/router"
	"llm-gateway/internal/stream"
	"llm-gateway/internal/token"
)

// ==================== 测试脚手架 ====================

const testVirtualModel = "vm"

func charOpenAIResp() string {
	return `{"id":"chatcmpl-1","object":"chat.completion","model":"real-model","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`
}

func charAnthropicResp() string {
	return `{"id":"msg_1","type":"message","role":"assistant","model":"real-model","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`
}

// newCharUpstream 启动固定返回状态码/响应体的上游 server。
func newCharUpstream(t *testing.T, status int, contentType, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// charStack 构建一套最小可用的 handler 依赖栈。
// managerProvider 是注册到 provider.Manager 的名称，routeProvider 是 real_models 中引用的名称；
// 二者不一致可用于模拟"路由到不存在的 provider"。
func charStack(t *testing.T, upstreamURL, upstreamProto, managerProvider, routeProvider string) (*mapper.Service, *router.Service, *stream.Handler, *token.Service) {
	t.Helper()
	pm := provider.NewManager(map[string]config.ProviderConfig{
		managerProvider: {BaseURL: upstreamURL, Protocol: upstreamProto, Timeout: 5 * time.Second},
	})
	tokenSvc := token.New(config.TokenConfig{})
	cbCfg := config.CircuitBreakerConfig{MaxRequests: 3, Interval: 10 * time.Second, Timeout: 5 * time.Second, FailureThreshold: 5, Cooldown: 30 * time.Second}
	rmCfg := config.RealModelsConfig{
		Strategy: "priority",
		Models: []config.FallbackItem{
			{Provider: routeProvider, Model: "real-model", Weight: 1, Timeout: 5 * time.Second},
		},
	}
	routerSvc := router.New(rmCfg, pm, tokenSvc, cbCfg, nil)
	streamHandler := stream.New(2 * time.Second)
	mapperSvc := mapper.New([]config.ModelEntry{{Name: testVirtualModel}})
	return mapperSvc, routerSvc, streamHandler, tokenSvc
}

// charInvoke 直接调用 gin handler，返回响应记录器。
func charInvoke(h gin.HandlerFunc, path, body string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("api_key", "test-key")
	h(c)
	return w
}

// ==================== handleChatCompletion ====================

func TestHandleChatCompletion_NonStream_Success(t *testing.T) {
	srv := newCharUpstream(t, http.StatusOK, "application/json", charOpenAIResp())
	m, r, s, tok := charStack(t, srv.URL, "openai", "prov", "prov")
	h := handleChatCompletion(m, r, s, tok, 5*time.Second)

	w := charInvoke(h, "/v1/chat/completions", `{"model":"vm","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"model":"vm"`) {
		t.Fatalf("expected model rewritten to virtual model, got %s", w.Body.String())
	}
}

func TestHandleChatCompletion_ModelNotAllowed(t *testing.T) {
	srv := newCharUpstream(t, http.StatusOK, "application/json", charOpenAIResp())
	m, r, s, tok := charStack(t, srv.URL, "openai", "prov", "prov")
	h := handleChatCompletion(m, r, s, tok, 5*time.Second)

	w := charInvoke(h, "/v1/chat/completions", `{"model":"unknown","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHandleChatCompletion_InvalidJSON(t *testing.T) {
	srv := newCharUpstream(t, http.StatusOK, "application/json", charOpenAIResp())
	m, r, s, tok := charStack(t, srv.URL, "openai", "prov", "prov")
	h := handleChatCompletion(m, r, s, tok, 5*time.Second)

	w := charInvoke(h, "/v1/chat/completions", `{`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleChatCompletion_NoAvailableModel(t *testing.T) {
	srv := newCharUpstream(t, http.StatusOK, "application/json", charOpenAIResp())
	// real_models 引用不存在的 provider → 无候选 → 503。
	m, r, s, tok := charStack(t, srv.URL, "openai", "present", "absent")
	h := handleChatCompletion(m, r, s, tok, 5*time.Second)

	w := charInvoke(h, "/v1/chat/completions", `{"model":"vm","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "no available model") {
		t.Fatalf("expected 'no available model', got %s", w.Body.String())
	}
}

func TestHandleChatCompletion_NonStream_UpstreamError(t *testing.T) {
	srv := newCharUpstream(t, http.StatusInternalServerError, "application/json", `{"error":"boom"}`)
	m, r, s, tok := charStack(t, srv.URL, "openai", "prov", "prov")
	h := handleChatCompletion(m, r, s, tok, 5*time.Second)

	w := charInvoke(h, "/v1/chat/completions", `{"model":"vm","messages":[{"role":"user","content":"hi"}]}`)
	// 非流式路径会把上游最后一个错误状态码与 body 透传给客户端。
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 passthrough, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "boom") {
		t.Fatalf("expected upstream error body, got %s", w.Body.String())
	}
}

func TestHandleChatCompletion_Stream_Success(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	srv := newCharUpstream(t, http.StatusOK, "text/event-stream", sse)
	m, r, s, tok := charStack(t, srv.URL, "openai", "prov", "prov")
	h := handleChatCompletion(m, r, s, tok, 5*time.Second)

	w := charInvoke(h, "/v1/chat/completions", `{"model":"vm","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "data:") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("expected SSE stream with DONE terminator, got %q", body)
	}
}

// ==================== handleAnthropicMessages ====================

func TestHandleAnthropicMessages_NonStream_Success(t *testing.T) {
	// Anthropic 客户端 → OpenAI 上游（Case 3）。
	srv := newCharUpstream(t, http.StatusOK, "application/json", charOpenAIResp())
	m, r, s, tok := charStack(t, srv.URL, "openai", "prov", "prov")
	h := handleAnthropicMessages(m, r, s, tok, 5*time.Second)

	w := charInvoke(h, "/v1/messages", `{"model":"vm","messages":[{"role":"user","content":"hi"}],"max_tokens":128}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"type":"message"`) {
		t.Fatalf("expected converted Anthropic response, got %s", w.Body.String())
	}
}

func TestHandleAnthropicMessages_Stream_Success(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	srv := newCharUpstream(t, http.StatusOK, "text/event-stream", sse)
	m, r, s, tok := charStack(t, srv.URL, "openai", "prov", "prov")
	h := handleAnthropicMessages(m, r, s, tok, 5*time.Second)

	w := charInvoke(h, "/v1/messages", `{"model":"vm","messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":128}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "message_stop") {
		t.Fatalf("expected Anthropic message_stop terminator, got %q", w.Body.String())
	}
}

func TestHandleAnthropicMessages_Case4_NonStream_Passthrough(t *testing.T) {
	// Anthropic 客户端 → Anthropic 上游（Case 4），应原样透传。
	srv := newCharUpstream(t, http.StatusOK, "application/json", charAnthropicResp())
	m, r, s, tok := charStack(t, srv.URL, "anthropic", "prov", "prov")
	h := handleAnthropicMessages(m, r, s, tok, 5*time.Second)

	w := charInvoke(h, "/v1/messages", `{"model":"vm","messages":[{"role":"user","content":"hi"}],"max_tokens":128}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "msg_1") {
		t.Fatalf("expected passthrough Anthropic body, got %s", w.Body.String())
	}
}

func TestHandleAnthropicMessages_ModelNotAllowed(t *testing.T) {
	srv := newCharUpstream(t, http.StatusOK, "application/json", charAnthropicResp())
	m, r, s, tok := charStack(t, srv.URL, "anthropic", "prov", "prov")
	h := handleAnthropicMessages(m, r, s, tok, 5*time.Second)

	w := charInvoke(h, "/v1/messages", `{"model":"unknown","messages":[{"role":"user","content":"hi"}],"max_tokens":128}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHandleAnthropicMessages_InvalidJSON(t *testing.T) {
	srv := newCharUpstream(t, http.StatusOK, "application/json", charAnthropicResp())
	m, r, s, tok := charStack(t, srv.URL, "anthropic", "prov", "prov")
	h := handleAnthropicMessages(m, r, s, tok, 5*time.Second)

	w := charInvoke(h, "/v1/messages", `{`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleAnthropicMessages_Case4_UpstreamError(t *testing.T) {
	srv := newCharUpstream(t, http.StatusBadRequest, "application/json", `{"type":"error","error":{"message":"bad"}}`)
	m, r, s, tok := charStack(t, srv.URL, "anthropic", "prov", "prov")
	h := handleAnthropicMessages(m, r, s, tok, 5*time.Second)

	w := charInvoke(h, "/v1/messages", `{"model":"vm","messages":[{"role":"user","content":"hi"}],"max_tokens":128}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 passthrough, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "bad") {
		t.Fatalf("expected upstream error body, got %s", w.Body.String())
	}
}

// ==================== 其他轻量 handler ====================

func TestHandleListModels(t *testing.T) {
	m, _, _, _ := charStack(t, "http://127.0.0.1:1", "openai", "prov", "prov")
	h := handleListModels(m)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	h(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), testVirtualModel) {
		t.Fatalf("expected virtual model in list, got %s", w.Body.String())
	}
}

func TestHandleCountTokens_NonAnthropicUpstream_EstimatesLocally(t *testing.T) {
	srv := newCharUpstream(t, http.StatusOK, "application/json", charOpenAIResp())
	m, r, _, tok := charStack(t, srv.URL, "openai", "prov", "prov")
	pm := provider.NewManager(map[string]config.ProviderConfig{
		"prov": {BaseURL: srv.URL, Protocol: "openai", Timeout: 5 * time.Second},
	})
	h := handleCountTokens(m, r, pm)

	w := charInvoke(h, "/v1/messages/count_tokens", `{"model":"vm","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "input_tokens") {
		t.Fatalf("expected input_tokens in usage, got %s", w.Body.String())
	}
	_ = tok
}

// ==================== 补充分支：429 / 连接失败 / 流式错误 ====================

func TestHandleChatCompletion_NonStream_Unreachable(t *testing.T) {
	// provider 可达性由 BaseURL 决定；指向不可达端口。
	m, r, s, tok := charStack(t, "http://127.0.0.1:1", "openai", "prov", "prov")
	h := handleChatCompletion(m, r, s, tok, 3*time.Second)

	w := charInvoke(h, "/v1/chat/completions", `{"model":"vm","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "all upstream models failed") {
		t.Fatalf("expected generic failure message, got %s", w.Body.String())
	}
}

func TestHandleAnthropicMessages_Unreachable(t *testing.T) {
	m, r, s, tok := charStack(t, "http://127.0.0.1:1", "anthropic", "prov", "prov")
	h := handleAnthropicMessages(m, r, s, tok, 3*time.Second)

	w := charInvoke(h, "/v1/messages", `{"model":"vm","messages":[{"role":"user","content":"hi"}],"max_tokens":128}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandleChatCompletion_NonStream_RateLimited(t *testing.T) {
	srv := newCharUpstream(t, http.StatusTooManyRequests, "application/json", `{"error":{"retry_after":1}}`)
	m, r, s, tok := charStack(t, srv.URL, "openai", "prov", "prov")
	h := handleChatCompletion(m, r, s, tok, 5*time.Second)

	w := charInvoke(h, "/v1/chat/completions", `{"model":"vm","messages":[{"role":"user","content":"hi"}]}`)
	// 429 触发退避后无更多候选，最终返回 503。
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 after 429 backoff, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandleAnthropicMessages_RateLimited(t *testing.T) {
	srv := newCharUpstream(t, http.StatusTooManyRequests, "application/json", `{"error":{"retry_after":1}}`)
	m, r, s, tok := charStack(t, srv.URL, "anthropic", "prov", "prov")
	h := handleAnthropicMessages(m, r, s, tok, 5*time.Second)

	w := charInvoke(h, "/v1/messages", `{"model":"vm","messages":[{"role":"user","content":"hi"}],"max_tokens":128}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 after 429 backoff, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandleChatCompletion_Stream_UpstreamError(t *testing.T) {
	srv := newCharUpstream(t, http.StatusInternalServerError, "application/json", `{"error":"boom"}`)
	m, r, s, tok := charStack(t, srv.URL, "openai", "prov", "prov")
	h := handleChatCompletion(m, r, s, tok, 5*time.Second)

	w := charInvoke(h, "/v1/chat/completions", `{"model":"vm","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 passthrough for stream error, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandleCompletion_NotImplemented(t *testing.T) {
	m, r, s, tok := charStack(t, "http://127.0.0.1:1", "openai", "prov", "prov")
	h := handleCompletion(m, r, s, tok)
	w := charInvoke(h, "/v1/completions", `{}`)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", w.Code)
	}
}

func TestDetectClientProtocol(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		headers map[string]string
		want    provider.ClientProtocol
	}{
		{"v1-messages", "/v1/messages", nil, provider.ProtocolAnthropic},
		{"messages", "/messages", nil, provider.ProtocolAnthropic},
		{"v1-chat", "/v1/chat/completions", nil, provider.ProtocolOpenAI},
		{"chat", "/chat/completions", nil, provider.ProtocolOpenAI},
		{"v1-completions", "/v1/completions", nil, provider.ProtocolOpenAI},
		{"completions", "/completions", nil, provider.ProtocolOpenAI},
		{"header-anthropic-version", "/other", map[string]string{"anthropic-version": "2023-06-01"}, provider.ProtocolAnthropic},
		{"header-x-api-key", "/other", map[string]string{"x-api-key": "k"}, provider.ProtocolAnthropic},
		{"default-openai", "/other", nil, provider.ProtocolOpenAI},
	}
	gin.SetMode(gin.TestMode)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, tc.path, nil)
			for k, v := range tc.headers {
				c.Request.Header.Set(k, v)
			}
			if got := detectClientProtocol(c); got != tc.want {
				t.Fatalf("expected %v, got %v", tc.want, got)
			}
		})
	}
}
