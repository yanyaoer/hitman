//go:build darwin

package hitman

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/netip"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	darwinSnapshotTTL = 200 * time.Millisecond

	darwinXinpgenSize        = 24
	darwinXsocketOffset      = 104
	darwinXinpcbForeignPort  = 16
	darwinXinpcbLocalPort    = 18
	darwinXinpcbVFlag        = 44
	darwinXinpcbForeignAddr  = 48
	darwinXinpcbLocalAddr    = 64
	darwinXinpcbIPv4Addr     = 12
	darwinXsocketUID         = 64
	darwinXsocketLastPID     = 68
	darwinTCPExtraStructSize = 208
)

type processOwner struct {
	ProcessPath string
	UserID      int32
}

type darwinConnectionEntry struct {
	localAddr  netip.Addr
	remoteAddr netip.Addr
	localPort  uint16
	remotePort uint16
	pid        uint32
	uid        int32
}

type darwinSnapshot struct {
	createdAt time.Time
	entries   []darwinConnectionEntry
}

type darwinConnectionFinder struct {
	access    sync.Mutex
	ttl       time.Duration
	snapshots map[string]darwinSnapshot
}

var sharedDarwinFinder = &darwinConnectionFinder{
	ttl:       darwinSnapshotTTL,
	snapshots: make(map[string]darwinSnapshot),
}

func findProcessOwner(ctx context.Context, network string, source netip.AddrPort, destination netip.AddrPort) (processOwner, error) {
	select {
	case <-ctx.Done():
		return processOwner{}, ctx.Err()
	default:
	}
	return sharedDarwinFinder.find(network, source, destination)
}

func (f *darwinConnectionFinder) find(network string, source netip.AddrPort, destination netip.AddrPort) (processOwner, error) {
	source = normalizeDarwinAddrPort(source)
	destination = normalizeDarwinAddrPort(destination)
	var lastOwner *processOwner
	for attempt := range 2 {
		snapshot, fromCache, err := f.loadSnapshot(network, attempt > 0)
		if err != nil {
			return processOwner{}, err
		}
		entry, exact, err := matchDarwinConnectionEntry(snapshot.entries, network, source, destination)
		if err != nil {
			if errorsIsProcessNotFound(err) && fromCache {
				continue
			}
			return processOwner{}, err
		}
		if fromCache && !exact {
			continue
		}
		owner := &processOwner{UserID: entry.uid}
		lastOwner = owner
		if entry.pid == 0 {
			return *owner, nil
		}
		processPath, err := getExecPathFromPID(entry.pid)
		if err == nil {
			owner.ProcessPath = processPath
			return *owner, nil
		}
		if fromCache {
			continue
		}
		return *owner, nil
	}
	if lastOwner != nil {
		return *lastOwner, nil
	}
	return processOwner{}, errProcessNotFound
}

func (f *darwinConnectionFinder) loadSnapshot(network string, forceRefresh bool) (darwinSnapshot, bool, error) {
	f.access.Lock()
	defer f.access.Unlock()
	if !forceRefresh {
		if snapshot, ok := f.snapshots[network]; ok && time.Since(snapshot.createdAt) < f.ttl {
			return snapshot, true, nil
		}
	}
	snapshot, err := buildDarwinSnapshot(network)
	if err != nil {
		return darwinSnapshot{}, false, err
	}
	f.snapshots[network] = snapshot
	return snapshot, false, nil
}

func buildDarwinSnapshot(network string) (darwinSnapshot, error) {
	spath, itemSize, err := darwinSnapshotSettings(network)
	if err != nil {
		return darwinSnapshot{}, err
	}
	value, err := unix.SysctlRaw(spath)
	if err != nil {
		return darwinSnapshot{}, err
	}
	return darwinSnapshot{
		createdAt: time.Now(),
		entries:   parseDarwinSnapshot(value, itemSize),
	}, nil
}

func darwinSnapshotSettings(network string) (string, int, error) {
	itemSize := darwinStructSize
	switch network {
	case "tcp":
		return "net.inet.tcp.pcblist_n", itemSize + darwinTCPExtraStructSize, nil
	case "udp":
		return "net.inet.udp.pcblist_n", itemSize, nil
	default:
		return "", 0, os.ErrInvalid
	}
}

func parseDarwinSnapshot(buf []byte, itemSize int) []darwinConnectionEntry {
	if len(buf) <= darwinXinpgenSize || itemSize <= 0 {
		return nil
	}
	entries := make([]darwinConnectionEntry, 0, (len(buf)-darwinXinpgenSize)/itemSize)
	for i := darwinXinpgenSize; i+itemSize <= len(buf); i += itemSize {
		inp := i
		so := i + darwinXsocketOffset
		entry, ok := parseDarwinConnectionEntry(buf[inp:so], buf[so:so+darwinStructSize-darwinXsocketOffset])
		if ok {
			entries = append(entries, entry)
		}
	}
	return entries
}

