package main

import (
	"fmt"
	"net/netip"
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

// appConfig is the fully-resolved runtime configuration. Values come from
// ~/.config/hitman/config.json, with HITMAN_* environment variables kept as
// transient overrides for development and one-off launches.
type appConfig struct {
	ListenAddr    string // TLS listener agent traffic is redirected to
	UpstreamMode  string // proxy or system
	UpstreamProxy string // sing-box socks/mixed inbound used for real egress
	UpstreamDNS   string // resolver used by system upstream mode
	FakeIPCIDR    netip.Prefix
	SocksAddr     string   // deprecated alias for old tests/scripts
	CADir         string   // holds hitman-ca.pem / .key
	AuditDir      string   // per-day audit output
	AllowHosts    []string // upstream Host allowlist (exact or ".suffix"); empty = allow all

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

func loadConfig() (appConfig, error) {
	fileCfg, err := loadFileConfig()
	if err != nil {
		return appConfig{}, fmt.Errorf("load config %s: %w", defaultConfigPath(), err)
	}
	upstreamProxy := configUpstreamProxy(fileCfg)
	fakeCIDR, err := netip.ParsePrefix(configString("HITMAN_FAKEIP_CIDR", fileCfg.FakeIPCIDR, defaultFakeIPCIDR))
	if err != nil {
		return appConfig{}, fmt.Errorf("parse HITMAN_FAKEIP_CIDR: %w", err)
	}
	if !fakeCIDR.Addr().Is4() {
		return appConfig{}, fmt.Errorf("HITMAN_FAKEIP_CIDR must be IPv4")
	}
	c := appConfig{
		ListenAddr:     configString("HITMAN_LISTEN", fileCfg.ListenAddr, "127.0.0.1:8471"),
		UpstreamMode:   configUpstreamMode(fileCfg, upstreamProxy),
		UpstreamProxy:  upstreamProxy,
		UpstreamDNS:    configString("HITMAN_DNS_UPSTREAM", fileCfg.UpstreamDNS, defaultUpstreamDNS),
		FakeIPCIDR:     fakeCIDR.Masked(),
		SocksAddr:      upstreamProxy,
		CADir:          configString("HITMAN_CA_DIR", fileCfg.CADir, "ca"),
		AuditDir:       configString("HITMAN_AUDIT_DIR", fileCfg.AuditDir, "audit"),
		AllowHosts:     configList("HITMAN_ALLOW_HOSTS", fileCfg.AllowHosts, defaultAllowHosts),
		MarkerText:     configString("HITMAN_MARKER", fileCfg.MarkerText, defaultMarkerText),
		TruncationStep: configInt("HITMAN_TRUNCATION_STEP", fileCfg.TruncationStep, defaultTruncationStep),
		MaxTierN:       configInt("HITMAN_MAX_TIER_N", fileCfg.MaxTierN, defaultMaxTierN),
		MaxContinue:    configInt("HITMAN_MAX_CONTINUE", fileCfg.MaxContinue, defaultMaxContinue),
		DebugLog:       configBool("HITMAN_DEBUG", fileCfg.DebugLog, false),
		AuditBodies:    configBool("HITMAN_AUDIT_BODIES", fileCfg.AuditBodies, true),
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
	return c, nil
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

func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
