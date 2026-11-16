//go:build !unix && !windows

package ssdp

import (
	"net"
	"net/netip"
)

// outgoing makes a socket send what it sends to a group by the
// interface that has an address. Where the system has no way to say so,
// the socket's own address has to do.
func outgoing(*net.UDPConn, netip.Addr) error { return nil }
