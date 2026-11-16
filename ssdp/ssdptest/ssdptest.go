// Package ssdptest is networks, hosts and devices in a test's process:
// SSDP as devices speak it, with no socket. A LAN carries what is sent
// to the group to everything on it; a Host is an ssdp.Network of the
// links a test plugs into LANs; a Device announces, answers searches,
// and can be made to say anything at all.
package ssdptest

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/forkcloser/go-upnp/ssdp"
)

// errUnplugged is what a link that was closed reads.
var errUnplugged = fmt.Errorf("the link is closed: %w", net.ErrClosed)

// backlog is how many datagrams wait to be read on a link; past it they
// are dropped, as a socket drops them.
const backlog = 1024

// LAN is one network: what is sent to the group on it reaches every
// link and every device on it.
type LAN struct {
	mu       sync.Mutex
	links    []*link
	devices  []*Device
	searches []string
	ports    uint16
}

// NewLAN makes a network with nothing on it.
func NewLAN() *LAN { return &LAN{ports: firstPort} }

// firstPort is the first port a link searches from.
const firstPort = 50000

// Searches is every search that was sent to the group on the LAN, as it
// was sent, in the order they came.
func (l *LAN) Searches() []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]string(nil), l.searches...)
}

// Host is the network of one host: an ssdp.Network, of the links it is
// plugged into LANs with.
type Host struct {
	mu       sync.Mutex
	links    []*link
	watching func(ssdp.LinkEvent)
	// failed ends the Watch with an error.
	failed chan error
}

// NewHost makes a host with no link.
func NewHost() *Host { return &Host{failed: make(chan error, 1)} }

// Plug puts the host on a LAN, through an interface of that name and
// number, at an address with its prefix. A Watch under way is told the
// link is up.
func (h *Host) Plug(lan *LAN, name string, index int, addr netip.Prefix) {
	lan.mu.Lock()
	lan.ports++
	plugged := &link{
		lan: lan, iface: ssdp.Interface{Index: index, Name: name}, addr: addr,
		port:  netip.AddrPortFrom(addr.Addr(), lan.ports),
		heard: make(chan packet, backlog), answered: make(chan packet, backlog), closed: make(chan struct{}),
		broken: make(chan struct{}),
	}
	lan.links = append(lan.links, plugged)
	lan.mu.Unlock()

	h.mu.Lock()
	defer h.mu.Unlock()

	h.links = append(h.links, plugged)

	if h.watching != nil {
		h.watching(ssdp.LinkEvent{Link: plugged, Change: ssdp.Up})
	}
}

// Unplug takes the host's interface of that name off its LAN. A Watch
// under way is told the link is down.
func (h *Host) Unplug(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for index, one := range h.links {
		if one.iface.Name != name {
			continue
		}

		h.links = append(h.links[:index], h.links[index+1:]...)
		_ = one.Close()

		if h.watching != nil {
			h.watching(ssdp.LinkEvent{Link: one, Change: ssdp.Down})
		}

		return
	}
}

// Sever breaks the host's interface of that name with no word from the
// network: what is read on it from now on fails with that error.
func (h *Host) Sever(name string, err error) {
	if one := h.named(name); one != nil {
		one.breaking.Do(func() {
			one.severed = err

			close(one.broken)
		})
	}
}

// Jam makes every search on the host's interface of that name fail with
// that error, from now on: a link that hears and cannot ask. With no
// error, its searches leave again.
func (h *Host) Jam(name string, err error) {
	if one := h.named(name); one != nil {
		one.jammed.Store(&err)
	}
}

// Open says whether the host's interface of that name is still open:
// whether nothing has closed its link.
func (h *Host) Open(name string) bool {
	one := h.named(name)
	if one == nil {
		return false
	}

	select {
	case <-one.closed:
		return false
	default:
		return true
	}
}

// Fail ends the Watch under way with an error: a network that can tell
// of its links no more.
func (h *Host) Fail(err error) { h.failed <- err }

