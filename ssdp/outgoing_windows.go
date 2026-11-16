//go:build windows

package ssdp

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
)

// outgoing makes a socket send what it sends to a group by the
// interface that has an address.
func outgoing(conn *net.UDPConn, addr netip.Addr) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return fmt.Errorf("the socket: %w", err)
	}

	var failed error

	err = raw.Control(func(descriptor uintptr) {
		failed = syscall.SetsockoptInet4Addr(
			syscall.Handle(descriptor),
			syscall.IPPROTO_IP,
			syscall.IP_MULTICAST_IF,
			addr.As4(),
		)
	})
	if err != nil || failed != nil {
		return fmt.Errorf("the interface multicast leaves by: %w", errors.Join(err, failed))
	}

	return nil
}
