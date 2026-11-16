// Package ssdp finds UPnP devices on the local network, as a control
// point does: it sends searches to SSDP's group and hears the answers,
// and hears what devices announce by themselves.
//
// A Browser is a host's part in it, on every interface the host has.
// The group's port is one to a host, so a Browser is opened once, by the
// program, and every Browse shares its sockets and what it has heard.
// What a library needs of it, it asks through an interface of its own
// that a Browse satisfies in a few lines.
//
// What is heard is trusted with little: an advertisement is taken only
// when its description is at the address it came from, it holds for as
// long as it says and no longer, and what is kept is bounded.
package ssdp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/textproto"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The search targets every device knows. Any device type, any service
// type and any device's own name, "uuid:" and its UUID, is one too.
const (
	// All is everything every device advertises.
	All = "ssdp:all"
	// RootDevice is every root device, once.
	RootDevice = "upnp:rootdevice"
)

// The errors, to test with errors.Is.
var (
	// ErrInvalid: a search target that is none: empty, or what a message
	// cannot hold.
	ErrInvalid = errors.New("not valid in SSDP")
	// ErrClosed: the Browser is closed.
	ErrClosed = errors.New("closed")
)

// Config is what a Browser is opened with. The zero Config is a Browser
// on every interface of the host.
type Config struct {
	// Interfaces says which interfaces to be on, of those the network
	// has. Nil is all of them.
	Interfaces func(Interface) bool
	// UserAgent is sent with every search when it is not empty: the
	// standard's form is "system/version UPnP/2.0 product/version".
	UserAgent string
	// Network is where the links are. Nil is System, the host's own.
	Network Network
}

// Change is what happened to an advertisement.
type Change uint8

// The changes.
const (
	// Found: the advertisement is there, and was not.
	Found Change = iota + 1
	// Changed: where it is, or the device's boot, is no longer what it
	// was.
	Changed
	// Lost: the device said goodbye, the advertisement ran out without
	// being said again, or the link it was heard on is gone.
	Lost
)

func (c Change) String() string {
	switch c {
	case Found:
		return "found"
	case Changed:
		return "changed"
	case Lost:
		return "lost"
	default:
		return "Change(" + strconv.Itoa(int(c)) + ")"
	}
}

// Advertisement is one thing a device advertises: itself as a root
// device, itself by its name, a device type or a service type.
type Advertisement struct {
	// USN is the advertisement's name, which tells it from every other.
	USN string
	// UDN is the device it is of: "uuid:" and a UUID, the same wherever
	// the device is.
	UDN string
	// Type is what it advertises: RootDevice, the UDN, or a device or
	// service type.
	Type string
	// Server is the device's word on its system and its product.
	Server string
	// Locations is every place it was heard to be, by interface and
	// then by URL: a device heard on two interfaces is told once, with
	// both. A lost advertisement has those it had last.
	Locations []Location
	// Header is the headers of the last message heard of it, for what a
	// maker adds. Both are the handler's own copies.
	Header textproto.MIMEHeader
}

// Boot is the device's count of its starts, BOOTID.UPNP.ORG, when it
// gives one. A change says the device started again, and that every
// subscription to its events is gone.
func (a Advertisement) Boot() (uint32, bool) {
	count, err := strconv.ParseUint(strings.TrimSpace(a.Header.Get("Bootid.upnp.org")), 10, 31)
	if err != nil {
		return 0, false
	}

	return uint32(count), true
}

// Location is one place an advertisement was heard to be.
type Location struct {
	// URL is the description of the root device. It is an http or https
	// URL at the address the message came from: none other is taken.
	URL string
	// Interface is the interface it was heard on.
	Interface Interface
	// Local is this host's address on that interface, on the device's
	// network: what the device can call back. It is not valid when the
	// device is on no network of the interface's.
	Local netip.Addr
}

// What a Browser keeps, and when it asks.
const (
	// maxKept is how many advertisements a Browser keeps. Past it what
	// is new is not taken until something ran out.
	maxKept = 4096
	// maxPlaces is how many places one advertisement may be at.
	maxPlaces = 8
	// again is how soon the same search may be sent again.
	again = time.Second
)

// sends is when a search is sent, from the moment it is asked for: more
// than once, since a datagram may be lost.
func sends() []time.Duration {
	return []time.Duration{0, 400 * time.Millisecond, 1200 * time.Millisecond}
}

