package ssdp

import (
	"context"
	"net/netip"
)

// Interface names a network interface of the host, as the host does.
type Interface struct {
	// Index is the host's number for it, above zero.
	Index int
	// Name is the host's name for it: "en0", "eth0".
	Name string
}

// Port is the port SSDP's group is spoken to.
const Port = 1900

// Group is the IPv4 group SSDP is spoken to, 239.255.255.250, on Port.
func Group() netip.AddrPort {
	return netip.AddrPortFrom(netip.AddrFrom4([4]byte{239, 255, 255, 250}), Port)
}

// MaxMessage is more than any SSDP message: one is a single datagram,
// and devices keep theirs under a link's MTU.
const MaxMessage = 8192

// Network is a host's links, as they come and go. System is the host's
// own; a test, or a program with a network stack of its own, brings
// another.
type Network interface {
	// Watch tells handle first every link there is, each as Up, then
	// that this is all of them, as Settled, once; and then every link
	// that comes up or goes down, for as long as ctx lasts. Events come
	// one at a time. It returns ctx's error when ctx ends, or sooner with
	// why the links can no longer be told of; every link it told of is
	// closed by then.
	Watch(ctx context.Context, handle func(LinkEvent)) error
}

// LinkEvent is a link come up or gone down; or, with no link, the word
// that every link there was has been told of.
type LinkEvent struct {
	// Link is the link. Once it is told of as Down it has been closed.
	// There is none with Settled.
	Link Link
	// Change is what happened to it.
	Change LinkChange
}

// LinkChange is what happened to a link.
type LinkChange uint8

// The changes of a link.
const (
	// Up: the link is there, and can be read and written.
	Up LinkChange = iota + 1
	// Down: the link is gone.
	Down
	// Settled: every link there was when Watch began has been told of.
	Settled
)

// Link is one interface of the host: where SSDP's group is heard on it,
// and where searches leave from and are answered. Its methods may be
// called from several goroutines. A Link is told from another by ==.
//
//nolint:interfacebloat // what a link is, its two ways in, its way out and its end: none of the six is another's
type Link interface {
	// Interface is the interface the link is on.
	Interface() Interface
	// Addrs is the interface's IPv4 addresses, each with the prefix it
	// is on: what a device on this link can call this host back at, and
	// what tells a neighbour from a stranger.
	Addrs() []netip.Prefix
	// Heard waits for the next message sent to the group on this link,
	// copies it into p, which holds MaxMessage bytes, and says who sent
	// it. It returns an error when the link can be read no more, and for
	// nothing less: after Close, an error that is net.ErrClosed; when the
	// link is broken, any other. What goes wrong with one message, one
	// too large among it, is the link's to pass over.
	Heard(p []byte) (n int, from netip.AddrPort, err error)
	// Search sends p to the group, on this link, from a port of the
	// link's own: devices answer a search at the port it came from, and
	// some answer none that comes from the group's. A link on which
	// no search can be sent, try after try for a second, is given up as
	// broken.
	Search(p []byte) error
	// Answered waits for the next message sent to the port searches
	// leave from, copies it into p, and says who sent it, as Heard does.
	// An answer may come from off the link, where the group's messages
	// do not: a device a router away answers the host it heard.
	Answered(p []byte) (n int, from netip.AddrPort, err error)
	// Close gives the link up. A Heard and an Answered under way return.
	Close() error
}
