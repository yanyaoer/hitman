package main

import (
	"os"
	"path/filepath"
	"testing"
)

func useTempConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("HITMAN_CONFIG", path)
	return path
}

func TestLoadConfigReadsHitmanEnv(t *testing.T) {
	useTempConfig(t)
	t.Setenv("HITMAN_LISTEN", "127.0.0.1:2222")
	t.Setenv("HITMAN_SOCKS", "direct")
	t.Setenv("HITMAN_MAX_CONTINUE", "2")
	t.Setenv("HITMAN_AUDIT_BODIES", "false")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != "127.0.0.1:2222" {
		t.Fatalf("ListenAddr = %q, want HITMAN_LISTEN", cfg.ListenAddr)
	}
	if cfg.SocksAddr != "" {
		t.Fatalf("SocksAddr = %q, want direct egress (empty)", cfg.SocksAddr)
	}
	if cfg.MaxContinue != 2 {
		t.Fatalf("MaxContinue = %d, want 2", cfg.MaxContinue)
	}
	if cfg.AuditBodies {
		t.Fatalf("AuditBodies = true, want false")
	}
}

func TestDefaultMaxContinueDisablesFold(t *testing.T) {
	useTempConfig(t)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxContinue != 0 {
		t.Fatalf("default MaxContinue = %d, want 0 (folding off by default)", cfg.MaxContinue)
	}
}

func TestDefaultUpstreamModeIsSystem(t *testing.T) {
	useTempConfig(t)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UpstreamMode != "system" {
		t.Fatalf("default UpstreamMode = %q, want system", cfg.UpstreamMode)
	}
	if cfg.UpstreamProxy != "" {
		t.Fatalf("default UpstreamProxy = %q, want empty", cfg.UpstreamProxy)
	}
}

func TestLoadConfigReadsJSONProxy(t *testing.T) {
	path := useTempConfig(t)
	if err := os.WriteFile(path, []byte(`{"upstreamProxy":"socks5://127.0.0.1:1080","upstreamDNS":"9.9.9.9:53","maxContinue":3}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UpstreamMode != "proxy" {
		t.Fatalf("UpstreamMode = %q, want proxy inferred from upstreamProxy", cfg.UpstreamMode)
	}
	if cfg.UpstreamProxy != "socks5://127.0.0.1:1080" {
		t.Fatalf("UpstreamProxy = %q", cfg.UpstreamProxy)
	}
	if cfg.UpstreamDNS != "9.9.9.9:53" {
		t.Fatalf("UpstreamDNS = %q", cfg.UpstreamDNS)
	}
	if cfg.MaxContinue != 3 {
		t.Fatalf("MaxContinue = %d", cfg.MaxContinue)
	}
}

func TestDefaultAllowHostsIncludeAnthropicAndGemini(t *testing.T) {
	useTempConfig(t)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"chatgpt.com", "api.anthropic.com", "generativelanguage.googleapis.com", "aiplatform.googleapis.com"} {
		if !hostAllowed(cfg.AllowHosts, host) {
			t.Fatalf("default AllowHosts does not allow %s: %#v", host, cfg.AllowHosts)
		}
	}
}

func TestLoadOrCreateCAUsesHitmanNames(t *testing.T) {
	dir := t.TempDir()
	caObj, err := loadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("loadOrCreateCA: %v", err)
	}
	if caObj.cert.Subject.CommonName != "hitman CA" {
		t.Fatalf("CA common name = %q, want hitman CA", caObj.cert.Subject.CommonName)
	}
	for _, name := range []string{"hitman-ca.pem", "hitman-ca.key"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("expected %s to exist: %v", name, err)
		}
	}
}
