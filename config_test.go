package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigReadsHitmanEnv(t *testing.T) {
	t.Setenv("HITMAN_LISTEN", "127.0.0.1:2222")
	t.Setenv("HITMAN_SOCKS", "direct")
	t.Setenv("HITMAN_MAX_CONTINUE", "2")
	t.Setenv("HITMAN_AUDIT_BODIES", "false")

	cfg := loadConfig()
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
	cfg := loadConfig()
	if cfg.MaxContinue != 0 {
		t.Fatalf("default MaxContinue = %d, want 0 (folding off by default)", cfg.MaxContinue)
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
