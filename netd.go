package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	tun "github.com/sagernet/sing-tun"
	"github.com/sagernet/sing/common/control"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

const (
	ipProtoTCP = 6
	ipProtoUDP = 17
)

type netDaemon struct {
	cfg              netConfig
	logger           stdTunLogger
	fakeIPs          *fakeIPStore
	targets          targetMatcher
	processes        processMatcher
	upstreamDial     func(context.Context, string, string) (net.Conn, error)
	upstreamLabel    string
	dns              *fakeDNSServer
	networkMonitor   tun.NetworkUpdateMonitor
	interfaceMonitor tun.DefaultInterfaceMonitor
	tunIf            tun.Tun
	stack            tun.Stack
	closeOnce        sync.Once
}

func runNetd() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	cfg, err := loadNetConfig()
	if err != nil {
		log.Fatalf("fatal: netd config: %v", err)
	}
	daemon, err := newNetDaemon(cfg)
	if err != nil {
		log.Fatalf("fatal: netd init: %v", err)
	}
	if err := daemon.Start(ctx); err != nil {
		_ = daemon.Close()
		log.Fatalf("fatal: netd start: %v", err)
	}
	log.Printf("netd ready (dns %s, fake %s, upstream %s, mitm %s)", cfg.DNSAddr, cfg.FakeIPCIDR, daemon.upstreamLabel, cfg.MITMAddr)
	<-ctx.Done()
	log.Printf("netd shutting down")
	_ = daemon.Close()
}

func newNetDaemon(cfg netConfig) (*netDaemon, error) {
	fakeIPs, err := newFakeIPStore(cfg.FakeIPCIDR)
	if err != nil {
		return nil, err
	}
	upstreamDial, upstreamLabel, err := upstreamDialContext(cfg.UpstreamMode, cfg.UpstreamProxy, cfg.UpstreamDNS, []netip.Prefix{cfg.FakeIPCIDR})
	if err != nil {
		return nil, err
	}
	targets := newTargetMatcher(cfg.Domains, cfg.DomainSuffixes)
	return &netDaemon{
		cfg:           cfg,
		logger:        stdTunLogger{prefix: "netd "},
		fakeIPs:       fakeIPs,
		targets:       targets,
		processes:     newProcessMatcher(cfg.Processes, cfg.ProcessPaths),
		upstreamDial:  upstreamDial,
		upstreamLabel: upstreamLabel,
		dns:           newFakeDNSServer(cfg.DNSAddr, cfg.UpstreamDNS, fakeIPs, targets),
	}, nil
}

func (d *netDaemon) Start(ctx context.Context) error {
	if err := d.dns.Start(); err != nil {
		return err
	}
	if err := installResolvers(d.cfg.ResolverDomains, d.cfg.DNSAddr); err != nil {
		return err
	}

	interfaceFinder := control.NewDefaultInterfaceFinder()
	networkMonitor, err := tun.NewNetworkUpdateMonitor(d.logger)
	if err != nil {
		return err
	}
	interfaceMonitor, err := tun.NewDefaultInterfaceMonitor(networkMonitor, d.logger, tun.DefaultInterfaceMonitorOptions{
		InterfaceFinder: interfaceFinder,
	})
	if err != nil {
		return err
	}
	d.networkMonitor = networkMonitor
	d.interfaceMonitor = interfaceMonitor
	if err := networkMonitor.Start(); err != nil {
		return err
	}
	if err := interfaceMonitor.Start(); err != nil {
		return err
	}

	name := d.cfg.InterfaceName
	if name == "" {
		name = tun.CalculateInterfaceName("")
	}
	tunOptions := tun.Options{
		Name:                    name,
		MTU:                     9000,
		Inet4Address:            []netip.Prefix{d.cfg.TunAddress},
		Inet4RouteAddress:       []netip.Prefix{d.cfg.FakeIPCIDR},
		DNSMode:                 tun.DNSModeDisabled,
		InterfaceFinder:         interfaceFinder,
		InterfaceMonitor:        interfaceMonitor,
		EXP_MultiPendingPackets: true,
		Logger:                  d.logger,
	}
	tunIf, err := tun.New(tunOptions)
	if err != nil {
		return err
	}
	d.tunIf = tunIf
	stack, err := tun.NewStack("gvisor", tun.StackOptions{
		Context:         ctx,
		Tun:             tunIf,
		TunOptions:      tunOptions,
		UDPTimeout:      30 * time.Second,
		ICMPTimeout:     30 * time.Second,
		Handler:         d,
		Logger:          d.logger,
		InterfaceFinder: interfaceFinder,
	})
	if err != nil {
		return err
	}
	if err := stack.Start(); err != nil {
		return err
	}
	d.stack = stack
	if err := tunIf.Start(); err != nil {
		return err
	}
	if actualName, err := tunIf.Name(); err == nil {
		log.Printf("netd tun started at %s", actualName)
	}
	return nil
}

