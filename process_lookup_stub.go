//go:build !darwin

package main

import (
	"context"
	"net/netip"
)

type processOwner struct {
	ProcessPath string
	UserID      int32
}

func findProcessOwner(context.Context, string, netip.AddrPort, netip.AddrPort) (processOwner, error) {
	return processOwner{}, errProcessNotFound
}