func parseDarwinConnectionEntry(inp []byte, so []byte) (darwinConnectionEntry, bool) {
	if len(inp) < darwinXsocketOffset || len(so) < darwinStructSize-darwinXsocketOffset {
		return darwinConnectionEntry{}, false
	}
	entry := darwinConnectionEntry{
		remotePort: binary.BigEndian.Uint16(inp[darwinXinpcbForeignPort : darwinXinpcbForeignPort+2]),
		localPort:  binary.BigEndian.Uint16(inp[darwinXinpcbLocalPort : darwinXinpcbLocalPort+2]),
		pid:        binary.NativeEndian.Uint32(so[darwinXsocketLastPID : darwinXsocketLastPID+4]),
		uid:        int32(binary.NativeEndian.Uint32(so[darwinXsocketUID : darwinXsocketUID+4])),
	}
	flag := inp[darwinXinpcbVFlag]
	switch {
	case flag&0x1 != 0:
		entry.remoteAddr = netip.AddrFrom4([4]byte(inp[darwinXinpcbForeignAddr+darwinXinpcbIPv4Addr : darwinXinpcbForeignAddr+darwinXinpcbIPv4Addr+4]))
		entry.localAddr = netip.AddrFrom4([4]byte(inp[darwinXinpcbLocalAddr+darwinXinpcbIPv4Addr : darwinXinpcbLocalAddr+darwinXinpcbIPv4Addr+4]))
		return entry, true
	case flag&0x2 != 0:
		entry.remoteAddr = netip.AddrFrom16([16]byte(inp[darwinXinpcbForeignAddr : darwinXinpcbForeignAddr+16]))
		entry.localAddr = netip.AddrFrom16([16]byte(inp[darwinXinpcbLocalAddr : darwinXinpcbLocalAddr+16]))
		return entry, true
	default:
		return darwinConnectionEntry{}, false
	}
}

func matchDarwinConnectionEntry(entries []darwinConnectionEntry, network string, source netip.AddrPort, destination netip.AddrPort) (darwinConnectionEntry, bool, error) {
	sourceAddr := source.Addr()
	if !sourceAddr.IsValid() {
		return darwinConnectionEntry{}, false, os.ErrInvalid
	}
	var localFallback darwinConnectionEntry
	var hasLocalFallback bool
	var wildcardFallback darwinConnectionEntry
	var hasWildcardFallback bool
	for _, entry := range entries {
		if entry.localPort != source.Port() || sourceAddr.BitLen() != entry.localAddr.BitLen() {
			continue
		}
		if entry.localAddr == sourceAddr && destination.IsValid() && entry.remotePort == destination.Port() && entry.remoteAddr == destination.Addr() {
			return entry, true, nil
		}
		if !destination.IsValid() && entry.localAddr == sourceAddr {
			return entry, true, nil
		}
		if network != "udp" {
			continue
		}
		if !hasLocalFallback && entry.localAddr == sourceAddr {
			hasLocalFallback = true
			localFallback = entry
		}
		if !hasWildcardFallback && entry.localAddr.IsUnspecified() {
			hasWildcardFallback = true
			wildcardFallback = entry
		}
	}
	if hasLocalFallback {
		return localFallback, false, nil
	}
	if hasWildcardFallback {
		return wildcardFallback, false, nil
	}
	return darwinConnectionEntry{}, false, errProcessNotFound
}

func normalizeDarwinAddrPort(addrPort netip.AddrPort) netip.AddrPort {
	if !addrPort.IsValid() {
		return addrPort
	}
	return netip.AddrPortFrom(addrPort.Addr().Unmap(), addrPort.Port())
}

func getExecPathFromPID(pid uint32) (string, error) {
	const (
		procpidpathinfo     = 0xb
		procpidpathinfosize = 1024
		proccallnumpidinfo  = 0x2
	)
	buf := make([]byte, procpidpathinfosize)
	_, _, errno := syscall.Syscall6(
		syscall.SYS_PROC_INFO,
		proccallnumpidinfo,
		uintptr(pid),
		procpidpathinfo,
		0,
		uintptr(unsafe.Pointer(&buf[0])),
		procpidpathinfosize)
	if errno != 0 {
		return "", errno
	}
	return unix.ByteSliceToString(buf), nil
}

var darwinStructSize = func() int {
	value, _ := syscall.Sysctl("kern.osrelease")
	major := value
	for i, ch := range value {
		if ch == '.' {
			major = value[:i]
			break
		}
	}
	var n int
	_, _ = fmt.Sscanf(major, "%d", &n)
	if n >= 22 {
		return 408
	}
	return 384
}()

func errorsIsProcessNotFound(err error) bool {
	return err == errProcessNotFound
}
