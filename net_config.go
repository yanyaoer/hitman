package main

import (
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"strings"
)

const (
	defaultUpstreamMode      = "proxy"
	defaultNetDNSAddr        = "127.0.0.1:8472"
	defaultNetMITMAddr       = "127.0.0.1:8471"
	defaultFakeIPCIDR        = "198.18.0.0/15"
	defaultTunAddress        = "172.31.255.1/30"
	defaultUpstreamDNS       = "1.1.1.1:53"
	defaultResolverDomains   = "chatgpt.com,anthropic.com,googleapis.com"
	defaultDomainSuffixes    = "aiplatform.googleapis.com"
	defaultHitmanProcesses   = "codex,claude,claude.exe,gemini,omp,pi,agy"
	defaultInterfaceNameHint = ""
)

type netConfig struct {
	DNSAddr         string
	MITMAddr        string
	UpstreamMode    string
	UpstreamProxy   string
	UpstreamDNS     string
	FakeIPCIDR      netip.Prefix
	TunAddress      netip.Prefix
	InterfaceName   string
	Domains         []string
	DomainSuffixes  []string
	Processes       []string
	ProcessPaths    []string
	ResolverDomains []string
}

func loadNetConfig() (netConfig, error) {
	fakeCIDR, err := netip.ParsePrefix(envOr("HITMAN_FAKEIP_CIDR", defaultFakeIPCIDR))
	if err != nil {
		return netConfig{}, fmt.Errorf("parse HITMAN_FAKEIP_CIDR: %w", err)
	}
	if !fakeCIDR.Addr().Is4() {
		return netConfig{}, fmt.Errorf("HITMAN_FAKEIP_CIDR must be IPv4")
	}
	tunAddress, err := netip.ParsePrefix(envOr("HITMAN_TUN_ADDRESS", defaultTunAddress))
	if err != nil {
		return netConfig{}, fmt.Errorf("parse HITMAN_TUN_ADDRESS: %w", err)
	}
	if !tunAddress.Addr().Is4() {
		return netConfig{}, fmt.Errorf("HITMAN_TUN_ADDRESS must be IPv4")
	}
	mode := normalizeUpstreamMode(envOr("HITMAN_UPSTREAM_MODE", defaultUpstreamMode))
	proxy := normalizeProxy(envOr("HITMAN_UPSTREAM_PROXY", envOr("HITMAN_SOCKS", "127.0.0.1:2333")))
	c := netConfig{
		DNSAddr:         envOr("HITMAN_NETD_DNS", defaultNetDNSAddr),
		MITMAddr:        envOr("HITMAN_MITM_ADDR", defaultNetMITMAddr),
		UpstreamMode:    mode,
		UpstreamProxy:   proxy,
		UpstreamDNS:     envOr("HITMAN_DNS_UPSTREAM", defaultUpstreamDNS),
		FakeIPCIDR:      fakeCIDR.Masked(),
		TunAddress:      tunAddress,
		InterfaceName:   envOr("HITMAN_TUN_NAME", defaultInterfaceNameHint),
		Domains:         normalizeDomainList(splitCSV(envOr("HITMAN_DOMAINS", defaultAllowHosts))),
		DomainSuffixes:  normalizeDomainList(splitCSV(envOr("HITMAN_DOMAIN_SUFFIXES", defaultDomainSuffixes))),
		Processes:       normalizeProcessList(splitCSV(envOr("HITMAN_PROCESSES", defaultHitmanProcesses))),
		ProcessPaths:    normalizeProcessList(splitCSV(envOr("HITMAN_PROCESS_PATHS", ""))),
		ResolverDomains: normalizeDomainList(splitCSV(envOr("HITMAN_RESOLVER_DOMAINS", defaultResolverDomains))),
	}
	if _, _, err := net.SplitHostPort(c.DNSAddr); err != nil {
		return netConfig{}, fmt.Errorf("parse HITMAN_NETD_DNS: %w", err)
	}
	if _, _, err := net.SplitHostPort(c.MITMAddr); err != nil {
		return netConfig{}, fmt.Errorf("parse HITMAN_MITM_ADDR: %w", err)
	}
	if _, _, err := net.SplitHostPort(c.UpstreamDNS); err != nil {
		return netConfig{}, fmt.Errorf("parse HITMAN_DNS_UPSTREAM: %w", err)
	}
	if c.UpstreamMode == "proxy" && c.UpstreamProxy == "" {
		return netConfig{}, fmt.Errorf("HITMAN_UPSTREAM_PROXY is required when HITMAN_UPSTREAM_MODE=proxy")
	}
	if len(c.Domains) == 0 && len(c.DomainSuffixes) == 0 {
		return netConfig{}, fmt.Errorf("at least one HITMAN_DOMAINS or HITMAN_DOMAIN_SUFFIXES entry is required")
	}
	return c, nil
}

func normalizeUpstreamMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "proxy":
		return "proxy"
	case "system", "direct":
		return "system"
	default:
		return strings.ToLower(strings.TrimSpace(mode))
	}
}

func fakeIPPrefixesFromEnv() []netip.Prefix {
	prefix, err := netip.ParsePrefix(envOr("HITMAN_FAKEIP_CIDR", defaultFakeIPCIDR))
	if err != nil || !prefix.Addr().IsValid() {
		return nil
	}
	return []netip.Prefix{prefix.Masked()}
}

type targetMatcher struct {
	domains  map[string]struct{}
	suffixes []string
}

func newTargetMatcher(domains, suffixes []string) targetMatcher {
	m := targetMatcher{domains: make(map[string]struct{}, len(domains))}
	for _, domain := range normalizeDomainList(domains) {
		m.domains[domain] = struct{}{}
	}
	m.suffixes = normalizeDomainList(suffixes)
	return m
}

func (m targetMatcher) matches(host string) bool {
	host = normalizeDomain(host)
	if host == "" {
		return false
	}
	if _, ok := m.domains[host]; ok {
		return true
	}
	for _, suffix := range m.suffixes {
		if host == suffix || strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

type processMatcher struct {
	names map[string]struct{}
	paths map[string]struct{}
}

func newProcessMatcher(names, paths []string) processMatcher {
	m := processMatcher{
		names: make(map[string]struct{}, len(names)),
		paths: make(map[string]struct{}, len(paths)),
	}
	for _, name := range normalizeProcessList(names) {
		m.names[name] = struct{}{}
	}
	for _, path := range normalizeProcessList(paths) {
		m.paths[path] = struct{}{}
	}
	return m
}

func (m processMatcher) empty() bool {
	return len(m.names) == 0 && len(m.paths) == 0
}

func (m processMatcher) matches(processPath string) bool {
	if m.empty() {
		return true
	}
	processPath = strings.TrimSpace(processPath)
	if processPath == "" {
		return false
	}
	if _, ok := m.paths[processPath]; ok {
		return true
	}
	_, ok := m.names[filepath.Base(processPath)]
	return ok
}

func normalizeDomainList(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, value := range in {
		value = normalizeDomain(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func normalizeDomain(value string) string {
	value = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(value)), ".")
	return value
}

func normalizeProcessList(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, value := range in {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
