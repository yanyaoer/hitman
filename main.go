package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("ai-bridge ")

	// This binary is the gateway server and takes no subcommands; control commands
	// (init/on/off/status/...) live in the `bridge` script. Guard against the easy
	// mix-up so `./ai-bridge init` gives a hint instead of trying to bind the port.
	if len(os.Args) > 1 {
		fmt.Fprintln(os.Stderr, "ai-bridge is the gateway server binary and takes no subcommands.")
		fmt.Fprintf(os.Stderr, "Did you mean the control script?  ./bridge %s\n", os.Args[1])
		os.Exit(2)
	}

	cfg := loadConfig()

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
	egress := "direct (via TUN)"
	if cfg.SocksAddr != "" {
		egress = "socks " + cfg.SocksAddr
	}
	log.Printf("listening on https://%s (egress %s, audit %s, ca %s)", cfg.ListenAddr, egress, cfg.AuditDir, cfg.CADir)

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
