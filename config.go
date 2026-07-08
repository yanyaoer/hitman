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
	defaultMaxContinue    = 3
)

// appConfig is the fully-resolved runtime configuration. Everything is
// overridable via environment variables so the launchd plist / bridge script
// can tune behaviour without touching code.
type appConfig struct {
	ListenAddr string   // TLS listener codex is redirected to
	SocksAddr  string   // sing-box socks-in used for real egress
	CADir      string   // holds ai-bridge-ca.pem / .key
	AuditDir   string   // per-day audit output
	AllowHosts []string // upstream Host allowlist (exact or ".suffix"); empty = allow all

	MarkerText     string
	TruncationStep int
	MaxTierN       int
	MaxContinue    int
	DebugLog       bool
	AuditBodies    bool
}

// foldConfig is the subset consumed by the fold state machine (ported 1:1 from
// cpa-plugin-codexcomp/config.go semantics).
type foldConfig struct {
	MarkerText     string
	TruncationStep int
	MaxTierN       int
	MaxContinue    int
	DebugLog       bool
}

func loadConfig() appConfig {
	c := appConfig{
		ListenAddr:     envOr("AI_BRIDGE_LISTEN", "127.0.0.1:8471"),
		SocksAddr:      envOr("AI_BRIDGE_SOCKS", "127.0.0.1:2333"),
		CADir:          envOr("AI_BRIDGE_CA_DIR", "ca"),
		AuditDir:       envOr("AI_BRIDGE_AUDIT_DIR", "audit"),
		AllowHosts:     splitCSV(envOr("AI_BRIDGE_ALLOW_HOSTS", "chatgpt.com")),
		MarkerText:     envOr("AI_BRIDGE_MARKER", defaultMarkerText),
		TruncationStep: envInt("AI_BRIDGE_TRUNCATION_STEP", defaultTruncationStep),
		MaxTierN:       envInt("AI_BRIDGE_MAX_TIER_N", defaultMaxTierN),
		MaxContinue:    envInt("AI_BRIDGE_MAX_CONTINUE", defaultMaxContinue),
		DebugLog:       envBool("AI_BRIDGE_DEBUG", false),
		AuditBodies:    envBool("AI_BRIDGE_AUDIT_BODIES", true),
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

func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envBool(key string, def bool) bool {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}