func (d *netDaemon) Close() error {
	var err error
	d.closeOnce.Do(func() {
		err = errors.Join(
			closeIf(d.stack),
			closeIf(d.tunIf),
			closeIf(d.interfaceMonitor),
			closeIf(d.networkMonitor),
			closeIf(d.dns),
			cleanupResolvers(d.cfg.ResolverDomains),
		)
	})
	return err
}

func closeIf(v interface{ Close() error }) error {
	if v == nil {
		return nil
	}
	return v.Close()
}

func (d *netDaemon) JudgeFlow(network uint8, _ netip.AddrPort, destination netip.AddrPort, _ []byte) tun.FlowVerdict {
	if network != ipProtoTCP || destination.Port() != 443 {
		return tun.FlowVerdict{Action: tun.ActionDrop}
	}
	if _, ok := d.fakeIPs.hostForAddr(destination.Addr()); !ok {
		return tun.FlowVerdict{Action: tun.ActionDrop}
	}
	return tun.FlowVerdict{Action: tun.ActionAccept}
}

func (d *netDaemon) NewConnectionEx(ctx context.Context, conn net.Conn, source M.Socksaddr, destination M.Socksaddr, onClose N.CloseHandlerFunc) {
	var closeErr error
	defer func() {
		_ = conn.Close()
		if onClose != nil {
			onClose(closeErr)
		}
	}()
	host, ok := d.fakeIPs.hostForAddr(destination.Addr)
	if !ok {
		closeErr = errors.New("unknown fake IP")
		return
	}
	if !d.targets.matches(host) {
		closeErr = errors.New("host no longer matches target")
		return
	}
	targetAddr := net.JoinHostPort(host, strconv.Itoa(int(destination.Port)))
	if destination.Port == 0 {
		targetAddr = net.JoinHostPort(host, "443")
	}

	matched, ownerPath := d.matchesProcess(ctx, source.AddrPort(), destination.AddrPort())
	var upstream net.Conn
	var err error
	if matched {
		upstream, err = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, "tcp", d.cfg.MITMAddr)
		if err == nil {
			log.Printf("netd mitm %s -> %s (%s)", source, host, ownerPath)
		}
	} else {
		upstream, err = d.upstreamDial(ctx, "tcp", targetAddr)
		if err == nil {
			if ownerPath == "" {
				ownerPath = "unknown-process"
			}
			log.Printf("netd pass %s -> %s (%s)", source, host, ownerPath)
		}
	}
	if err != nil {
		closeErr = err
		return
	}
	closeErr = pipeConns(conn, upstream)
}

func (d *netDaemon) NewPacketConnectionEx(_ context.Context, conn N.PacketConn, _ M.Socksaddr, _ M.Socksaddr, onClose N.CloseHandlerFunc) {
	err := conn.Close()
	if onClose != nil {
		onClose(err)
	}
}

func (d *netDaemon) matchesProcess(ctx context.Context, source netip.AddrPort, destination netip.AddrPort) (bool, string) {
	if d.processes.empty() {
		return true, "host-level"
	}
	var owner processOwner
	var err error
	for attempt := range 3 {
		owner, err = findProcessOwner(ctx, "tcp", source, destination)
		if err == nil {
			break
		}
		if attempt < 2 {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if err != nil {
		log.Printf("netd process lookup miss %s -> %s: %v", source, destination, err)
		return false, ""
	}
	return d.processes.matches(owner.ProcessPath), owner.ProcessPath
}

func pipeConns(a, b net.Conn) error {
	defer b.Close()
	errc := make(chan error, 2)
	go func() {
		_, err := io.Copy(a, b)
		_ = a.Close()
		errc <- err
	}()
	go func() {
		_, err := io.Copy(b, a)
		_ = b.Close()
		errc <- err
	}()
	err := <-errc
	if err == nil || errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrClosed) {
		return nil
	}
	return err
}

type stdTunLogger struct {
	prefix string
}

func (l stdTunLogger) Trace(args ...any) { log.Print(append([]any{l.prefix}, args...)...) }
func (l stdTunLogger) Debug(args ...any) { log.Print(append([]any{l.prefix}, args...)...) }
func (l stdTunLogger) Info(args ...any)  { log.Print(append([]any{l.prefix}, args...)...) }
func (l stdTunLogger) Warn(args ...any)  { log.Print(append([]any{l.prefix}, args...)...) }
func (l stdTunLogger) Error(args ...any) { log.Print(append([]any{l.prefix}, args...)...) }
func (l stdTunLogger) Fatal(args ...any) { log.Fatal(append([]any{l.prefix}, args...)...) }
func (l stdTunLogger) Panic(args ...any) { log.Panic(append([]any{l.prefix}, args...)...) }
