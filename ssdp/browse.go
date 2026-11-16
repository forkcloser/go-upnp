package ssdp

import (
	"context"
	"fmt"
	"maps"
	"net/textproto"
	"slices"
)

// browse is one Browse: what it looks for, and what it has told its
// handler of.
type browse struct {
	target string
	// rouse holds one token when something may have changed.
	rouse chan struct{}
	// told is what the handler knows of, each as it was last told. It is
	// the Browse's own, read and written by its goroutine alone.
	told map[string]Advertisement
}

// stir has the Browse look again.
func (r *browse) stir() {
	select {
	case r.rouse <- struct{}{}:
	default:
	}
}

// telling is one thing a Browse has to tell its handler.
type telling struct {
	change Change
	of     Advertisement
}

// Browse tells handle of what is advertised under a search target:
// first what the Browser already knows of, then what answers the search
// it sends and what devices announce, each as Found; each one that is
// no longer where it was, or whose device started again, as Changed;
// each one that said goodbye, ran out without being said again, or was
// heard on a link that is gone, as Lost, with what it last was. Calls
// come one at a time, on the goroutine that called Browse, for as long
// as ctx lasts.
//
// A type is found at the version asked for or a later one. The search
// is sent on every link, and again when what was found is about to run
// out, so that a device is not lost for its announcements not reaching
// this host.
//
// handle may take its time. A Browse keeps what its handler was told
// and nothing of what happened since: a slow handler delays its own
// Browse and no other, and is told how things stand when it comes back,
// not every step in between.
//
// Browse returns ctx's error when ctx ends, ErrClosed when the Browser
// was closed, and why when the network can be heard no more: when it
// ended, or when the last link the Browser was on broke, which a link
// no search could leave on for a second, three tries running, is taken
// for. A host that was off its network for that long, waking or
// roaming, may lose its Browser so: the program opens another.
func (b *Browser) Browse(ctx context.Context, target string, handle func(Change, Advertisement)) error {
	if target == "" || !sayable(target) {
		return fmt.Errorf("%w: the search target %q", ErrInvalid, target)
	}

	browsing := &browse{target: target, rouse: make(chan struct{}, 1), told: map[string]Advertisement{}}

	b.mu.Lock()

	// One that is over tells nothing: what it kept is of a network it is
	// no longer on.
	if b.closed || b.failed != nil {
		b.mu.Unlock()

		return fmt.Errorf("browsing for %s: %w", target, b.why())
	}

	b.browses[browsing] = struct{}{}
	b.ask(target)
	b.mu.Unlock()

	defer b.leave(browsing)

	for {
		for _, one := range b.look(browsing) {
			handle(one.change, one.of)
		}

		select {
		case <-browsing.rouse:
		case <-ctx.Done():
			return fmt.Errorf("browsing for %s: %w", target, context.Cause(ctx))
		case <-b.over:
			return fmt.Errorf("browsing for %s: %w", target, b.why())
		}
	}
}

// leave ends a Browse: it is stirred no more, and what it browsed for is
// asked after no more on its account.
func (b *Browser) leave(browsing *browse) {
	b.mu.Lock()
	defer b.mu.Unlock()

	delete(b.browses, browsing)
}

// look is what a Browse has to tell of: everything that is kept and
// serves for what it looks for, against what its handler was last told,
// by name.
func (b *Browser) look(browsing *browse) []telling {
	b.mu.Lock()
	defer b.mu.Unlock()

	var tell []telling

	for _, usn := range slices.Sorted(maps.Keys(b.kept)) {
		one := b.kept[usn]
		if one == nil || !serves(one.what, browsing.target) {
			continue
		}

		now := one.advertisement()
		last, known := browsing.told[usn]

		switch {
		case !known:
			tell = append(tell, telling{change: Found, of: handed(now)})
		case !same(last, now):
			tell = append(tell, telling{change: Changed, of: handed(now)})
		}

		browsing.told[usn] = now
	}

	for _, usn := range slices.Sorted(maps.Keys(browsing.told)) {
		if one := b.kept[usn]; one != nil && serves(one.what, browsing.target) {
			continue
		}

		tell = append(tell, telling{change: Lost, of: handed(browsing.told[usn])})

		delete(browsing.told, usn)
	}

	return tell
}

// handed is an advertisement as a handler gets it: its own, to do with
// as it likes. What the Browse compares the next with is not it.
func handed(kept Advertisement) Advertisement {
	kept.Locations = slices.Clone(kept.Locations)

	header := make(textproto.MIMEHeader, len(kept.Header))
	for name, values := range kept.Header {
		header[name] = slices.Clone(values)
	}

	kept.Header = header

	return kept
}

// same says whether an advertisement is where it was and its device has
// not started again: what a handler is told of when it is not.
func same(one, other Advertisement) bool {
	oneBoot, oneCounted := one.Boot()
	otherBoot, otherCounted := other.Boot()

	return oneBoot == otherBoot && oneCounted == otherCounted && slices.Equal(one.Locations, other.Locations)
}
