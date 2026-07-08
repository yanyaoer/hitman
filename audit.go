package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// auditor writes one directory per day. Each request yields a req-<id>.json
// (redacted metadata + request body) and, when streamed, a req-<id>.sse with the
// full downstream response; index.jsonl holds a one-line summary per request.
type auditor struct {
	dir        string
	recordBody bool
	mu         sync.Mutex
}

func newAuditor(dir string, recordBody bool) *auditor {
	return &auditor{dir: dir, recordBody: recordBody}
}

type auditRecord struct {
	a       *auditor
	id      string
	dayDir  string
	start   time.Time
	meta    map[string]any
	sseFile *os.File
	sseErr  bool
}

func (a *auditor) begin(r *http.Request, kind string, body []byte) *auditRecord {
	id := newID()
	day := time.Now().Format("2006-01-02")
	rec := &auditRecord{
		a:      a,
		id:     id,
		dayDir: filepath.Join(a.dir, day),
		start:  time.Now(),
		meta: map[string]any{
			"ts":      time.Now().Format(time.RFC3339),
			"id":      id,
			"kind":    kind,
			"cli":     detectCLI(r.Header.Get("User-Agent")),
			"ua":      r.Header.Get("User-Agent"),
			"method":  r.Method,
			"host":    r.Host,
			"path":    r.URL.Path,
			"headers": redactHeaders(r.Header),
		},
	}
	if a.recordBody && len(body) > 0 && json.Valid(body) {
		rec.meta["request_body"] = json.RawMessage(body)
	}
	return rec
}

func (rec *auditRecord) sse(p []byte) {
	if rec == nil || rec.sseErr {
		return
	}
	if rec.sseFile == nil {
		if err := os.MkdirAll(rec.dayDir, 0o755); err != nil {
			rec.sseErr = true
			return
		}
		f, err := os.Create(filepath.Join(rec.dayDir, "req-"+rec.id+".sse"))
		if err != nil {
			rec.sseErr = true
			logWarn("audit sse create", err)
			return
		}
		rec.sseFile = f
	}
	_, _ = rec.sseFile.Write(p)
}

func (rec *auditRecord) fields(kv map[string]any) {
	if rec == nil {
		return
	}
	for k, v := range kv {
		rec.meta[k] = v
	}
}

func (rec *auditRecord) done() {
	if rec == nil {
		return
	}
	if rec.sseFile != nil {
		_ = rec.sseFile.Close()
	}
	rec.meta["latency_ms"] = time.Since(rec.start).Milliseconds()
	if err := os.MkdirAll(rec.dayDir, 0o755); err != nil {
		logWarn("audit mkdir", err)
		return
	}
	if b, err := json.MarshalIndent(rec.meta, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(rec.dayDir, "req-"+rec.id+".json"), b, 0o644)
	}
	summary := map[string]any{}
	for _, k := range []string{"ts", "id", "kind", "cli", "method", "path", "model", "stream", "folded", "status", "rounds", "stopped_reason", "latency_ms"} {
		if v, ok := rec.meta[k]; ok {
			summary[k] = v
		}
	}
	if v, ok := rec.meta["usage"]; ok {
		summary["usage"] = v
	}
	rec.a.appendIndex(rec.dayDir, summary)
}

func (a *auditor) appendIndex(dayDir string, summary map[string]any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	line, err := json.Marshal(summary)
	if err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dayDir, "index.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		logWarn("audit index", err)
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

func detectCLI(ua string) string {
	l := strings.ToLower(ua)
	switch {
	case strings.Contains(l, "codex"):
		return "codex"
	case strings.Contains(l, "claude"):
		return "claude"
	default:
		return "other"
	}
}

var redactedHeaderKeys = map[string]bool{
	"authorization":       true,
	"cookie":              true,
	"set-cookie":          true,
	"proxy-authorization": true,
	"x-api-key":           true,
}

func redactHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for k, v := range h {
		val := strings.Join(v, ", ")
		if redactedHeaderKeys[strings.ToLower(k)] {
			val = "***"
		}
		out[k] = val
	}
	return out
}

func newID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return time.Now().Format("150405") + "-" + hex.EncodeToString(b[:])
}

func logWarn(ctx string, err error) {
	log.Printf("[audit] warn: %s: %v", ctx, err)
}
