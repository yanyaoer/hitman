package main

import (
	"context"
	"crypto/tls"
	"errors"
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
		NextProtos:     []string{"h2", "http/1.1"},
	}
	httpServer := &http.Server{Addr: cfg.ListenAddr, Handler: srv, TLSConfig: tlsCfg}

	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		log.Fatalf("fatal: listen %s: %v", cfg.ListenAddr, err)
	}
	log.Printf("listening on https://%s (socks %s, audit %s, ca %s)", cfg.ListenAddr, cfg.SocksAddr, cfg.AuditDir, cfg.CADir)

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
