package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAnthropicPassthroughAuditsResponseUsage(t *testing.T) {
	auditDir := t.TempDir()
	srv := &server{
		cfg:   appConfig{AllowHosts: []string{"api.anthropic.com"}},
		audit: newAuditor(auditDir, true),
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.String() != "https://api.anthropic.com/v1/messages" {
				t.Fatalf("upstream URL = %q, want anthropic messages endpoint", req.URL.String())
			}
			return newTestResponse(http.StatusOK, "text/event-stream", "event: message_start\n"+
				`data: {"type":"message_start","message":{"model":"claude-test","usage":{"input_tokens":7,"output_tokens":0}}}`+"\n\n"+
				"event: message_delta\n"+
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`+"\n\n"), nil
		})},
	}

	req := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(`{"model":"claude-test","stream":true}`))
	req.Header.Set("User-Agent", "claude-cli test")
	req.Header.Set("Anthropic-Api-Key", "secret")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "message_delta") {
		t.Fatalf("response body missing streamed payload: %s", rr.Body.String())
	}

	meta := readOnlyAuditMeta(t, auditDir)
	if meta["kind"] != "anthropic.messages" || meta["provider"] != "anthropic" || meta["model"] != "claude-test" {
		t.Fatalf("unexpected audit metadata: %#v", meta)
	}
	if meta["stream"] != true {
		t.Fatalf("stream metadata = %#v, want true", meta["stream"])
	}
	headers, ok := meta["headers"].(map[string]any)
	if !ok || headers["Anthropic-Api-Key"] != "***" {
		t.Fatalf("anthropic api key was not redacted: %#v", meta["headers"])
	}
	usage, ok := meta["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage missing from audit metadata: %#v", meta)
	}
	if usage["input_tokens"] != float64(7) || usage["output_tokens"] != float64(4) || usage["total_tokens"] != float64(11) {
		t.Fatalf("unexpected usage: %#v", usage)
	}
	responseLog, _ := meta["response_log"].(string)
	if responseLog == "" || filepath.Ext(responseLog) != ".sse" {
		t.Fatalf("response_log = %q, want .sse file", responseLog)
	}
	if b, err := os.ReadFile(filepath.Join(auditDayDir(t, auditDir), responseLog)); err != nil || !strings.Contains(string(b), "message_delta") {
		t.Fatalf("response log not written correctly: bytes=%q err=%v", string(b), err)
	}
}

func readOnlyAuditMeta(t *testing.T, auditDir string) map[string]any {
	t.Helper()
	dayDir := auditDayDir(t, auditDir)
	entries, err := os.ReadDir(dayDir)
	if err != nil {
		t.Fatalf("read audit day dir: %v", err)
	}
	var jsonFiles []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "req-") || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		jsonFiles = append(jsonFiles, filepath.Join(dayDir, entry.Name()))
	}
	if len(jsonFiles) != 1 {
		t.Fatalf("expected one audit json file, got %d: %#v", len(jsonFiles), jsonFiles)
	}
	b, err := os.ReadFile(jsonFiles[0])
	if err != nil {
		t.Fatalf("read audit json: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(b, &meta); err != nil {
		t.Fatalf("decode audit json: %v\n%s", err, string(b))
	}
	return meta
}

func auditDayDir(t *testing.T, auditDir string) string {
	t.Helper()
	entries, err := os.ReadDir(auditDir)
	if err != nil {
		t.Fatalf("read audit dir: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(auditDir, entry.Name())
		}
	}
	t.Fatalf("no audit day directory under %s", auditDir)
	return ""
}
