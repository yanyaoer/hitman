package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

type server struct {
	cfg    appConfig
	client *http.Client
	audit  *auditor
}

func newServer(cfg appConfig, audit *auditor) *server {
	transport := &http.Transport{
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   20 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
		ExpectContinueTimeout: 2 * time.Second,
	}
	dialContext, _, err := upstreamDialContext(cfg.UpstreamMode, cfg.UpstreamProxy, cfg.UpstreamDNS, fakeIPPrefixesFromEnv())
	if err != nil {
		transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
			return nil, err
		}
	} else {
		transport.DialContext = dialContext
	}
	return &server{
		cfg:   cfg,
		audit: audit,
		client: &http.Client{
			Transport: transport,
			// Forward 3xx verbatim to codex; a transparent proxy must not collapse
			// upstream auth/redirect flows by following them itself.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Defense in depth: only forward (and attach the client's own credentials) to
	// allowlisted upstream hosts. sing-box already scopes what reaches us, but this
	// prevents a misrouted/redirected request from leaking the Bearer to any Host.
	if !hostAllowed(s.cfg.AllowHosts, r.Host) {
		http.Error(w, "hitman: host not allowed: "+r.Host, http.StatusForbidden)
		return
	}
	// Reject WebSocket upgrades fast so codex falls back to the HTTP/SSE responses
	// transport, which is the path we fold. We deliberately do not proxy the WS
	// transport (its frames would need parsing to fold).
	if strings.Contains(strings.ToLower(r.Header.Get("Upgrade")), "websocket") {
		http.Error(w, "hitman: websocket transport not supported; use HTTPS/SSE", http.StatusUpgradeRequired)
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/backend-api/codex/responses" {
		s.handleResponses(w, r)
		return
	}
	s.handlePassthrough(w, r)
}

func hostAllowed(allow []string, host string) bool {
	if len(allow) == 0 {
		return true
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(host)
	for _, a := range allow {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		if host == a || strings.HasSuffix(host, "."+a) {
			return true
		}
	}
	return false
}

func upstreamURL(r *http.Request) string {
	return "https://" + r.Host + r.URL.RequestURI()
}

func (s *server) handlePassthrough(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		rec := s.audit.begin(r, "passthrough", nil)
		defer rec.done()
		http.Error(w, "hitman: read request body: "+err.Error(), http.StatusBadRequest)
		rec.fields(map[string]any{"status": 400, "error": err.Error()})
		return
	}
	info := classifyEndpoint(r, body)
	var auditBody []byte
	if info.AuditRequestBody {
		auditBody = body
	}
	rec := s.audit.begin(r, info.Kind, auditBody)
	defer rec.done()
	rec.fields(info.fields())
	s.forward(w, r, body, rec, forwardOptions{Endpoint: info, AuditResponse: info.AuditResponse})
}

func (s *server) handleResponses(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	rec := s.audit.begin(r, "responses", body)
	defer rec.done()
	if err != nil {
		http.Error(w, "hitman: read request body: "+err.Error(), http.StatusBadRequest)
		rec.fields(map[string]any{"status": 400, "error": err.Error()})
		return
	}

	var parsed map[string]any
	info := classifyEndpoint(r, body)
	rec.fields(info.fields())
	if err := json.Unmarshal(body, &parsed); err != nil || !foldApplies(parsed) {
		rec.fields(map[string]any{"folded": false})
		s.forward(w, r, body, rec, forwardOptions{Endpoint: info, AuditResponse: true})
		return
	}

	rec.fields(map[string]any{"folded": true, "model": parsed["model"], "stream": true})

	replay := filterReplayHeaders(r.Header)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	emit := func(p []byte) error {
		_, err := w.Write(p)
		if flusher != nil {
			flusher.Flush()
		}
		rec.sse(p)
		return err
	}

	upURL := upstreamURL(r)
	opener := func(b []byte) (int, io.ReadCloser, error) {
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upURL, bytes.NewReader(b))
		if err != nil {
			return 0, nil, err
		}
		req.Header = replay.Clone()
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		resp, err := s.client.Do(req)
		if err != nil {
			return 0, nil, err
		}
		return resp.StatusCode, resp.Body, nil
	}

	fs := newFoldState(parsed, inputArray(parsed), emit, opener, s.cfg.fold())
	fs.run()

	rec.fields(map[string]any{
		"rounds":         fs.roundNo,
		"usage":          fs.summedUsage,
		"stopped_reason": nilIfEmpty(fs.stoppedReason()),
		"status":         200,
	})
}

