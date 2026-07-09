//go:build darwin

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
)

const (
	resolverDir    = "/etc/resolver"
	resolverMarker = "# hitman managed resolver"
)

func installResolvers(domains []string, dnsAddr string) error {
	nameserver, port, err := resolverNameserverAndPort(dnsAddr)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(resolverDir, 0o755); err != nil {
		return err
	}
	for _, domain := range normalizeDomainList(domains) {
		path := filepath.Join(resolverDir, domain)
		if existing, err := os.ReadFile(path); err == nil && !bytes.Contains(existing, []byte(resolverMarker)) {
			return fmt.Errorf("%s exists and is not managed by hitman", path)
		}
		body := []byte(resolverMarker + "\n" +
			"# remove with ./hitman off\n" +
			"nameserver " + nameserver + "\n" +
			"port " + port + "\n")
		if err := os.WriteFile(path, body, 0o644); err != nil {
			return fmt.Errorf("write resolver %s: %w", path, err)
		}
	}
	return nil
}

func cleanupResolvers(domains []string) error {
	var firstErr error
	for _, domain := range normalizeDomainList(domains) {
		path := filepath.Join(resolverDir, domain)
		existing, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !bytes.Contains(existing, []byte(resolverMarker)) {
			continue
		}
		if err := os.Remove(path); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
