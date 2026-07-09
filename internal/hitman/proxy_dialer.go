package hitman

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"
)

func normalizeProxy(v string) string {
	v = strings.TrimSpace(v)
	switch strings.ToLower(v) {
	case "", "direct", "none", "off", "-":
		return ""
	}
	if !strings.Contains(v, "://") {
		return "socks5://" + v
	}
	return v
}

// normalizeSocks is kept for old tests and old HITMAN_SOCKS semantics.
func normalizeSocks(v string) string {
	return normalizeProxy(v)
}

func proxyDialContext(proxyAddr string, requireProxy bool) (func(context.Context, string, string) (net.Conn, error), string, error) {
	proxyAddr = normalizeProxy(proxyAddr)
	baseDialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	if proxyAddr == "" {
		if requireProxy {
			return nil, "", fmt.Errorf("upstream proxy is required")
		}
		return baseDialer.DialContext, "direct", nil
	}
	u, err := url.Parse(proxyAddr)
	if err != nil {
		return nil, "", err
	}
	if u.Host == "" {
		return nil, "", fmt.Errorf("proxy address missing host: %s", proxyAddr)
	}
	switch strings.ToLower(u.Scheme) {
	case "socks", "socks5":
		return socks5DialContext(baseDialer, u.Host), "socks5://" + u.Host, nil
	case "http":
		return httpConnectDialContext(baseDialer, u.Host), "http://" + u.Host, nil
	default:
		return nil, "", fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
	}
}

func upstreamDialContext(mode, proxyAddr, dnsAddr string, blockedPrefixes []netip.Prefix) (func(context.Context, string, string) (net.Conn, error), string, error) {
	mode = normalizeUpstreamMode(mode)
	switch mode {
	case "proxy":
		return proxyDialContext(proxyAddr, true)
	case "system":
		if _, _, err := net.SplitHostPort(dnsAddr); err != nil {
			return nil, "", fmt.Errorf("parse upstream DNS: %w", err)
		}
		return realIPDialContext(&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}, dnsAddr, blockedPrefixes), "system:" + dnsAddr, nil
	default:
		return nil, "", fmt.Errorf("unsupported upstream mode %q", mode)
	}
}

func realIPDialContext(baseDialer *net.Dialer, dnsAddr string, blockedPrefixes []netip.Prefix) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ip, err := resolveRealIP(ctx, dnsAddr, host, blockedPrefixes)
		if err != nil {
			return nil, err
		}
		return baseDialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
	}
}

func resolveRealIP(ctx context.Context, dnsAddr, host string, blockedPrefixes []netip.Prefix) (netip.Addr, error) {
	if addr, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		if prefixContains(blockedPrefixes, addr) {
			return netip.Addr{}, fmt.Errorf("resolved address %s is inside blocked fake-IP range", addr)
		}
		return addr, nil
	}
	host = dns.Fqdn(normalizeDomain(host))
	for _, qtype := range []uint16{dns.TypeA, dns.TypeAAAA} {
		msg := new(dns.Msg)
		msg.SetQuestion(host, qtype)
		resp, _, err := new(dns.Client).ExchangeContext(ctx, msg, dnsAddr)
		if err != nil {
			return netip.Addr{}, err
		}
		if resp == nil {
			continue
		}
		for _, rr := range resp.Answer {
			var addr netip.Addr
			switch v := rr.(type) {
			case *dns.A:
				addr, _ = netip.AddrFromSlice(v.A)
			case *dns.AAAA:
				addr, _ = netip.AddrFromSlice(v.AAAA)
			default:
				continue
			}
			if !addr.IsValid() {
				continue
			}
			if prefixContains(blockedPrefixes, addr) {
				return netip.Addr{}, fmt.Errorf("resolved address %s is inside blocked fake-IP range", addr)
			}
			return addr, nil
		}
	}
	return netip.Addr{}, fmt.Errorf("no A/AAAA record for %s via %s", host, dnsAddr)
}

func prefixContains(prefixes []netip.Prefix, addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, prefix := range prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// socks5DialContext returns a DialContext that tunnels TCP through a SOCKS5 proxy
// using a domain CONNECT target so DNS/routing happens inside sing-box.
func socks5DialContext(baseDialer *net.Dialer, socksAddr string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, portStr, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return nil, err
		}
		conn, err := baseDialer.DialContext(ctx, "tcp", socksAddr)
		if err != nil {
			return nil, err
		}
		if err := withHandshakeDeadline(ctx, conn, func() error {
			return socks5Handshake(conn, host, port)
		}); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return conn, nil
	}
}

func httpConnectDialContext(baseDialer *net.Dialer, proxyAddr string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := baseDialer.DialContext(ctx, "tcp", proxyAddr)
		if err != nil {
			return nil, err
		}
		var out net.Conn
		if err := withHandshakeDeadline(ctx, conn, func() error {
			var hErr error
			out, hErr = httpConnectHandshake(conn, addr)
			return hErr
		}); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return out, nil
	}
}

func withHandshakeDeadline(ctx context.Context, conn net.Conn, fn func() error) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(15 * time.Second)
	}
	_ = conn.SetDeadline(deadline)
	err := fn()
	_ = conn.SetDeadline(time.Time{})
	return err
}

func socks5Handshake(conn net.Conn, host string, port int) error {
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return err
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return err
	}
	if reply[0] != 0x05 || reply[1] != 0x00 {
		return fmt.Errorf("socks5: unsupported auth method %d", reply[1])
	}
	if len(host) > 255 {
		return fmt.Errorf("socks5: host too long")
	}
	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
	req = append(req, host...)
	req = append(req, byte(port>>8), byte(port&0xff))
	if _, err := conn.Write(req); err != nil {
		return err
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return err
	}
	if head[1] != 0x00 {
		return fmt.Errorf("socks5: connect failed (rep=%d)", head[1])
	}
	switch head[3] {
	case 0x01:
		_, err := io.ReadFull(conn, make([]byte, 4+2))
		return err
	case 0x04:
		_, err := io.ReadFull(conn, make([]byte, 16+2))
		return err
	case 0x03:
		l := make([]byte, 1)
		if _, err := io.ReadFull(conn, l); err != nil {
			return err
		}
		_, err := io.ReadFull(conn, make([]byte, int(l[0])+2))
		return err
	default:
		return fmt.Errorf("socks5: unknown atyp %d", head[3])
	}
}

func httpConnectHandshake(conn net.Conn, target string) (net.Conn, error) {
	req := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\nProxy-Connection: Keep-Alive\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("http proxy connect failed: %s", resp.Status)
	}
	if br.Buffered() == 0 {
		return conn, nil
	}
	return &bufferedConn{Conn: conn, r: br}, nil
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	if c.r != nil && c.r.Buffered() > 0 {
		return c.r.Read(p)
	}
	c.r = nil
	return c.Conn.Read(p)
}
