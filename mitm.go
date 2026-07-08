package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"sync"
	"time"
)

// certMinter signs a per-SNI leaf certificate on demand from the local CA and
// caches it, so the TLS listener can present a valid-looking cert for whatever
// hostname (e.g. chatgpt.com) the client asked for.
type certMinter struct {
	ca    *ca
	mu    sync.Mutex
	cache map[string]*tls.Certificate
}

func newCertMinter(c *ca) *certMinter {
	return &certMinter{ca: c, cache: map[string]*tls.Certificate{}}
}

func (m *certMinter) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	name := hello.ServerName
	if name == "" {
		name = "localhost"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.cache[name]; ok {
		return c, nil
	}
	c, err := m.mint(name)
	if err != nil {
		return nil, err
	}
	m.cache[name] = c
	return c, nil
}

func (m *certMinter) mint(name string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	if ip := net.ParseIP(name); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{name}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, m.ca.cert, &key.PublicKey, m.ca.key)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{
		Certificate: [][]byte{der, m.ca.cert.Raw},
		PrivateKey:  key,
	}, nil
}