// Browser is a host's part in SSDP as a control point. Its methods may
// be called from several goroutines.
type Browser struct {
	userAgent string
	// life ends when the Browser closes, and running is what it started
	// that ends with it.
	life    context.Context //nolint:containedctx // what outlives a call: the links, read for as long as the Browser is open
	end     context.CancelFunc
	running sync.WaitGroup
	// stir holds one token when the schedule has to be looked at again.
	stir chan struct{}
	// over is closed when the Browser is closed or its network failed.
	over chan struct{}

	mu sync.Mutex
	// failed is why the network can be heard no more.
	failed error
	closed bool
	// links is the links the Browser is on, each with how long no
	// search has left on it.
	links   map[Link]*silence
	kept    map[string]*kept
	browses map[*browse]struct{}
	// due is the searches to send, and asked when each target was last
	// asked for.
	due   []sending
	asked map[string]time.Time
}

// silence is how long no search has left on a link: how many times in
// a row one was due and could not be sent, since when, and when last.
type silence struct {
	failures int
	since    time.Time
	last     time.Time
}

// stale is how long after a search could not be sent the next failure
// is no longer of the same silence: longer than the sends of one search
// are apart.
const stale = 2 * time.Second

// kept is one advertisement as the Browser holds it.
type kept struct {
	usn    string
	what   string
	server string
	header textproto.MIMEHeader
	places map[place]*heard
}

// place is where an advertisement was heard to be.
type place struct {
	link Link
	url  string
}

// heard is when a place was last said, by whom, and what follows from
// it.
type heard struct {
	from  netip.Addr
	local netip.Addr
	// ends is when it runs out, and refresh when to ask after it.
	ends    time.Time
	refresh time.Time
}

// sending is one search to send.
type sending struct {
	at     time.Time
	target string
}

// Open makes a Browser and puts it on the network: when it returns, it
// is on every link there is, and every other method works. ctx bounds
// the opening alone; the Browser lasts until Close. A host with no
// usable interface is no error: links are taken up as they come.
func Open(ctx context.Context, cfg Config) (*Browser, error) {
	if !sayable(cfg.UserAgent) {
		return nil, fmt.Errorf("%w: the user agent %q", ErrInvalid, cfg.UserAgent)
	}

	network := cfg.Network
	if network == nil {
		network = System()
	}

	browser := &Browser{
		userAgent: cfg.UserAgent,
		stir:      make(chan struct{}, 1),
		over:      make(chan struct{}),
		links:     map[Link]*silence{},
		kept:      map[string]*kept{},
		browses:   map[*browse]struct{}{},
		asked:     map[string]time.Time{},
	}
	browser.life, browser.end = context.WithCancel(context.WithoutCancel(ctx))

	settled := make(chan struct{})

	browser.running.Go(func() {
		var once sync.Once

		err := network.Watch(browser.life, func(event LinkEvent) {
			switch event.Change {
			case Up:
				browser.up(event.Link, cfg.Interfaces)
			case Down:
				browser.down(event.Link)
			case Settled:
				once.Do(func() { close(settled) })
			}
		})

		browser.fail(err)
	})

	browser.running.Go(browser.schedule)

	select {
	case <-settled:
		return browser, nil
	case <-browser.over:
		why := browser.why()

		return nil, fmt.Errorf("opening the network: %w", errors.Join(why, browser.Close()))
	case <-ctx.Done():
		_ = browser.Close()

		return nil, fmt.Errorf("opening the network: %w", context.Cause(ctx))
	}
}

// Close takes the Browser off the network. Every Browse returns
// ErrClosed. It closes the links it is on: a network closes its own
// when its Watch ends, and one that does not would leave their readers
// waiting for good.
func (b *Browser) Close() error {
	b.mu.Lock()

	if !b.closed {
		b.closed = true

		if b.failed == nil {
			close(b.over)
		}
	}

	links := make([]Link, 0, len(b.links))
	for link := range b.links {
		links = append(links, link)
	}

	b.links = map[Link]*silence{}
	b.mu.Unlock()

	b.end()

	var failed error

	for _, link := range links {
		failed = errors.Join(failed, link.Close())
	}

	b.running.Wait()

	if failed != nil {
		return fmt.Errorf("closing the links: %w", failed)
	}

	return nil
}

// errSilent is a network whose Watch returned with nothing to say.
var errSilent = errors.New("the network told of its links no more")

// why is what ended the Browser.
func (b *Browser) why() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed || b.failed == nil {
		return ErrClosed
	}

	return b.failed
}

// fail records that the network can be heard no more, unless the
// Browser was closed, which is why it then stopped.
func (b *Browser) fail(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.lost(err)
}

// lost is fail, with the lock held.
func (b *Browser) lost(err error) {
	if b.closed || b.failed != nil {
		return
	}

	if err == nil {
		err = errSilent
	}

	b.failed = fmt.Errorf("the network is lost: %w", err)

	close(b.over)
}