// Watch is ssdp.Network's.
func (h *Host) Watch(ctx context.Context, handle func(ssdp.LinkEvent)) error {
	h.mu.Lock()

	for _, one := range h.links {
		handle(ssdp.LinkEvent{Link: one, Change: ssdp.Up})
	}

	handle(ssdp.LinkEvent{Change: ssdp.Settled})

	h.watching = handle
	h.mu.Unlock()

	var err error

	select {
	case <-ctx.Done():
		err = context.Cause(ctx)
	case err = <-h.failed:
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	h.watching = nil

	for _, one := range h.links {
		_ = one.Close()
	}

	return err
}

// named is the host's link on the interface of that name.
func (h *Host) named(name string) *link {
	h.mu.Lock()
	defer h.mu.Unlock()

	for _, one := range h.links {
		if one.iface.Name == name {
			return one
		}
	}

	return nil
}

// packet is one datagram and who sent it.
type packet struct {
	from     netip.AddrPort
	datagram string
}

// link is a host's interface on a LAN.
type link struct {
	lan   *LAN
	iface ssdp.Interface
	addr  netip.Prefix
	// port is where its searches leave from and are answered.
	port     netip.AddrPort
	heard    chan packet
	answered chan packet
	closing  sync.Once
	closed   chan struct{}
	// broken is closed when the link was severed, with severed as why;
	// jammed is why its searches fail, when they do.
	breaking sync.Once
	broken   chan struct{}
	severed  error
	jammed   atomic.Pointer[error]
}

func (l *link) Interface() ssdp.Interface { return l.iface }
func (l *link) Addrs() []netip.Prefix     { return []netip.Prefix{l.addr} }

func (l *link) Heard(into []byte) (int, netip.AddrPort, error) { return l.next(l.heard, into) }

func (l *link) Answered(into []byte) (int, netip.AddrPort, error) { return l.next(l.answered, into) }

func (l *link) Search(datagram []byte) error {
	select {
	case <-l.closed:
		return errUnplugged
	default:
	}

	if jammed := l.jammed.Load(); jammed != nil && *jammed != nil {
		return *jammed
	}

	l.lan.group(l.port, string(datagram), l)

	return nil
}

func (l *link) Close() error {
	l.closing.Do(func() { close(l.closed) })

	return nil
}

func (l *link) next(from chan packet, into []byte) (int, netip.AddrPort, error) {
	select {
	case one := <-from:
		return copy(into, one.datagram), one.from, nil
	case <-l.closed:
		return 0, netip.AddrPort{}, errUnplugged
	case <-l.broken:
		return 0, netip.AddrPort{}, l.severed
	}
}

// deliver hands a datagram to what reads a link, or drops it when too
// much waits there already.
func deliver(reading chan packet, from netip.AddrPort, datagram string) {
	select {
	case reading <- packet{from: from, datagram: datagram}:
	default:
	}
}

// Advert is one thing a Device advertises.
type Advert struct {
	// USN and Type are its name and what it advertises.
	USN  string
	Type string
	// Location is the URL of the device's description.
	Location string
	// MaxAge is how long it says it holds, in seconds. Zero says
	// nothing, which a control point reads as half an hour.
	MaxAge int
	// Header is headers a maker adds, each as it goes on the wire.
	Header map[string]string
}

// Device is a device on a LAN: it answers the searches it hears, and
// says what a test has it say.
type Device struct {
	lan  *LAN
	addr netip.Addr

	mu      sync.Mutex
	adverts []Advert
	boot    int
	mute    bool
}

// Device puts a device on the LAN, at an address, advertising what is
// given. It announces nothing until told to.
func (l *LAN) Device(addr netip.Addr, adverts ...Advert) *Device {
	device := &Device{lan: l, addr: addr, adverts: adverts}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.devices = append(l.devices, device)

	return device
}

// group delivers a datagram sent to the group.
func (l *LAN) group(from netip.AddrPort, datagram string, sender *link) {
	l.mu.Lock()
	links := append([]*link(nil), l.links...)
	devices := append([]*Device(nil), l.devices...)

	if strings.HasPrefix(datagram, "M-SEARCH") {
		l.searches = append(l.searches, datagram)
	}

	l.mu.Unlock()

	for _, one := range links {
		if one != sender {
			deliver(one.heard, from, datagram)
		}
	}

	for _, device := range devices {
		device.asked(from, datagram)
	}
}

// answer delivers a datagram sent to one address and port.
func (l *LAN) answer(from, target netip.AddrPort, datagram string) {
	l.mu.Lock()
	links := append([]*link(nil), l.links...)
	l.mu.Unlock()

	for _, one := range links {
		if one.port == target {
			deliver(one.answered, from, datagram)
		}
	}
}

// Advertise changes what the device advertises, without a word: what it
// says and answers from now on.
func (d *Device) Advertise(adverts ...Advert) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.adverts = adverts
}

