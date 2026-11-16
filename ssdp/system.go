package ssdp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
)

// System is the host's own network: every interface that is up, takes
// multicast and has an IPv4 address, as it stands when Watch begins. An
// interface that comes up, goes down or changes its address after that
// is not told of. Where SSDP is spoken on some interface and none can be
// opened, Watch ends with why: a program the system keeps off the
// network is told so.
//
//nolint:iface // a Network, whatever it is made of on each system: the API's word for it
func System() Network { return system{} }

type system struct{}

func (system) Watch(ctx context.Context, handle func(LinkEvent)) error {
	interfaces, err := net.Interfaces()
	if err != nil {
		return fmt.Errorf("the host's interfaces: %w", err)
	}

	var (
		links  []*hostLink
		closed error
	)

	defer func() {
		for _, link := range links {
			_ = link.Close()
		}
	}()

	for index := range interfaces {
		// One that cannot be opened is one the host is not on: another
		// program holds the port to itself, or the interface went.
		link, openErr := linkOf(ctx, &interfaces[index])
		if openErr != nil || link == nil {
			closed = errors.Join(closed, openErr)

			continue
		}

		links = append(links, link)

		handle(LinkEvent{Link: link, Change: Up})
	}

	if len(links) == 0 && closed != nil {
		return fmt.Errorf("the host's network: no interface can be opened: %w", closed)
	}

	handle(LinkEvent{Change: Settled})

	<-ctx.Done()

	return fmt.Errorf("the host's network: %w", context.Cause(ctx))
}

// hostLink is one interface of the host: a socket in the group, on the
// group's port, and one on a port of its own that searches leave from.
type hostLink struct {
	iface Interface
	addrs []netip.Prefix
	group *net.UDPConn
	own   *net.UDPConn
	// whole is buffers a datagram is read into, each larger than any.
	whole sync.Pool
}

// linkOf is the link of an interface, or none when SSDP is not spoken on
// it: one that is down, takes no multicast, or has no IPv4 address.
func linkOf(ctx context.Context, iface *net.Interface) (*hostLink, error) {
	if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagMulticast == 0 {
		return nil, nil //nolint:nilnil // no link and no error: SSDP is not spoken there
	}

	addrs, err := iface.Addrs()
	if err != nil {
		return nil, fmt.Errorf("the addresses of %s: %w", iface.Name, err)
	}

	link := &hostLink{iface: Interface{Index: iface.Index, Name: iface.Name}}

	for _, addr := range addrs {
		network, isNetwork := addr.(*net.IPNet)
		if !isNetwork {
			continue
		}

		if prefix, parseErr := netip.ParsePrefix(network.String()); parseErr == nil && prefix.Addr().Is4() {
			link.addrs = append(link.addrs, prefix)
		}
	}

	if len(link.addrs) == 0 {
		return nil, nil //nolint:nilnil // no link and no error: SSDP is not spoken there
	}

	link.group, err = net.ListenMulticastUDP("udp4", iface, net.UDPAddrFromAddrPort(Group()))
	if err != nil {
		return nil, fmt.Errorf("joining the group on %s: %w", iface.Name, err)
	}

	var listening net.ListenConfig

	from := link.addrs[0].Addr()

	own, err := listening.ListenPacket(ctx, "udp4", netip.AddrPortFrom(from, 0).String())
	if err != nil {
		_ = link.group.Close()

		return nil, fmt.Errorf("a port on %s: %w", iface.Name, err)
	}

	sending, isUDP := own.(*net.UDPConn)

	// A socket bound to an address does not, on every system, send to a
	// group by the interface that has the address.
	if !isUDP || outgoing(sending, from) != nil {
		_ = own.Close()
		_ = link.group.Close()

		return nil, fmt.Errorf("%w: sending to the group by %s", errors.ErrUnsupported, iface.Name)
	}

	link.own = sending

	return link, nil
}

func (l *hostLink) Interface() Interface  { return l.iface }
func (l *hostLink) Addrs() []netip.Prefix { return l.addrs }

// String is the link as a person reads it: its interface, and the
// address and port its searches leave from.
func (l *hostLink) String() string {
	return l.iface.Name + " " + l.own.LocalAddr().String()
}

// Heard reads what is sent to the group by a neighbour on this link. A
// socket in the group hears, on some systems, what arrives for it on
// any interface: what comes from no network of this one is another
// link's, or a stranger's.
func (l *hostLink) Heard(into []byte) (int, netip.AddrPort, error) {
	for {
		count, from, err := l.receive(l.group, into)
		if err != nil {
			return 0, netip.AddrPort{}, fmt.Errorf("the group on %s: %w", l.iface.Name, err)
		}

		for _, prefix := range l.addrs {
			if prefix.Contains(from.Addr()) {
				return count, from, nil
			}
		}
	}
}

func (l *hostLink) Search(datagram []byte) error {
	if _, err := l.own.WriteToUDPAddrPort(datagram, Group()); err != nil {
		return fmt.Errorf("searching on %s: %w", l.iface.Name, err)
	}

	return nil
}

func (l *hostLink) Answered(into []byte) (int, netip.AddrPort, error) {
	count, from, err := l.receive(l.own, into)
	if err != nil {
		return 0, netip.AddrPort{}, fmt.Errorf("answers on %s: %w", l.iface.Name, err)
	}

	return count, from, nil
}

func (l *hostLink) Close() error {
	if err := errors.Join(l.group.Close(), l.own.Close()); err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("closing %s: %w", l.iface.Name, err)
	}

	return nil
}

// maxDatagram is more than any datagram holds: read into less, one that
// is larger is cut short on some systems and is an error on others.
const maxDatagram = 1 << 16

// broken is how many reads in a row may fail before a socket is taken
// for broken. One read failing says nothing of the socket: a datagram
// that was refused somewhere comes back, on some systems, as the error
// of the next read.
const broken = 16

// receive reads the next datagram of a size SSDP has from a socket, and
// who sent it. What is larger is passed over, and so is a read that
// fails, until the socket is closed or too many have failed in a row.
func (l *hostLink) receive(conn *net.UDPConn, into []byte) (int, netip.AddrPort, error) {
	kept, pooled := l.whole.Get().(*[]byte)
	if !pooled {
		made := make([]byte, maxDatagram)
		kept = &made
	}

	defer l.whole.Put(kept)

	whole := *kept

	for failures := 0; ; {
		count, from, err := conn.ReadFromUDPAddrPort(whole)

		switch {
		case err == nil && count <= len(into):
			return copy(into, whole[:count]), netip.AddrPortFrom(from.Addr().Unmap(), from.Port()), nil
		case err == nil:
			failures = 0
		case errors.Is(err, net.ErrClosed):
			return 0, netip.AddrPort{}, err //nolint:wrapcheck // its caller says which socket
		default:
			failures++
			if failures >= broken {
				return 0, netip.AddrPort{}, err //nolint:wrapcheck // its caller says which socket
			}
		}
	}
}
