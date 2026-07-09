package main

import (
	"context"
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestFakeIPStoreStableReverseMapping(t *testing.T) {
	store, err := newFakeIPStore(netip.MustParsePrefix("198.18.0.0/30"))
	if err != nil {
		t.Fatalf("newFakeIPStore: %v", err)
	}
	first, err := store.addrForHost("API.Anthropic.Com.")
	if err != nil {
		t.Fatalf("addrForHost first: %v", err)
	}
	second, err := store.addrForHost("api.anthropic.com")
	if err != nil {
		t.Fatalf("addrForHost second: %v", err)
	}
	if first != second {
		t.Fatalf("fake IP not stable: %s != %s", first, second)
	}
	host, ok := store.hostForAddr(first)
	if !ok || host != "api.anthropic.com" {
		t.Fatalf("reverse = %q,%v; want api.anthropic.com,true", host, ok)
	}
}

func TestTargetMatcherExactAndSuffix(t *testing.T) {
	matcher := newTargetMatcher(
		[]string{"chatgpt.com", "generativelanguage.googleapis.com"},
		[]string{"aiplatform.googleapis.com"},
	)
	for _, host := range []string{"chatgpt.com", "generativelanguage.googleapis.com", "us-central1-aiplatform.googleapis.com"} {
		if !matcher.matches(host) {
			t.Fatalf("expected %s to match", host)
		}
	}
	if matcher.matches("storage.googleapis.com") {
		t.Fatalf("storage.googleapis.com should not match")
	}
}

func TestProcessMatcherUsesBasenameAndPath(t *testing.T) {
	matcher := newProcessMatcher([]string{"codex", "claude"}, []string{"/opt/agents/custom"})
	for _, path := range []string{"/usr/local/bin/codex", "/Applications/Claude.app/Contents/MacOS/claude", "/opt/agents/custom"} {
		if !matcher.matches(path) {
			t.Fatalf("expected %s to match", path)
		}
	}
	if matcher.matches("/usr/bin/curl") {
		t.Fatalf("curl should not match")
	}
}

func TestHTTPConnectDialer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- err.Error()
			return
		}
		defer conn.Close()
		buf := make([]byte, 4096)
		n, err := conn.Read(buf)
		if err != nil {
			done <- err.Error()
			return
		}
		req := string(buf[:n])
		if !strings.Contains(req, "CONNECT example.com:443 HTTP/1.1") {
			done <- req
			return
		}
		_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		n, err = conn.Read(buf)
		if err != nil {
			done <- err.Error()
			return
		}
		_, _ = conn.Write([]byte("echo:" + string(buf[:n])))
		done <- "ok"
	}()

	dial, label, err := proxyDialContext("http://"+ln.Addr().String(), true)
	if err != nil {
		t.Fatalf("proxyDialContext: %v", err)
	}
	if !strings.HasPrefix(label, "http://") {
		t.Fatalf("label = %q", label)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := dial(ctx, "tcp", "example.com:443")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	reply := make([]byte, 9)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(reply) != "echo:ping" {
		t.Fatalf("reply = %q", string(reply))
	}
	if got := <-done; got != "ok" {
		t.Fatalf("proxy server got %q", got)
	}
}

func TestSystemDialerResolvesRealIP(t *testing.T) {
	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcpLn.Close()
	_, port, err := net.SplitHostPort(tcpLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan struct{}, 1)
	go func() {
		conn, err := tcpLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		accepted <- struct{}{}
	}()

	dnsAddr, closeDNS := startTestDNSServer(t, map[string]string{"example.test.": "127.0.0.1"})
	defer closeDNS()
	dial, label, err := upstreamDialContext("system", "", dnsAddr, []netip.Prefix{netip.MustParsePrefix("198.18.0.0/15")})
	if err != nil {
		t.Fatalf("upstreamDialContext: %v", err)
	}
	if label != "system:"+dnsAddr {
		t.Fatalf("label = %q", label)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := dial(ctx, "tcp", net.JoinHostPort("example.test", port))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.Close()
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatalf("server did not accept resolved real-IP connection")
	}
}

func TestSystemDialerRejectsFakeIP(t *testing.T) {
	dnsAddr, closeDNS := startTestDNSServer(t, map[string]string{"loop.test.": "198.18.0.8"})
	defer closeDNS()
	dial, _, err := upstreamDialContext("system", "", dnsAddr, []netip.Prefix{netip.MustParsePrefix("198.18.0.0/15")})
	if err != nil {
		t.Fatalf("upstreamDialContext: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = dial(ctx, "tcp", "loop.test:443")
	if err == nil || !strings.Contains(err.Error(), "blocked fake-IP") {
		t.Fatalf("err = %v, want blocked fake-IP rejection", err)
	}
}

func startTestDNSServer(t *testing.T, records map[string]string) (string, func()) {
	t.Helper()
	packetConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := dns.NewServeMux()
	mux.HandleFunc(".", func(w dns.ResponseWriter, req *dns.Msg) {
		resp := new(dns.Msg)
		resp.SetReply(req)
		for _, q := range req.Question {
			ipString, ok := records[q.Name]
			if !ok || q.Qtype != dns.TypeA {
				continue
			}
			resp.Answer = append(resp.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 1},
				A:   net.ParseIP(ipString).To4(),
			})
		}
		_ = w.WriteMsg(resp)
	})
	server := &dns.Server{PacketConn: packetConn, Handler: mux}
	go func() { _ = server.ActivateAndServe() }()
	return packetConn.LocalAddr().String(), func() { _ = server.Shutdown() }
}
