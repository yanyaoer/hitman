package hitman

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func runServe() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("fatal: config: %v", err)
	}

	caObj, err := loadOrCreateCA(cfg.CADir)
	if err != nil {
		log.Fatalf("fatal: CA init: %v", err)
	}
	minter := newCertMinter(caObj)
	audit := newAuditor(cfg.AuditDir, cfg.AuditBodies)
	srv := newServer(cfg, audit)

	tlsCfg := &tls.Config{
		GetCertificate: minter.getCertificate,
		MinVersion:     tls.VersionTLS12,
		// http/1.1 only: codex negotiates WebSocket-over-h2 (RFC 8441 extended
		// CONNECT) for the responses endpoint, which Go's h2 server rejects and
		// resets the whole multiplexed connection. Forcing http/1.1 keeps every
		// codex request on a transport our handler serves, and lets codex fall
		// back to the HTTP/SSE path we fold.
		NextProtos: []string{"http/1.1"},
	}
	httpServer := &http.Server{Addr: cfg.ListenAddr, Handler: srv, TLSConfig: tlsCfg}

	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		log.Fatalf("fatal: listen %s: %v", cfg.ListenAddr, err)
	}
	log.Printf("listening on https://%s (egress %s, audit %s, ca %s)", cfg.ListenAddr, upstreamLabel(cfg), cfg.AuditDir, cfg.CADir)

	go func() {
		if err := httpServer.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("fatal: serve: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Printf("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(ctx)
}

func upstreamLabel(cfg appConfig) string {
	_, label, err := upstreamDialContext(cfg.UpstreamMode, cfg.UpstreamProxy, cfg.UpstreamDNS, []netip.Prefix{cfg.FakeIPCIDR})
	if err != nil {
		return "invalid (" + err.Error() + ")"
	}
	return label
}