// up takes a link up: it is read from here on, and what is being
// browsed for is asked for on it.
func (b *Browser) up(link Link, wanted func(Interface) bool) {
	// One that is not wanted is given back at once: it holds a place in
	// the group for nothing.
	if wanted != nil && !wanted(link.Interface()) {
		_ = link.Close()

		return
	}

	b.mu.Lock()

	if b.closed {
		b.mu.Unlock()

		return
	}

	b.links[link] = &silence{}

	for browsing := range b.browses {
		b.ask(browsing.target)
	}

	b.mu.Unlock()

	b.running.Go(func() { b.read(link, link.Heard) })
	b.running.Go(func() { b.read(link, link.Answered) })
}

// down gives a link up, as the network said: what was heard on it alone
// is lost.
func (b *Browser) down(link Link) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.drop(link)
}

// broken gives up a link that can no longer be read, or searched on.
// When it was the last, the Browser can find nothing any more, and says
// why: silence would read as a network with no device on it.
func (b *Browser) broken(link Link, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.drop(link) {
		return
	}

	_ = link.Close()

	if len(b.links) == 0 {
		b.lost(fmt.Errorf("no link is left: %w", err))
	}
}

// drop forgets a link and what was heard on it, and says whether the
// Browser was on it. Called with the lock held.
func (b *Browser) drop(link Link) bool {
	if _, held := b.links[link]; !held {
		return false
	}

	delete(b.links, link)

	for usn, one := range b.kept {
		changed := false

		for where := range one.places {
			if where.link == link {
				delete(one.places, where)

				changed = true
			}
		}

		if changed {
			b.settle(usn, one)
		}
	}

	return true
}

// read takes what comes on one of a link's two ways in, until the link
// is closed.
func (b *Browser) read(link Link, next func([]byte) (int, netip.AddrPort, error)) {
	buffer := make([]byte, MaxMessage)

	for {
		count, from, err := next(buffer)
		if err != nil {
			// A link returns an error when it can be read no more: when
			// it was closed, which is the network's doing and no loss of
			// the Browser's, or when it is broken.
			if errors.Is(err, net.ErrClosed) {
				b.down(link)
			} else {
				b.broken(link, err)
			}

			return
		}

		if said, ok := read(buffer[:count], from.Addr()); ok {
			b.take(link, from.Addr(), said)
		}
	}
}

// take records what a device said.
func (b *Browser) take(link Link, from netip.Addr, said message) {
	now := time.Now()

	b.mu.Lock()
	defer b.mu.Unlock()

	if _, held := b.links[link]; !held {
		return
	}

	one := b.kept[said.usn]

	if said.kind == goodbye {
		if one == nil {
			return
		}

		// A goodbye takes the places it was said from: an announcement is
		// held to the address it came from, and so is its end.
		for where, last := range one.places {
			if where.link == link && last.from == from {
				delete(one.places, where)
			}
		}

		b.settle(said.usn, one)

		return
	}

	where := place{link: link, url: said.location}

	if one == nil {
		if len(b.kept) >= maxKept {
			return
		}

		one = &kept{usn: said.usn, places: map[place]*heard{}}
		b.kept[said.usn] = one
	}

	if one.places[where] == nil && len(one.places) >= maxPlaces {
		return
	}

	// Asked after at three quarters of its time, so that a device whose
	// announcements do not reach this host is not lost for that.
	one.places[where] = &heard{
		from: from, local: local(link, from), ends: now.Add(said.lasts), refresh: now.Add(said.lasts / 4 * 3),
	}
	one.what, one.server, one.header = advertised(said), said.server, said.header

	b.settle(said.usn, one)
}

// advertised is what a message advertises. Its name says it, after the
// device's: an answer to a search names the type that was asked for,
// which for a device of a later version is not the one it has.
func advertised(said message) string {
	device, what, typed := strings.Cut(said.usn, "::")

	switch {
	case typed && what != "":
		return what
	case strings.HasPrefix(strings.ToLower(device), "uuid:"):
		return device
	default:
		return said.what
	}
}

// local is this host's address on a link, on the network a device is
// on.
func local(link Link, device netip.Addr) netip.Addr {
	for _, prefix := range link.Addrs() {
		if prefix.Contains(device.Unmap()) {
			return prefix.Addr()
		}
	}

	return netip.Addr{}
}

// settle forgets an advertisement that is nowhere any more, tells every
// Browse that it may have changed, and has the schedule looked at
// again. Called with the lock held.
func (b *Browser) settle(usn string, one *kept) {
	if len(one.places) == 0 {
		delete(b.kept, usn)
	}

	for browsing := range b.browses {
		browsing.stir()
	}

	b.wake()
}

// wake has the schedule looked at again.
func (b *Browser) wake() {
	select {
	case b.stir <- struct{}{}:
	default:
	}
}

