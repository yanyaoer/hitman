package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClassifyAnthropicMessages(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6","stream":true}`))
	info := classifyEndpoint(req, []byte(`{"model":"claude-sonnet-4-6","stream":true}`))
	if info.Kind != "anthropic.messages" || info.Provider != "anthropic" || info.Model != "claude-sonnet-4-6" || !info.Stream {
		t.Fatalf("unexpected endpoint info: %+v", info)
	}
	if !info.AuditRequestBody || !info.AuditResponse {
		t.Fatalf("anthropic messages should audit request and response: %+v", info)
	}
}

func TestClassifyGeminiStreamGenerateContent(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse", strings.NewReader(`{"contents":[]}`))
	info := classifyEndpoint(req, []byte(`{"contents":[]}`))
	if info.Kind != "gemini.streamGenerateContent" || info.Provider != "gemini" || info.Model != "gemini-2.5-pro" || !info.Stream {
		t.Fatalf("unexpected endpoint info: %+v", info)
	}
}

func TestClassifyVertexGeminiGenerateContent(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://us-central1-aiplatform.googleapis.com/v1/projects/p/locations/us-central1/publishers/google/models/gemini-2.5-pro:generateContent", strings.NewReader(`{"contents":[]}`))
	info := classifyEndpoint(req, []byte(`{"contents":[]}`))
	if info.Kind != "gemini.generateContent" || info.Provider != "gemini" || info.Model != "gemini-2.5-pro" || info.Stream {
		t.Fatalf("unexpected endpoint info: %+v", info)
	}
}

func TestExtractAnthropicStreamUsage(t *testing.T) {
	fields := extractEndpointResponseFields(endpointInfo{Provider: "anthropic"}, []byte(
		"event: message_start\n"+
			`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-test","usage":{"input_tokens":3,"output_tokens":0,"cache_read_input_tokens":1}}}`+"\n\n"+
			"event: message_delta\n"+
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2,"thinking_tokens":5}}`+"\n\n",
	))
	assertUsageInt(t, fields, "input_tokens", 3)
	assertUsageInt(t, fields, "output_tokens", 2)
	assertUsageInt(t, fields, "total_tokens", 5)
	assertUsageInt(t, fields, "thinking_tokens", 5)
	if fields["response_model"] != "claude-test" {
		t.Fatalf("response_model = %v, want claude-test", fields["response_model"])
	}
	if fields["stopped_reason"] != "end_turn" {
		t.Fatalf("stopped_reason = %v, want end_turn", fields["stopped_reason"])
	}
}

func TestExtractGeminiUsage(t *testing.T) {
	fields := extractEndpointResponseFields(endpointInfo{Provider: "gemini"}, []byte(
		`data: {"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15,"cachedContentTokenCount":2,"thoughtsTokenCount":3},"modelVersion":"gemini-2.5-pro","responseId":"resp_1"}`+"\n\n",
	))
	assertUsageInt(t, fields, "input_tokens", 10)
	assertUsageInt(t, fields, "output_tokens", 5)
	assertUsageInt(t, fields, "total_tokens", 15)
	assertUsageInt(t, fields, "cached_tokens", 2)
	assertUsageInt(t, fields, "reasoning_tokens", 3)
	if fields["response_model"] != "gemini-2.5-pro" {
		t.Fatalf("response_model = %v, want gemini-2.5-pro", fields["response_model"])
	}
	if fields["response_id"] != "resp_1" {
		t.Fatalf("response_id = %v, want resp_1", fields["response_id"])
	}
}

func assertUsageInt(t *testing.T, fields map[string]any, key string, want int64) {
	t.Helper()
	usage, ok := fields["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage missing or wrong type: %#v", fields["usage"])
	}
	got, ok := usage[key].(int64)
	if !ok || got != want {
		t.Fatalf("usage[%s] = %#v, want %d", key, usage[key], want)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func newTestResponse(status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
