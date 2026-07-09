package main

import (
	"fmt"
	"net/netip"
	"sync"
)

type fakeIPStore struct {
	prefix netip.Prefix
	first  uint32
	last   uint32
	next   uint32

	mu     sync.RWMutex
	byHost map[string]netip.Addr
	byIP   map[netip.Addr]string
}

func newFakeIPStore(prefix netip.Prefix) (*fakeIPStore, error) {
	prefix = prefix.Masked()
	if !prefix.Addr().Is4() {
		return nil, fmt.Errorf("fake IP prefix must be IPv4")
	}
	bits := prefix.Bits()
	if bits > 30 {
		return nil, fmt.Errorf("fake IP prefix %s is too small", prefix)
	}
	base := ipv4ToUint32(prefix.Addr())
	size := uint32(1) << uint(32-bits)
	first := base + 1
	last := base + size - 2
	return &fakeIPStore{
		prefix: prefix,
		first:  first,
		last:   last,
		next:   first,
		byHost: make(map[string]netip.Addr),
		byIP:   make(map[netip.Addr]string),
	}, nil
}

func (s *fakeIPStore) addrForHost(host string) (netip.Addr, error) {
	host = normalizeDomain(host)
	if host == "" {
		return netip.Addr{}, fmt.Errorf("empty host")
	}
	s.mu.RLock()
	if addr, ok := s.byHost[host]; ok {
		s.mu.RUnlock()
		return addr, nil
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	if addr, ok := s.byHost[host]; ok {
		return addr, nil
	}
	if s.next > s.last {
		return netip.Addr{}, fmt.Errorf("fake IP pool exhausted: %s", s.prefix)
	}
	addr := uint32ToIPv4(s.next)
	s.next++
	s.byHost[host] = addr
	s.byIP[addr] = host
	return addr, nil
}

func (s *fakeIPStore) hostForAddr(addr netip.Addr) (string, bool) {
	addr = addr.Unmap()
	s.mu.RLock()
	defer s.mu.RUnlock()
	host, ok := s.byIP[addr]
	return host, ok
}

func ipv4ToUint32(addr netip.Addr) uint32 {
	a := addr.As4()
	return uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
}

func uint32ToIPv4(v uint32) netip.Addr {
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}
