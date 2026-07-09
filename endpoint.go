package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"
)

const defaultAllowHosts = "chatgpt.com,api.anthropic.com,generativelanguage.googleapis.com,aiplatform.googleapis.com"

type endpointInfo struct {
	Kind             string
	Provider         string
	Endpoint         string
	Model            string
	Stream           bool
	AuditRequestBody bool
	AuditResponse    bool
}

func classifyEndpoint(r *http.Request, body []byte) endpointInfo {
	if r == nil {
		return endpointInfo{Kind: "passthrough"}
	}
	host := normalizedHost(r.Host)
	path := r.URL.Path
	method := r.Method

	if method == http.MethodPost && host == "chatgpt.com" && path == "/backend-api/codex/responses" {
		return endpointInfo{
			Kind:             "codex.responses",
			Provider:         "codex",
			Endpoint:         "responses",
			Model:            jsonString(body, "model"),
			Stream:           jsonBool(body, "stream"),
			AuditRequestBody: true,
			AuditResponse:    true,
		}
	}

	if isAnthropicEndpoint(host, path) {
		switch {
		case method == http.MethodPost && path == "/v1/messages":
			return endpointInfo{
				Kind:             "anthropic.messages",
				Provider:         "anthropic",
				Endpoint:         "messages",
				Model:            jsonString(body, "model"),
				Stream:           jsonBool(body, "stream"),
				AuditRequestBody: true,
				AuditResponse:    true,
			}
		case method == http.MethodPost && path == "/v1/messages/count_tokens":
			return endpointInfo{
				Kind:             "anthropic.count_tokens",
				Provider:         "anthropic",
				Endpoint:         "count_tokens",
				Model:            jsonString(body, "model"),
				AuditRequestBody: true,
				AuditResponse:    true,
			}
		case method == http.MethodGet && path == "/v1/models":
			return endpointInfo{
				Kind:          "anthropic.models",
				Provider:      "anthropic",
				Endpoint:      "models",
				AuditResponse: true,
			}
		}
	}

	if isGeminiEndpoint(host, path) {
		if method == http.MethodGet && (path == "/v1beta/models" || path == "/v1/models") {
			return endpointInfo{
				Kind:          "gemini.models",
				Provider:      "gemini",
				Endpoint:      "models",
				AuditResponse: true,
			}
		}
		if method == http.MethodPost {
			if model, action, ok := geminiModelAction(path); ok {
				switch action {
				case "generateContent", "streamGenerateContent", "countTokens":
					return endpointInfo{
						Kind:             "gemini." + action,
						Provider:         "gemini",
						Endpoint:         action,
						Model:            model,
						Stream:           action == "streamGenerateContent" || r.URL.Query().Get("alt") == "sse",
						AuditRequestBody: true,
						AuditResponse:    true,
					}
				}
			}
		}
	}

	return endpointInfo{Kind: "passthrough"}
}

func (e endpointInfo) fields() map[string]any {
	fields := map[string]any{"kind": e.Kind}
	if e.Provider != "" {
		fields["provider"] = e.Provider
	}
	if e.Endpoint != "" {
		fields["endpoint"] = e.Endpoint
	}
	if e.Model != "" {
		fields["model"] = e.Model
	}
	if e.Stream {
		fields["stream"] = true
	}
	return fields
}

func (e endpointInfo) responseLogExt(contentType string) string {
	if e.Stream || strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		return ".sse"
	}
	return ".response"
}

func normalizedHost(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.ToLower(strings.TrimSpace(host))
}

func isAnthropicEndpoint(host, path string) bool {
	return host == "api.anthropic.com" || strings.HasSuffix(host, ".anthropic.com") ||
		path == "/v1/messages" || path == "/v1/messages/count_tokens"
}

func isGeminiEndpoint(host, path string) bool {
	return host == "generativelanguage.googleapis.com" ||
		host == "aiplatform.googleapis.com" ||
		strings.HasSuffix(host, ".aiplatform.googleapis.com") ||
		strings.HasPrefix(path, "/v1beta/models/") || strings.HasPrefix(path, "/v1/models/") ||
		strings.Contains(path, "/publishers/google/models/") ||
		path == "/v1beta/models" || path == "/v1/models"
}

func geminiModelAction(path string) (model, action string, ok bool) {
	for _, prefix := range []string{"/v1beta/models/", "/v1/models/"} {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(path, prefix)
		i := strings.LastIndex(rest, ":")
		if i <= 0 || i == len(rest)-1 {
			return "", "", false
		}
		rawModel := strings.TrimPrefix(rest[:i], "models/")
		if decoded, err := url.PathUnescape(rawModel); err == nil {
			rawModel = decoded
		}
		return rawModel, rest[i+1:], true
	}
	if marker := "/publishers/google/models/"; strings.Contains(path, marker) {
		rest := path[strings.LastIndex(path, marker)+len(marker):]
		i := strings.LastIndex(rest, ":")
		if i <= 0 || i == len(rest)-1 {
			return "", "", false
		}
		rawModel := rest[:i]
		if decoded, err := url.PathUnescape(rawModel); err == nil {
			rawModel = decoded
		}
		return rawModel, rest[i+1:], true
	}
	return "", "", false
}

func jsonString(body []byte, key string) string {
	var m map[string]any
	if len(body) == 0 || json.Unmarshal(body, &m) != nil {
		return ""
	}
	v, _ := m[key].(string)
	return strings.TrimSpace(v)
}

func jsonBool(body []byte, key string) bool {
	var m map[string]any
	if len(body) == 0 || json.Unmarshal(body, &m) != nil {
		return false
	}
	v, _ := m[key].(bool)
	return v
}