// ask has a search for a target sent, now and again shortly after,
// unless it was asked for a moment ago: the sends of that one are not
// over, and reach whatever link has come up since. Called with the lock
// held.
func (b *Browser) ask(target string) {
	now := time.Now()

	if last, was := b.asked[target]; was && now.Sub(last) < again {
		return
	}

	// What was asked longer ago than that says nothing any more.
	for other, last := range b.asked {
		if now.Sub(last) >= again {
			delete(b.asked, other)
		}
	}

	b.asked[target] = now

	for _, after := range sends() {
		b.due = append(b.due, sending{at: now.Add(after), target: target})
	}

	b.wake()
}

// schedule sends the searches that are due, drops what ran out, and asks
// after what is about to, for as long as the Browser is open.
func (b *Browser) schedule() {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()

	for {
		next := b.turn(time.Now())

		timer.Reset(max(time.Until(next), time.Millisecond))

		select {
		case <-timer.C:
		case <-b.stir:
		case <-b.life.Done():
			return
		}
	}
}

// turn does what is due at a moment, and returns when to look again.
func (b *Browser) turn(now time.Time) time.Time {
	b.mu.Lock()

	next := now.Add(time.Hour)
	targets := b.searches(now, &next)

	if b.age(now, &next) {
		for browsing := range b.browses {
			b.ask(browsing.target)
		}
	}

	links := make([]Link, 0, len(b.links))
	for link := range b.links {
		links = append(links, link)
	}

	userAgent := b.userAgent
	b.mu.Unlock()

	for _, link := range links {
		var (
			left   bool
			failed error
		)

		for _, target := range targets {
			if err := link.Search(search(target, userAgent)); err != nil {
				failed = err
			} else {
				left = true
			}
		}

		if left || failed != nil {
			b.sent(link, left, failed)
		}
	}

	return next
}

// sent notes what became of the searches that were due on a link at
// one moment. One that leaves ends the silence, whatever became of the
// others due with it. A link is given up when none has left at as many
// moments in a row as a search has sends, over a second or more: a
// search that did not leave at all, and not several that were due
// together and met one bad moment. A failure long after the last one is
// the first of another silence.
func (b *Browser) sent(link Link, left bool, failed error) {
	now := time.Now()

	b.mu.Lock()

	quiet := b.links[link]
	if quiet == nil {
		b.mu.Unlock()

		return
	}

	switch {
	case left:
		*quiet = silence{}
	case quiet.failures == 0 || now.Sub(quiet.last) > stale:
		*quiet = silence{failures: 1, since: now, last: now}
	default:
		quiet.failures++
		quiet.last = now
	}

	lost := quiet.failures >= len(sends()) && now.Sub(quiet.since) >= again

	b.mu.Unlock()

	if lost {
		b.broken(link, failed)
	}
}

// searches is the targets of the searches that are due, each once; next
// is brought forward to the first of those that are not. Called with
// the lock held.
func (b *Browser) searches(now time.Time, next *time.Time) []string {
	var targets []string

	later := b.due[:0]

	for _, one := range b.due {
		if one.at.After(now) {
			later = append(later, one)
			*next = earliest(*next, one.at)

			continue
		}

		if !slices.Contains(targets, one.target) {
			targets = append(targets, one.target)
		}
	}

	b.due = later

	return targets
}

// age drops the places that ran out, and says whether one is about to:
// what it is then asked after once, and set anew by what answers. next
// is brought forward to the first moment something is due. Called with
// the lock held.
func (b *Browser) age(now time.Time, next *time.Time) bool {
	stale := false

	for usn, one := range b.kept {
		changed := false

		for where, last := range one.places {
			if !last.ends.After(now) {
				delete(one.places, where)

				changed = true

				continue
			}

			if !last.refresh.After(now) {
				last.refresh = last.ends
				stale = true
			}

			*next = earliest(*next, last.ends, last.refresh)
		}

		if changed {
			b.settle(usn, one)
		}
	}

	return stale
}

// earliest is the first of a moment and some others.
func earliest(first time.Time, others ...time.Time) time.Time {
	for _, moment := range others {
		if moment.Before(first) {
			first = moment
		}
	}

	return first
}

// advertisement is what a Browse is told of an advertisement that is
// kept.
func (k *kept) advertisement() Advertisement {
	told := Advertisement{
		USN: k.usn, Type: k.what, Server: k.server, Header: k.header,
		Locations: make([]Location, 0, len(k.places)),
	}

	told.UDN, _, _ = strings.Cut(k.usn, "::")

	for where, last := range k.places {
		told.Locations = append(
			told.Locations,
			Location{URL: where.url, Interface: where.link.Interface(), Local: last.local},
		)
	}

	slices.SortFunc(told.Locations, func(one, other Location) int {
		if one.Interface.Index != other.Interface.Index {
			return one.Interface.Index - other.Interface.Index
		}

		return strings.Compare(one.URL, other.URL)
	})

	return told
}