// Boot sets the device's count of its starts, which goes with
// everything it says from now on; zero says none.
func (d *Device) Boot(count int) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.boot = count
}

// Mute makes the device answer no search, or answer them again.
func (d *Device) Mute(on bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.mute = on
}

// Announce has the device say that everything it advertises is there.
func (d *Device) Announce() { d.notify("ssdp:alive") }

// Goodbye has the device say that everything it advertises is leaving.
func (d *Device) Goodbye() { d.notify("ssdp:byebye") }

// Say has the device send anything at all to the group.
func (d *Device) Say(datagram string) {
	d.lan.group(netip.AddrPortFrom(d.addr, ssdp.Port), datagram, nil)
}

func (d *Device) notify(kind string) {
	d.mu.Lock()
	adverts := append([]Advert(nil), d.adverts...)
	boot := d.boot
	d.mu.Unlock()

	for _, advert := range adverts {
		said := "NOTIFY * HTTP/1.1\r\nHOST: " + ssdp.Group().String() + "\r\nNT: " + advert.Type + "\r\nNTS: " + kind +
			"\r\nUSN: " + advert.USN + "\r\n"

		if kind == "ssdp:alive" {
			said += advert.lines(boot)
		}

		d.Say(said + "\r\n")
	}
}

// lines is what an advertisement says of itself, in an announcement and
// in an answer alike.
func (a Advert) lines(boot int) string {
	var said strings.Builder

	said.WriteString("LOCATION: " + a.Location + "\r\nSERVER: test/1.0 UPnP/1.1 device/1.0\r\n")

	if a.MaxAge != 0 {
		said.WriteString("CACHE-CONTROL: max-age=" + strconv.Itoa(a.MaxAge) + "\r\n")
	}

	if boot != 0 {
		said.WriteString("BOOTID.UPNP.ORG: " + strconv.Itoa(boot) + "\r\n")
	}

	for name, value := range a.Header {
		said.WriteString(name + ": " + value + "\r\n")
	}

	return said.String()
}

// asked answers a search, as the standard has a device answer: once for
// each thing it advertises that the search asks for.
func (d *Device) asked(from netip.AddrPort, datagram string) {
	if !strings.HasPrefix(datagram, "M-SEARCH * HTTP/1.1\r\n") ||
		!strings.Contains(datagram, "\r\nMAN: \"ssdp:discover\"\r\n") {
		return
	}

	_, rest, found := strings.Cut(datagram, "\r\nST: ")
	target, _, _ := strings.Cut(rest, "\r\n")

	d.mu.Lock()
	adverts := append([]Advert(nil), d.adverts...)
	boot, mute := d.boot, d.mute
	d.mu.Unlock()

	if !found || mute {
		return
	}

	for _, advert := range adverts {
		under, ok := answers(advert.Type, target)
		if !ok {
			continue
		}

		d.lan.answer(netip.AddrPortFrom(d.addr, ssdp.Port), from,
			"HTTP/1.1 200 OK\r\nEXT:\r\nST: "+under+"\r\nUSN: "+advert.USN+"\r\n"+advert.lines(boot)+"\r\n")
	}
}

// answers says whether something advertised answers a search, and under
// what: itself when everything is asked for, and what was asked for
// when that is its type at its version or an earlier one.
func answers(advertised, target string) (string, bool) {
	if target == ssdp.All {
		return advertised, true
	}

	if advertised == target {
		return target, true
	}

	has, hasVersion, hasOK := version(advertised)
	wanted, wantedVersion, wantedOK := version(target)

	return target, hasOK && wantedOK && has == wanted && hasVersion >= wantedVersion
}

func version(urn string) (string, int, bool) {
	cut := strings.LastIndexByte(urn, ':')
	if cut < 0 || !strings.HasPrefix(urn, "urn:") {
		return "", 0, false
	}

	number, err := strconv.Atoi(urn[cut+1:])

	return urn[:cut], number, err == nil
}