type forwardOptions struct {
	Endpoint      endpointInfo
	AuditResponse bool
}

// forward is a transparent reverse proxy: decrypt -> re-encrypt to the real
// upstream via socks -> stream the response back. Used for every non-folded
// request (token refresh, rate-limit checks, non-gpt-5.5 responses, etc). When
// enabled, the response stream is teed into the audit response file.
func (s *server) forward(w http.ResponseWriter, r *http.Request, body []byte, rec *auditRecord, opts forwardOptions) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL(r), bytes.NewReader(body))
	if err != nil {
		http.Error(w, "hitman: build request: "+err.Error(), http.StatusBadGateway)
		rec.fields(map[string]any{"status": 502, "error": err.Error()})
		return
	}
	applyReplayHeaders(req.Header, r.Header)
	resp, err := s.client.Do(req)
	if err != nil {
		http.Error(w, "hitman: upstream error: "+err.Error(), http.StatusBadGateway)
		rec.fields(map[string]any{"status": 502, "error": err.Error()})
		return
	}
	defer resp.Body.Close()
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	respLogExt := opts.Endpoint.responseLogExt(resp.Header.Get("Content-Type"))
	var captured bytes.Buffer
	captureTruncated := false
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if _, werr := w.Write(chunk); werr != nil {
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
			if opts.AuditResponse {
				rec.response(chunk, respLogExt)
				if captured.Len()+len(chunk) <= maxResponseAnalyzeBytes {
					_, _ = captured.Write(chunk)
				} else {
					captureTruncated = true
				}
			}
		}
		if rerr != nil {
			break
		}
	}
	fields := map[string]any{"status": resp.StatusCode}
	if opts.AuditResponse {
		fields["response_bytes_analyzed"] = captured.Len()
		if captureTruncated {
			fields["response_analysis_truncated"] = true
		} else if extracted := extractEndpointResponseFields(opts.Endpoint, captured.Bytes()); len(extracted) > 0 {
			for k, v := range extracted {
				fields[k] = v
			}
		}
	}
	rec.fields(fields)
}

// foldApplies mirrors cpa-plugin-codexcomp's routeModel gate: only gpt-5.5,
// streamed, array input, no server-side previous_response_id.
func foldApplies(body map[string]any) bool {
	if body == nil {
		return false
	}
	if m, _ := body["model"].(string); m != "gpt-5.5" {
		return false
	}
	if s, ok := body["stream"].(bool); !ok || !s {
		return false
	}
	if _, has := body["previous_response_id"]; has {
		return false
	}
	if _, ok := body["input"].([]any); !ok {
		return false
	}
	return true
}

func inputArray(body map[string]any) []any {
	arr, _ := body["input"].([]any)
	return append([]any(nil), arr...)
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

var hopByHop = map[string]bool{
	"connection": true, "proxy-connection": true, "keep-alive": true,
	"transfer-encoding": true, "upgrade": true, "te": true, "trailer": true,
	"proxy-authenticate": true, "proxy-authorization": true,
}

// filterReplayHeaders keeps the client's headers (crucially its own Authorization
// Bearer) but drops hop-by-hop, Host, Content-Length and Accept-Encoding so the
// Go transport recomputes them and transparently handles compression.
func filterReplayHeaders(src http.Header) http.Header {
	out := http.Header{}
	for k, vv := range src {
		lk := strings.ToLower(k)
		if hopByHop[lk] || lk == "host" || lk == "content-length" || lk == "accept-encoding" {
			continue
		}
		out[k] = append([]string(nil), vv...)
	}
	return out
}

func applyReplayHeaders(dst, src http.Header) {
	for k, vv := range filterReplayHeaders(src) {
		dst[k] = vv
	}
}

func copyResponseHeaders(dst, src http.Header) {
	for k, vv := range src {
		lk := strings.ToLower(k)
		if hopByHop[lk] || lk == "content-length" {
			continue
		}
		dst[k] = append([]string(nil), vv...)
	}
}
