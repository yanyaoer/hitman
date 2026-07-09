package main

import (
	"os"
	"strconv"
	"strings"
)

const (
	defaultMarkerText     = "Continue thinking..."
	defaultTruncationStep = 518
	defaultMaxTierN       = 6
	// 0 = folding disabled by default: hitman still intercepts, audits, and detects
	// truncation (records proxy_rounds/stopped_reason), but does not continue. Set
	// HITMAN_MAX_CONTINUE >= 1 to enable multi-round fold.
	defaultMaxContinue = 0
)

// appConfig is the fully-resolved runtime configuration. Everything is
// overridable via HITMAN_* environment variables so the launchd plist / hitman
// script can tune behaviour without touching code.
type appConfig struct {
	ListenAddr string   // TLS listener codex is redirected to
	SocksAddr  string   // sing-box socks-in used for real egress
	CADir      string   // holds hitman-ca.pem / .key
	AuditDir   string   // per-day audit output
	AllowHosts []string // upstream Host allowlist (exact or ".suffix"); empty = allow all

	MarkerText     string
	TruncationStep int
	MaxTierN       int
	MaxContinue    int
	DebugLog       bool
	AuditBodies    bool
}

// foldConfig is the subset consumed by the fold state machine.
type foldConfig struct {
	MarkerText     string
	TruncationStep int
	MaxTierN       int
	MaxContinue    int
	DebugLog       bool
}

func loadConfig() appConfig {
	c := appConfig{
		ListenAddr:     envOr("HITMAN_LISTEN", "127.0.0.1:8471"),
		SocksAddr:      normalizeSocks(envOr("HITMAN_SOCKS", "127.0.0.1:2333")),
		CADir:          envOr("HITMAN_CA_DIR", "ca"),
		AuditDir:       envOr("HITMAN_AUDIT_DIR", "audit"),
		AllowHosts:     splitCSV(envOr("HITMAN_ALLOW_HOSTS", "chatgpt.com")),
		MarkerText:     envOr("HITMAN_MARKER", defaultMarkerText),
		TruncationStep: envInt("HITMAN_TRUNCATION_STEP", defaultTruncationStep),
		MaxTierN:       envInt("HITMAN_MAX_TIER_N", defaultMaxTierN),
		MaxContinue:    envInt("HITMAN_MAX_CONTINUE", defaultMaxContinue),
		DebugLog:       envBool("HITMAN_DEBUG", false),
		AuditBodies:    envBool("HITMAN_AUDIT_BODIES", true),
	}
	if strings.TrimSpace(c.MarkerText) == "" {
		c.MarkerText = defaultMarkerText
	}
	if c.TruncationStep <= 0 {
		c.TruncationStep = defaultTruncationStep
	}
	if c.MaxTierN < 0 {
		c.MaxTierN = defaultMaxTierN
	}
	if c.MaxContinue < 0 {
		c.MaxContinue = defaultMaxContinue
	}
	return c
}

func (c appConfig) fold() foldConfig {
	return foldConfig{
		MarkerText:     c.MarkerText,
		TruncationStep: c.TruncationStep,
		MaxTierN:       c.MaxTierN,
		MaxContinue:    c.MaxContinue,
		DebugLog:       c.DebugLog,
	}
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

// normalizeSocks maps the "direct" sentinels to an empty SocksAddr, which selects
// direct egress (dial the upstream straight, letting the sing-box TUN capture it).
// Useful when strict_route makes the socks inbound unreachable from local processes.
func normalizeSocks(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "direct", "none", "off", "-":
		return ""
	default:
		return v
	}
}

func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
