package hitman

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/miekg/dns"
)

type fakeDNSServer struct {
	addr     string
	upstream string
	store    *fakeIPStore
	matcher  targetMatcher

	udp *dns.Server
	tcp *dns.Server
}

func newFakeDNSServer(addr, upstream string, store *fakeIPStore, matcher targetMatcher) *fakeDNSServer {
	return &fakeDNSServer{
		addr:     addr,
		upstream: upstream,
		store:    store,
		matcher:  matcher,
	}
}

func (s *fakeDNSServer) Start() error {
	mux := dns.NewServeMux()
	mux.HandleFunc(".", s.handle)

	udpPacketConn, err := net.ListenPacket("udp", s.addr)
	if err != nil {
		return fmt.Errorf("listen DNS UDP %s: %w", s.addr, err)
	}
	tcpListener, err := net.Listen("tcp", s.addr)
	if err != nil {
		_ = udpPacketConn.Close()
		return fmt.Errorf("listen DNS TCP %s: %w", s.addr, err)
	}
	s.udp = &dns.Server{PacketConn: udpPacketConn, Handler: mux}
	s.tcp = &dns.Server{Listener: tcpListener, Handler: mux}
	go func() { _ = s.udp.ActivateAndServe() }()
	go func() { _ = s.tcp.ActivateAndServe() }()
	return nil
}

func (s *fakeDNSServer) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var err error
	if s.udp != nil {
		err = s.udp.ShutdownContext(ctx)
	}
	if s.tcp != nil {
		if tcpErr := s.tcp.ShutdownContext(ctx); err == nil {
			err = tcpErr
		}
	}
	return err
}

func (s *fakeDNSServer) handle(w dns.ResponseWriter, req *dns.Msg) {
	resp, err := s.answer(req)
	if err != nil {
		resp = new(dns.Msg)
		resp.SetRcode(req, dns.RcodeServerFailure)
	}
	_ = w.WriteMsg(resp)
}

func (s *fakeDNSServer) answer(req *dns.Msg) (*dns.Msg, error) {
	resp := new(dns.Msg)
	resp.SetReply(req)
	resp.Authoritative = true
	resp.RecursionAvailable = true
	for _, q := range req.Question {
		host := normalizeDomain(q.Name)
		if s.matcher.matches(host) {
			answers, err := s.answerTarget(q, host)
			if err != nil {
				return nil, err
			}
			resp.Answer = append(resp.Answer, answers...)
			continue
		}
		answers, err := s.forwardQuestion(q)
		if err != nil {
			return nil, err
		}
		resp.Answer = append(resp.Answer, answers...)
	}
	return resp, nil
}

func (s *fakeDNSServer) answerTarget(q dns.Question, host string) ([]dns.RR, error) {
	if q.Qclass != dns.ClassINET {
		return nil, nil
	}
	switch q.Qtype {
	case dns.TypeA:
		addr, err := s.store.addrForHost(host)
		if err != nil {
			return nil, err
		}
		return []dns.RR{&dns.A{
			Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30},
			A:   net.IP(addr.AsSlice()),
		}}, nil
	case dns.TypeAAAA, dns.TypeHTTPS, dns.TypeSVCB:
		return nil, nil
	default:
		return nil, nil
	}
}

func (s *fakeDNSServer) forwardQuestion(q dns.Question) ([]dns.RR, error) {
	req := new(dns.Msg)
	req.SetQuestion(q.Name, q.Qtype)
	req.Question[0].Qclass = q.Qclass
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, _, err := new(dns.Client).ExchangeContext(ctx, req, s.upstream)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, nil
	}
	return resp.Answer, nil
}

func resolverNameserverAndPort(addr string) (string, string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", "", err
	}
	if net.ParseIP(host) == nil {
		return "", "", fmt.Errorf("resolver nameserver must be an IP address: %s", host)
	}
	if _, err := strconv.Atoi(port); err != nil {
		return "", "", err
	}
	return host, port, nil
}
