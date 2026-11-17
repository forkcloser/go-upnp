package ssdp_test

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/forkcloser/go-upnp/ssdp"
	"github.com/forkcloser/go-upnp/ssdp/ssdptest"
)

// wait bounds every wait of a test; generous, for a loaded runner. quiet
// is how long nothing has to happen for it to count as nothing.
const (
	wait  = 30 * time.Second
	quiet = 300 * time.Millisecond
)

// What the tests' network is made of.
const (
	lightUDN  = "uuid:00000000-0000-4000-8000-000000000001"
	lightType = "urn:schemas-upnp-org:device:BinaryLight:1"
	lightAt   = "http://192.0.2.20:49152/description.xml"
)

func address(text string) netip.Addr  { return netip.MustParseAddr(text) }
func prefix(text string) netip.Prefix { return netip.MustParsePrefix(text) }

// light is what a light advertises: itself as a root device, by its
// name, and by its type.
func light(age int) []ssdptest.Advert {
	return []ssdptest.Advert{
		{USN: lightUDN + "::" + ssdp.RootDevice, Type: ssdp.RootDevice, Location: lightAt, MaxAge: age},
		{USN: lightUDN, Type: lightUDN, Location: lightAt, MaxAge: age},
		{
			USN: lightUDN + "::" + lightType, Type: lightType, Location: lightAt, MaxAge: age,
			Header: map[string]string{"X-MAKER": "a maker"},
		},
	}
}

// network is a LAN with one host on it, at 192.0.2.10 on en0, and a
// Browser open on that host.
func network(t *testing.T, cfg ssdp.Config) (*ssdptest.LAN, *ssdptest.Host, *ssdp.Browser) {
	t.Helper()

	lan, host := ssdptest.NewLAN(), ssdptest.NewHost()
	host.Plug(lan, "en0", 1, prefix("192.0.2.10/24"))

	cfg.Network = host

	browser, err := ssdp.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = browser.Close() })

	return lan, host, browser
}

// told is one call of a Browse's handler.
type told struct {
	change ssdp.Change
	of     ssdp.Advertisement
}

// browsing starts a Browse and returns what it tells, what it ends
// with, and how to end it.
func browsing(t *testing.T, browser *ssdp.Browser, target string) (chan told, chan error, context.CancelFunc) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	calls := make(chan told, 8192)
	done := make(chan error, 1)

	go func() {
		done <- browser.Browse(ctx, target, func(change ssdp.Change, of ssdp.Advertisement) {
			calls <- told{change: change, of: of}
		})
	}()

	t.Cleanup(cancel)

	return calls, done, cancel
}

func next(t *testing.T, calls chan told, done chan error) told {
	t.Helper()

	select {
	case call := <-calls:
		return call
	case err := <-done:
		t.Fatalf("the Browse ended: %v", err)
	case <-time.After(wait):
		t.Fatal("nothing was told")
	}

	return told{}
}

func nothing(t *testing.T, calls chan told) {
	t.Helper()

	select {
	case call := <-calls:
		t.Fatalf("told %s of %+v, want nothing", call.change, call.of)
	case <-time.After(quiet):
	}
}

func ended(t *testing.T, done chan error) error {
	t.Helper()

	select {
	case err := <-done:
		return err
	case <-time.After(wait):
		t.Fatal("the Browse did not end")
	}

	return nil
}

// here is where the light is heard by the tests' host.
func here() []ssdp.Location {
	return []ssdp.Location{
		{URL: lightAt, Interface: ssdp.Interface{Index: 1, Name: "en0"}, Local: address("192.0.2.10")},
	}
}

// TestBrowse: a search is sent as the standard writes it, more than
// once, and what answers is told once, with what it is and where.
func TestBrowse(t *testing.T) {
	t.Parallel()

	lan, _, browser := network(t, ssdp.Config{UserAgent: "test/1 UPnP/2.0 browser/1"})
	device := lan.Device(address("192.0.2.20"), light(1800)...)

	calls, done, _ := browsing(t, browser, lightType)

	found := next(t, calls, done)
	if found.change != ssdp.Found || found.of.USN != lightUDN+"::"+lightType || found.of.UDN != lightUDN ||
		found.of.Type != lightType || found.of.Server != "test/1.0 UPnP/1.1 device/1.0" ||
		!slices.Equal(found.of.Locations, here()) || found.of.Header.Get("X-Maker") != "a maker" {
		t.Fatalf("told %s of %+v", found.change, found.of)
	}

	if _, counted := found.of.Boot(); counted {
		t.Error("a device that gave no count of its starts has one")
	}

	// Answered three times, told once.
	nothing(t, calls)

	const want = "M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nMAN: \"ssdp:discover\"\r\nMX: 2\r\n" +
		"ST: " + lightType + "\r\nUSER-AGENT: test/1 UPnP/2.0 browser/1\r\n\r\n"

	for deadline := time.Now().Add(wait); len(lan.Searches()) < 3; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the search was sent %d times, want 3", len(lan.Searches()))
		}
	}

	if sent := lan.Searches(); !slices.Equal(sent, []string{want, want, want}) {
		t.Errorf("the searches sent = %q", sent)
	}

	// The handler's own: changing it changes no other's. The device says
	// no more, so that what the Browser keeps is what it kept.
	device.Mute(true)

	// Written in place, and set: neither reaches what the Browser keeps.
	again, againDone, _ := browsing(t, browser, lightType)
	header := next(t, again, againDone).of.Header
	header.Set("Server", "changed")

	if values := header["X-Maker"]; len(values) == 1 {
		values[0] = "changed"
	} else {
		t.Fatalf("the maker's header = %v", values)
	}

	last, lastDone, _ := browsing(t, browser, lightType)
	if known := next(
		t,
		last,
		lastDone,
	); known.of.Header.Get("X-Maker") != "a maker" ||
		known.of.Header.Get("Server") == "changed" {
		t.Errorf("a Browse was told a header another one's handler changed: %v", known.of.Header)
	}
}

// TestOneSearchForSeveral: several Browses for the same thing, begun
// together, share one search.
func TestOneSearchForSeveral(t *testing.T) {
	t.Parallel()

	lan, _, browser := network(t, ssdp.Config{})

	for range 4 {
		browsing(t, browser, ssdp.RootDevice)
	}

	browsing(t, browser, ssdp.All)

	// Long enough for every send of a search, and of one too many.
	time.Sleep(2 * time.Second)

	roots, all := 0, 0

	for _, sent := range lan.Searches() {
		switch {
		case strings.Contains(sent, "ST: "+ssdp.RootDevice+"\r\n"):
			roots++
		case strings.Contains(sent, "ST: "+ssdp.All+"\r\n"):
			all++
		}
	}

	if roots != 3 || all != 3 {
		t.Errorf("four Browses for one thing and one for another sent %d and %d searches, want 3 and 3", roots, all)
	}
}

// TestWhatASearchFinds: everything, the root devices, a device by its
// name, a type at the version asked for or a later one.
func TestWhatASearchFinds(t *testing.T) {
	t.Parallel()

	lan, _, browser := network(t, ssdp.Config{})

	newer := strings.TrimSuffix(lightType, "1") + "2"
	lan.Device(address("192.0.2.20"), ssdptest.Advert{USN: lightUDN + "::" + newer, Type: newer, Location: lightAt},
		light(0)[0], light(0)[1])

	for target, want := range map[string][]string{
		ssdp.All:                                 {lightUDN, lightUDN + "::" + ssdp.RootDevice, lightUDN + "::" + newer},
		ssdp.RootDevice:                          {lightUDN + "::" + ssdp.RootDevice},
		lightUDN:                                 {lightUDN},
		lightType:                                {lightUDN + "::" + newer},
		newer:                                    {lightUDN + "::" + newer},
		strings.TrimSuffix(lightType, "1") + "3": nil,
		"urn:schemas-upnp-org:device:Other:1":    nil,
		"uuid:00000000-0000-4000-8000-00000000000f": nil,
	} {
		calls, done, cancel := browsing(t, browser, target)

		var got []string

		for range want {
			call := next(t, calls, done)
			if call.change != ssdp.Found {
				t.Errorf("browsing for %s told %s of %s", target, call.change, call.of.USN)
			}

			got = append(got, call.of.USN)
		}

		nothing(t, calls)
		cancel()

		slices.Sort(got)

		if !slices.Equal(got, want) {
			t.Errorf("browsing for %s found %v, want %v", target, got, want)
		}
	}

	// What is known is found under a type in another case too, though a
	// device answers no search for one.
	calls, done, _ := browsing(t, browser, strings.ToUpper(lightType))
	if known := next(t, calls, done); known.of.USN != lightUDN+"::"+newer {
		t.Errorf("browsing for a type in capitals found %s", known.of.USN)
	}
}

// TestAnnouncements: a device that answers no search is found when it
// announces itself, and lost when it says goodbye, with what it was.
func TestAnnouncements(t *testing.T) {
	t.Parallel()

	lan, _, browser := network(t, ssdp.Config{})
	device := lan.Device(address("192.0.2.20"), light(1800)...)
	device.Mute(true)

	calls, done, _ := browsing(t, browser, ssdp.RootDevice)

	nothing(t, calls)
	device.Announce()

	if found := next(t, calls, done); found.change != ssdp.Found || !slices.Equal(found.of.Locations, here()) {
		t.Fatalf("told %s of %+v", found.change, found.of)
	}

	// Said again, it is the same.
	device.Announce()
	nothing(t, calls)

	device.Goodbye()

	lost := next(t, calls, done)
	if lost.change != ssdp.Lost || lost.of.USN != lightUDN+"::"+ssdp.RootDevice ||
		!slices.Equal(lost.of.Locations, here()) {
		t.Fatalf("told %s of %+v", lost.change, lost.of)
	}

	// A goodbye of what is not known is nothing.
	device.Goodbye()
	nothing(t, calls)

	// What is already known is told at once, to a Browse that comes late.
	device.Announce()
	next(t, calls, done)

	late, lateDone, _ := browsing(t, browser, ssdp.RootDevice)
	if known := next(t, late, lateDone); known.change != ssdp.Found {
		t.Fatalf("a late Browse was told %s", known.change)
	}
}

// TestRunningOut: an advertisement holds for as long as it says. One
// about to run out is asked after, and stays when its device answers;
// one whose device answers no more is lost.
func TestRunningOut(t *testing.T) {
	t.Parallel()

	lan, _, browser := network(t, ssdp.Config{})
	device := lan.Device(address("192.0.2.20"), light(1)...)

	calls, done, _ := browsing(t, browser, ssdp.RootDevice)
	next(t, calls, done)

	// Three times its length, and still there: asked after, and answered.
	select {
	case call := <-calls:
		t.Fatalf("told %s of a device that answers", call.change)
	case <-time.After(3 * time.Second):
	}

	if sent := len(lan.Searches()); sent < 6 {
		t.Errorf("%d searches were sent over three lengths, want it asked after each time", sent)
	}

	device.Mute(true)

	began := time.Now()

	if lost := next(t, calls, done); lost.change != ssdp.Lost || time.Since(began) > 2*time.Second {
		t.Fatalf("told %s after %s", lost.change, time.Since(began))
	}
}

// TestChanged: a device that is somewhere else, or that started again,
// is told as changed.
func TestChanged(t *testing.T) {
	t.Parallel()

	lan, _, browser := network(t, ssdp.Config{})
	device := lan.Device(address("192.0.2.20"), light(2)[0])
	device.Boot(7)

	calls, done, _ := browsing(t, browser, ssdp.RootDevice)

	found := next(t, calls, done)
	if boot, counted := found.of.Boot(); found.change != ssdp.Found || !counted || boot != 7 {
		t.Fatalf("told %s with the boot %d, %t", found.change, boot, counted)
	}

	device.Boot(8)
	device.Announce()

	started := next(t, calls, done)
	if boot, _ := started.of.Boot(); started.change != ssdp.Changed || boot != 8 ||
		!slices.Equal(started.of.Locations, here()) {
		t.Fatalf("told %s with the boot %d of %+v", started.change, boot, started.of)
	}

	// At another port: there too, until the first runs out.
	const moved = "http://192.0.2.20:49153/description.xml"

	device.Mute(true)
	device.Advertise(
		ssdptest.Advert{USN: lightUDN + "::" + ssdp.RootDevice, Type: ssdp.RootDevice, Location: moved, MaxAge: 1800},
	)
	device.Announce()

	both := next(t, calls, done)
	if both.change != ssdp.Changed || len(both.of.Locations) != 2 || both.of.Locations[0].URL != lightAt ||
		both.of.Locations[1].URL != moved {
		t.Fatalf("told %s of %+v", both.change, both.of)
	}

	one := next(t, calls, done)
	if one.change != ssdp.Changed || len(one.of.Locations) != 1 || one.of.Locations[0].URL != moved {
		t.Fatalf("told %s of %+v", one.change, one.of)
	}
}

// TestSeveralInterfaces: a device heard on two interfaces is told once,
// with both and this host's address on each; an interface that goes
// takes what was heard on it alone.
func TestSeveralInterfaces(t *testing.T) {
	t.Parallel()

	lan, host, browser := network(t, ssdp.Config{})
	device := lan.Device(address("192.0.2.20"), light(1800)[0])
	device.Mute(true)

	calls, done, _ := browsing(t, browser, ssdp.RootDevice)

	host.Plug(lan, "en1", 2, prefix("192.0.2.11/24"))
	device.Announce()

	// One call or two, as the two links are read: the last has both.
	last := next(t, calls, done)
	if len(last.of.Locations) == 1 {
		last = next(t, calls, done)
	}

	want := []ssdp.Location{
		{URL: lightAt, Interface: ssdp.Interface{Index: 1, Name: "en0"}, Local: address("192.0.2.10")},
		{URL: lightAt, Interface: ssdp.Interface{Index: 2, Name: "en1"}, Local: address("192.0.2.11")},
	}
	if !slices.Equal(last.of.Locations, want) {
		t.Fatalf("heard on two interfaces: %+v", last.of.Locations)
	}

	host.Unplug("en0")

	if one := next(t, calls, done); one.change != ssdp.Changed || !slices.Equal(one.of.Locations, want[1:]) {
		t.Fatalf("with one interface gone: %s of %+v", one.change, one.of.Locations)
	}

	host.Unplug("en1")

	if lost := next(t, calls, done); lost.change != ssdp.Lost || !slices.Equal(lost.of.Locations, want[1:]) {
		t.Fatalf("with both gone: %s of %+v", lost.change, lost.of.Locations)
	}
}

// TestALinkThatComesUp: what is being browsed for is searched for on a
// link that comes up, long after the Browse began; and a device on no
// network of the link's has no address of this host's to call back.
func TestALinkThatComesUp(t *testing.T) {
	t.Parallel()

	lan, host, browser := network(t, ssdp.Config{})
	calls, done, _ := browsing(t, browser, ssdp.RootDevice)

	// The searches the Browse began with are all sent.
	for deadline := time.Now().Add(wait); len(lan.Searches()) < 3; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the search was sent %d times, want 3", len(lan.Searches()))
		}
	}

	other := ssdptest.NewLAN()
	other.Device(address("192.0.2.20"), light(1800)[0])

	host.Plug(other, "en1", 2, prefix("198.51.100.1/24"))

	found := next(t, calls, done)
	if found.change != ssdp.Found || len(found.of.Locations) != 1 || found.of.Locations[0].Local.IsValid() ||
		found.of.Locations[0].Interface.Name != "en1" || len(other.Searches()) == 0 {
		t.Fatalf("on a link that came up: %s of %+v", found.change, found.of.Locations)
	}
}

// TestWhichInterfaces: a Browser is on the interfaces it is told to be
// on, and no other.
func TestWhichInterfaces(t *testing.T) {
	t.Parallel()

	lan, host := ssdptest.NewLAN(), ssdptest.NewHost()
	host.Plug(lan, "en0", 1, prefix("192.0.2.10/24"))
	host.Plug(lan, "en1", 2, prefix("192.0.2.11/24"))

	browser, err := ssdp.Open(t.Context(), ssdp.Config{
		Network: host, Interfaces: func(one ssdp.Interface) bool { return one.Name == "en1" },
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = browser.Close() })

	lan.Device(address("192.0.2.20"), light(1800)[0])

	calls, done, _ := browsing(t, browser, ssdp.RootDevice)

	found := next(t, calls, done)
	if len(found.of.Locations) != 1 || found.of.Locations[0].Interface.Name != "en1" {
		t.Fatalf("found on %+v", found.of.Locations)
	}

	nothing(t, calls)

	// The one it is not on is given back, and the one it is on is not.
	if host.Open("en0") || !host.Open("en1") {
		t.Errorf("en0 open: %t, en1 open: %t", host.Open("en0"), host.Open("en1"))
	}
}

// TestWhatIsNotTaken: what is no advertisement, and an advertisement
// whose description is not where it came from, is not told.
func TestWhatIsNotTaken(t *testing.T) {
	t.Parallel()

	lan, _, browser := network(t, ssdp.Config{})
	device := lan.Device(address("192.0.2.20"))

	calls, done, _ := browsing(t, browser, ssdp.All)

	alive := func(location, more string) string {
		return "NOTIFY * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nNT: upnp:rootdevice\r\nNTS: ssdp:alive\r\n" +
			"USN: uuid:1::upnp:rootdevice\r\nLOCATION: " + location + "\r\n" + more + "\r\n"
	}

	for name, said := range map[string]string{
		"a description at another address": alive("http://192.0.2.99/d.xml", ""),
		"a description at a name":          alive("http://device.example/d.xml", ""),
		"a description that is no http":    alive("file:///etc/passwd", ""),
		"one at the device, and no http":   alive("ftp://192.0.2.20/d.xml", ""),
		"a description that is no URL":     alive("http://[::1", ""),
		"no description":                   alive("", ""),
		"another control point's search":   "M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nMAN: \"ssdp:discover\"\r\nMX: 1\r\nST: ssdp:all\r\n\r\n",
		"an answer that is no success":     "HTTP/1.1 500 No\r\nST: upnp:rootdevice\r\nUSN: uuid:1\r\nLOCATION: http://192.0.2.20/d.xml\r\n\r\n",
		"an announcement of nothing known": strings.Replace(alive("http://192.0.2.20/d.xml", ""), "ssdp:alive", "ssdp:other", 1),
		"no name":                          strings.Replace(alive("http://192.0.2.20/d.xml", ""), "USN: uuid:1::upnp:rootdevice\r\n", "", 1),
		"no type":                          strings.Replace(alive("http://192.0.2.20/d.xml", ""), "NT: upnp:rootdevice\r\n", "", 1),
		"a name too long":                  strings.Replace(alive("http://192.0.2.20/d.xml", ""), "uuid:1::", "uuid:"+strings.Repeat("1", 600)+"::", 1),
		"no headers":                       "NOTIFY * HTTP/1.1\r\n\r\n",
		"one word":                         "NOTIFY\r\n",
		"no line":                          "NOTIFY * HTTP/1.1",
		"nothing":                          "",
		"not text":                         "\x00\x01\x02\xff",
	} {
		device.Say(said)

		select {
		case call := <-calls:
			t.Errorf("%s: told %s of %+v", name, call.change, call.of)
		case <-time.After(quiet / 10):
		}
	}

	// As devices write them: any case, no last empty line, a mapped address.
	for name, said := range map[string]string{
		"names in any case":   "notify * http/1.1\r\nnt: upnp:rootdevice\r\nnts: SSDP:ALIVE\r\nusn: uuid:a\r\nlocation: http://192.0.2.20/d.xml\r\n\r\n",
		"no last empty line":  "NOTIFY * HTTP/1.1\r\nNT: upnp:rootdevice\r\nNTS: ssdp:alive\r\nUSN: uuid:b\r\nLOCATION: http://192.0.2.20/d.xml\r\n",
		"an answer under 1.0": "HTTP/1.0 200 OK\r\nST: upnp:rootdevice\r\nUSN: uuid:c\r\nLOCATION: https://192.0.2.20/d.xml\r\n\r\n",
		"an update":           strings.Replace(alive("http://192.0.2.20/d.xml", ""), "ssdp:alive", "ssdp:update", 1),
	} {
		device.Say(said)

		if found := next(t, calls, done); found.change != ssdp.Found {
			t.Errorf("%s: told %s", name, found.change)
		}
	}
}

// TestHowLongAnAdvertisementHolds: for what it says, within bounds, and
// half an hour when it says nothing that can be read.
func TestHowLongAnAdvertisementHolds(t *testing.T) {
	t.Parallel()

	lan, _, browser := network(t, ssdp.Config{})
	device := lan.Device(address("192.0.2.20"))
	device.Mute(true)

	calls, done, _ := browsing(t, browser, ssdp.All)

	alive := func(usn, control string) string {
		return "NOTIFY * HTTP/1.1\r\nNT: upnp:rootdevice\r\nNTS: ssdp:alive\r\nUSN: " + usn +
			"\r\nLOCATION: http://192.0.2.20/d.xml\r\n" + control + "\r\n"
	}

	// A second at least, whatever it says: these two run out together.
	device.Say(alive("uuid:zero", "CACHE-CONTROL: max-age=0\r\n"))
	device.Say(alive("uuid:one", "Cache-Control: no-cache, MAX-AGE = 1 \r\n"))
	// And these hold.
	device.Say(alive("uuid:none", ""))
	device.Say(alive("uuid:odd", "CACHE-CONTROL: max-age=soon\r\n"))
	device.Say(alive("uuid:below", "CACHE-CONTROL: max-age=-5\r\n"))
	device.Say(alive("uuid:other", "CACHE-CONTROL: no-cache\r\n"))
	// The first that is said is the one, readable or not; a name that
	// only begins like it is another; and a length past what a number of
	// seconds can be as a time is a long one, not a short one.
	device.Say(alive("uuid:first", "CACHE-CONTROL: max-age=soon, max-age=1\r\n"))
	device.Say(alive("uuid:maximum", "CACHE-CONTROL: maximum=1\r\n"))
	device.Say(alive("uuid:huge", "CACHE-CONTROL: max-age=9223372037\r\n"))
	device.Say(alive("uuid:huger", "CACHE-CONTROL: max-age=9223372036854775807\r\n"))

	for range 10 {
		next(t, calls, done)
	}

	began := time.Now()

	var lost []string

	for range 2 {
		call := next(t, calls, done)
		if call.change != ssdp.Lost {
			t.Fatalf("told %s of %s", call.change, call.of.USN)
		}

		lost = append(lost, call.of.USN)
	}

	slices.Sort(lost)

	if took := time.Since(
		began,
	); !slices.Equal(lost, []string{"uuid:one", "uuid:zero"}) || took < quiet ||
		took > 3*time.Second {
		t.Fatalf("lost %v after %s", lost, took)
	}

	nothing(t, calls)
}

// TestASlowHandler: a handler may take its time: its Browse is told how
// things stand when it comes back, and another's is not kept waiting.
func TestASlowHandler(t *testing.T) {
	t.Parallel()

	lan, _, browser := network(t, ssdp.Config{})
	device := lan.Device(address("192.0.2.20"), light(1800)[0])
	device.Mute(true)

	entered, release := make(chan struct{}, 1), make(chan struct{})

	var slow []string

	slowDone := make(chan error, 1)
	ctx, cancel := context.WithCancel(t.Context())

	defer cancel()

	go func() {
		slowDone <- browser.Browse(ctx, ssdp.All, func(change ssdp.Change, of ssdp.Advertisement) {
			slow = append(slow, change.String()+" "+of.USN)

			select {
			case entered <- struct{}{}:
			default:
			}

			<-release
		})
	}()

	calls, done, _ := browsing(t, browser, ssdp.All)

	device.Announce()
	next(t, calls, done)

	select {
	case <-entered:
	case <-time.After(wait):
		t.Fatal("the slow handler was not called")
	}

	// Four hundred others come and go, and the first goes, while it
	// waits: fewer datagrams than a link holds unread, so none is dropped.
	for round := range 400 {
		usn := "uuid:" + strconv.Itoa(round)
		device.Say(
			"NOTIFY * HTTP/1.1\r\nNT: upnp:rootdevice\r\nNTS: ssdp:alive\r\nUSN: " + usn + "\r\nLOCATION: http://192.0.2.20/d.xml\r\n\r\n",
		)
		device.Say("NOTIFY * HTTP/1.1\r\nNT: upnp:rootdevice\r\nNTS: ssdp:byebye\r\nUSN: " + usn + "\r\n\r\n")
	}

	device.Goodbye()

	// The other Browse is told all the while, to the last.
	for {
		if call := next(t, calls, done); call.change == ssdp.Lost && call.of.USN == lightUDN+"::"+ssdp.RootDevice {
			break
		}
	}

	close(release)
	cancel()

	if err := ended(t, slowDone); !errors.Is(err, context.Canceled) {
		t.Fatalf("the slow Browse ended with %v", err)
	}

	// Found, and then lost: nothing of what came and went in between.
	root := lightUDN + "::" + ssdp.RootDevice
	if !slices.Equal(slow, []string{"found " + root, "lost " + root}) &&
		!slices.Equal(slow, []string{"found " + root}) {
		t.Errorf("the slow handler was told %v", slow)
	}
}

// TestWhatIsKept: a Browser keeps a bounded number of advertisements,
// and takes no more until some are gone.
func TestWhatIsKept(t *testing.T) {
	t.Parallel()

	lan, _, browser := network(t, ssdp.Config{})
	device := lan.Device(address("192.0.2.20"))
	device.Mute(true)

	calls, done, _ := browsing(t, browser, ssdp.All)

	say := func(kind, usn string) {
		device.Say("NOTIFY * HTTP/1.1\r\nNT: upnp:rootdevice\r\nNTS: ssdp:" + kind + "\r\nUSN: " + usn +
			"\r\nLOCATION: http://192.0.2.20/d.xml\r\n\r\n")
	}

	for round := range 5000 {
		say("alive", "uuid:"+strconv.Itoa(round))

		// As fast as it is read: nothing is dropped on the way in.
		if round < 4096 {
			next(t, calls, done)
		}
	}

	nothing(t, calls)

	say("byebye", "uuid:0")

	if lost := next(t, calls, done); lost.change != ssdp.Lost || lost.of.USN != "uuid:0" {
		t.Fatalf("told %s of %s", lost.change, lost.of.USN)
	}

	say("alive", "uuid:new")

	if found := next(t, calls, done); found.change != ssdp.Found || found.of.USN != "uuid:new" {
		t.Fatalf("told %s of %s", found.change, found.of.USN)
	}

	// And one advertisement is at a bounded number of places.
	for port := range 20 {
		device.Say(
			"NOTIFY * HTTP/1.1\r\nNT: upnp:rootdevice\r\nNTS: ssdp:alive\r\nUSN: uuid:new\r\nLOCATION: http://192.0.2.20:" +
				strconv.Itoa(
					1000+port,
				) + "/d.xml\r\n\r\n",
		)
	}

	// In as many calls as it takes: eight, and no more.
	for places := 0; places != 8; {
		places = len(next(t, calls, done).of.Locations)
	}

	nothing(t, calls)
}

// TestHowABrowseEnds: with its context, with the Browser, and with the
// network; and what cannot be asked for is not.
func TestHowABrowseEnds(t *testing.T) {
	t.Parallel()

	lan, _, browser := network(t, ssdp.Config{})
	lan.Device(address("192.0.2.20"), light(1800)...)

	// Something is found, and kept.
	calls, done, cancel := browsing(t, browser, ssdp.All)
	next(t, calls, done)
	cancel()

	if err := ended(t, done); !errors.Is(err, context.Canceled) {
		t.Errorf("a Browse whose context ended = %v", err)
	}

	for name, target := range map[string]string{
		"nothing": "", "two lines": "upnp:rootdevice\r\nX: y", "a control character": "a\x00b", "too long": strings.Repeat("a", 600),
	} {
		if err := browser.Browse(
			t.Context(),
			target,
			func(ssdp.Change, ssdp.Advertisement) {},
		); !errors.Is(
			err,
			ssdp.ErrInvalid,
		) {
			t.Errorf("browsing for %s = %v, want ErrInvalid", name, err)
		}
	}

	_, done, _ = browsing(t, browser, ssdp.All)

	// Under way before the Browser is closed.
	time.Sleep(quiet / 10)

	if err := browser.Close(); err != nil {
		t.Fatal(err)
	}

	if err := ended(t, done); !errors.Is(err, ssdp.ErrClosed) {
		t.Errorf("a Browse whose Browser was closed = %v", err)
	}

	// A Browse of a Browser that is over is told nothing of what it kept.
	err := browser.Browse(t.Context(), ssdp.All, func(change ssdp.Change, of ssdp.Advertisement) {
		t.Errorf("a Browse of a closed Browser was told %s of %s", change, of.USN)
	})
	if !errors.Is(err, ssdp.ErrClosed) {
		t.Errorf("a Browse of a closed Browser = %v", err)
	}

	if err := browser.Close(); err != nil {
		t.Errorf("closing twice = %v", err)
	}
}

// errNetwork is what the network of TestTheNetworkFails ends with.
var errNetwork = errors.New("the cable was cut")

// TestTheNetworkFails: a network that can tell of its links no more
// ends every Browse with why, and is not an ErrClosed.
func TestTheNetworkFails(t *testing.T) {
	t.Parallel()

	lan, host, browser := network(t, ssdp.Config{})
	lan.Device(address("192.0.2.20"), light(1800)...)

	calls, done, _ := browsing(t, browser, ssdp.All)
	next(t, calls, done)

	host.Fail(errNetwork)

	if err := ended(t, done); !errors.Is(err, errNetwork) || errors.Is(err, ssdp.ErrClosed) {
		t.Errorf("a Browse whose network failed = %v", err)
	}

	err := browser.Browse(t.Context(), ssdp.All, func(change ssdp.Change, of ssdp.Advertisement) {
		t.Errorf("a Browse after the network failed was told %s of %s", change, of.USN)
	})
	if !errors.Is(err, errNetwork) {
		t.Errorf("a Browse after the network failed = %v", err)
	}

	// A network that fails before it has told of its links is no Browser.
	failing := ssdptest.NewHost()
	failing.Fail(errNetwork)

	if _, err = ssdp.Open(t.Context(), ssdp.Config{Network: unsettled{failing}}); !errors.Is(err, errNetwork) {
		t.Errorf("Open on a network that fails = %v", err)
	}

	ended, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err = ssdp.Open(
		ended,
		ssdp.Config{Network: unsettled{ssdptest.NewHost()}},
	); !errors.Is(
		err,
		context.Canceled,
	) {
		t.Errorf("Open under a context that ended = %v", err)
	}

	if _, err = ssdp.Open(
		t.Context(),
		ssdp.Config{Network: ssdptest.NewHost(), UserAgent: "two\r\nlines"},
	); !errors.Is(
		err,
		ssdp.ErrInvalid,
	) {
		t.Errorf("Open with a user agent of two lines = %v", err)
	}
}

// careless is a network that ends with an error and leaves its links
// open: one that does not keep its side. Its links last as long as the
// test does.
type careless struct {
	*ssdptest.Host

	lasting context.Context //nolint:containedctx // the test's own, which outlives the Watch
	fail    chan error
}

func (c careless) Watch(ctx context.Context, handle func(ssdp.LinkEvent)) error {
	go func() { _ = c.Host.Watch(c.lasting, handle) }()

	select {
	case err := <-c.fail:
		return err
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// TestABrowseAfterTheNetworkFailed: it is told nothing of what the
// Browser kept, though the links were left open and nothing was
// forgotten.
func TestABrowseAfterTheNetworkFailed(t *testing.T) {
	t.Parallel()

	lan, host := ssdptest.NewLAN(), ssdptest.NewHost()
	host.Plug(lan, "en0", 1, prefix("192.0.2.10/24"))
	lan.Device(address("192.0.2.20"), light(1800)...)

	failing := careless{Host: host, lasting: t.Context(), fail: make(chan error, 1)}

	browser, err := ssdp.Open(t.Context(), ssdp.Config{Network: failing})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = browser.Close() })

	calls, done, _ := browsing(t, browser, ssdp.All)
	next(t, calls, done)

	failing.fail <- errNetwork

	if err = ended(t, done); !errors.Is(err, errNetwork) {
		t.Fatalf("a Browse whose network failed = %v", err)
	}

	err = browser.Browse(t.Context(), ssdp.All, func(change ssdp.Change, of ssdp.Advertisement) {
		t.Errorf("a Browse after the network failed was told %s of %s", change, of.USN)
	})
	if !errors.Is(err, errNetwork) {
		t.Errorf("a Browse after the network failed = %v", err)
	}

	// And closing does not wait for links the network left open.
	closed := make(chan error, 1)

	go func() { closed <- browser.Close() }()

	select {
	case err = <-closed:
		if err != nil || host.Open("en0") {
			t.Errorf("closing = %v, the link open: %t", err, host.Open("en0"))
		}
	case <-time.After(wait):
		t.Fatal("closing waits for links the network left open")
	}
}

// unsettled is a network that never says it has told of every link.
type unsettled struct{ *ssdptest.Host }

func (u unsettled) Watch(ctx context.Context, handle func(ssdp.LinkEvent)) error {
	return u.Host.Watch(ctx, func(event ssdp.LinkEvent) {
		if event.Change != ssdp.Settled {
			handle(event)
		}
	})
}

// TestTheHostsOwnNetwork: a Browser opens on the host's own network,
// whatever interfaces it has, and closes. What it finds there is no
// test's to say.
func TestTheHostsOwnNetwork(t *testing.T) {
	t.Parallel()

	browser, err := ssdp.Open(t.Context(), ssdp.Config{})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), quiet)
	defer cancel()

	// Its time is up, or the system let no search leave, which a program
	// kept off the network is told: both are how it should end here.
	err = browser.Browse(ctx, ssdp.RootDevice, func(ssdp.Change, ssdp.Advertisement) {})
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "the network is lost") {
		t.Errorf("a Browse on the host's network = %v", err)
	}

	if err = browser.Close(); err != nil {
		t.Errorf("closing = %v", err)
	}
}

// said is an announcement as a device sends it.
func said(kind, usn, what, location, more string) string {
	text := "NOTIFY * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nNT: " + what + "\r\nNTS: ssdp:" + kind + "\r\nUSN: " + usn + "\r\n"
	if location != "" {
		text += "LOCATION: " + location + "\r\n"
	}

	return text + more + "\r\n"
}

// errBroken is what a link of the tests breaks with.
var errBroken = errors.New("the interface caught fire")

// TestALinkThatBreaks: a link that can be read no more, with no word
// from the network, takes what was heard on it; the last one to break
// ends every Browse with why, and a link the network takes away does
// not.
func TestALinkThatBreaks(t *testing.T) {
	t.Parallel()

	lan, host, browser := network(t, ssdp.Config{})
	host.Plug(lan, "en1", 2, prefix("192.0.2.11/24"))

	device := lan.Device(address("192.0.2.20"), light(1800)...)
	device.Mute(true)
	device.Announce()

	calls, done, _ := browsing(t, browser, ssdp.RootDevice)

	both := next(t, calls, done)
	for len(both.of.Locations) != 2 {
		both = next(t, calls, done)
	}

	host.Sever("en0", errBroken)

	if one := next(
		t,
		calls,
		done,
	); one.change != ssdp.Changed || len(one.of.Locations) != 1 ||
		one.of.Locations[0].Interface.Name != "en1" {
		t.Fatalf("with one link broken: %s of %+v", one.change, one.of.Locations)
	}

	// What it is given back for: nothing reads it any more.
	for deadline := time.Now().Add(wait); host.Open("en0"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("a broken link was left open")
		}
	}

	// A new Browse is served by the link that is left.
	device.Mute(false)

	late, lateDone, _ := browsing(t, browser, lightUDN)
	if found := next(t, late, lateDone); found.change != ssdp.Found {
		t.Fatalf("a Browse after a link broke was told %s", found.change)
	}

	host.Sever("en1", errBroken)

	if lost := next(t, calls, done); lost.change != ssdp.Lost {
		t.Fatalf("with both links broken: %s", lost.change)
	}

	if err := ended(t, done); !errors.Is(err, errBroken) || errors.Is(err, ssdp.ErrClosed) {
		t.Fatalf("a Browse whose last link broke = %v", err)
	}

	if err := browser.Browse(
		t.Context(),
		ssdp.All,
		func(ssdp.Change, ssdp.Advertisement) {},
	); !errors.Is(
		err,
		errBroken,
	) {
		t.Fatalf("a Browse after the last link broke = %v", err)
	}
}

// TestALinkTakenAway: the network taking its links away, the last one
// too, is no failure: a Browse goes on, and finds again on what comes.
func TestALinkTakenAway(t *testing.T) {
	t.Parallel()

	lan, host, browser := network(t, ssdp.Config{})
	lan.Device(address("192.0.2.20"), light(1800)[0])

	calls, done, _ := browsing(t, browser, ssdp.RootDevice)

	next(t, calls, done)
	host.Unplug("en0")

	if lost := next(t, calls, done); lost.change != ssdp.Lost {
		t.Fatalf("with the link gone: %s", lost.change)
	}

	nothing(t, calls)
	host.Plug(lan, "en0", 1, prefix("192.0.2.10/24"))

	if found := next(t, calls, done); found.change != ssdp.Found {
		t.Fatalf("with the link back: %s", found.change)
	}
}

// TestASearchThatCannotLeave: a link on which no search can be sent is
// given up, and when it was the only one the Browse ends with why:
// silence would read as a network with no device on it.
func TestASearchThatCannotLeave(t *testing.T) {
	t.Parallel()

	lan, host, browser := network(t, ssdp.Config{})
	host.Plug(lan, "en1", 2, prefix("192.0.2.11/24"))
	host.Jam("en1", errBroken)

	lan.Device(address("192.0.2.20"), light(1800)...)

	calls, done, _ := browsing(t, browser, ssdp.RootDevice)

	// Found on the link that asked: an answer goes to the port its
	// search came from.
	found := next(t, calls, done)
	if found.change != ssdp.Found || len(found.of.Locations) != 1 || found.of.Locations[0].Interface.Name != "en0" {
		t.Fatalf("with one link that cannot ask: %s of %+v", found.change, found.of.Locations)
	}

	// Given up after as many failures as a search has sends.
	for deadline := time.Now().Add(wait); host.Open("en1"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("a link that cannot ask was kept")
		}
	}

	nothing(t, calls)

	host.Jam("en0", errBroken)

	// The next search is what finds out.
	late, lateDone, _ := browsing(t, browser, lightUDN)

	if err := ended(t, lateDone); !errors.Is(err, errBroken) || errors.Is(err, ssdp.ErrClosed) {
		t.Fatalf("a Browse whose searches cannot leave = %v", err)
	}

	select {
	case call := <-late:
		t.Errorf("told %s", call.change)
	default:
	}

	if err := ended(t, done); !errors.Is(err, errBroken) {
		t.Fatalf("the other Browse = %v", err)
	}
}

// TestASearchThatLeavesAtLast: a search that cannot be sent once is no
// reason to give a link up: the next send may leave.
func TestASearchThatLeavesAtLast(t *testing.T) {
	t.Parallel()

	lan, host, browser := network(t, ssdp.Config{})
	lan.Device(address("192.0.2.20"), light(1800)...)

	host.Jam("en0", errBroken)

	// Several things browsed for, one after the other in a moment, are
	// as many searches that cannot leave: one bad moment, not a link on
	// which nothing leaves.
	calls, done, _ := browsing(t, browser, ssdp.RootDevice)

	time.Sleep(5 * time.Millisecond)

	other, otherDone, _ := browsing(t, browser, lightUDN)

	time.Sleep(5 * time.Millisecond)

	third, thirdDone, _ := browsing(t, browser, lightType)

	for _, more := range []string{ssdp.All, "urn:x:device:A:1", "urn:x:device:B:1"} {
		time.Sleep(5 * time.Millisecond)
		browsing(t, browser, more)
	}

	// Past the first send, ahead of the second.
	time.Sleep(quiet / 3)
	host.Jam("en0", nil)

	for _, browse := range []struct {
		calls chan told
		done  chan error
	}{{calls, done}, {other, otherDone}, {third, thirdDone}} {
		if found := next(t, browse.calls, browse.done); found.change != ssdp.Found {
			t.Fatalf("told %s", found.change)
		}
	}

	if !host.Open("en0") {
		t.Error("a link whose first searches could not leave was given up")
	}
}

// TestSearchesThatFailNowAndThen: it takes as many moments in a row, with
// no search leaving, as a search has sends: one that leaves in between
// clears the count.
func TestSearchesThatFailNowAndThen(t *testing.T) {
	t.Parallel()

	lan, host, browser := network(t, ssdp.Config{})
	lan.Device(address("192.0.2.20"), light(1800)...)

	// A search's sends are at 0, 0.4 and 1.2 seconds: the first fails,
	// the second leaves, the third fails.
	host.Jam("en0", errBroken)

	calls, done, _ := browsing(t, browser, ssdp.RootDevice)

	time.Sleep(200 * time.Millisecond)
	host.Jam("en0", nil)

	if found := next(t, calls, done); found.change != ssdp.Found {
		t.Fatalf("told %s", found.change)
	}

	time.Sleep(600 * time.Millisecond)
	host.Jam("en0", errBroken)
	time.Sleep(1500 * time.Millisecond)

	// And the first of another search fails, which is the third failure
	// and the second in a row: its second leaves.
	other, otherDone, _ := browsing(t, browser, lightUDN)

	time.Sleep(200 * time.Millisecond)
	host.Jam("en0", nil)

	if found := next(t, other, otherDone); found.change != ssdp.Found {
		t.Fatalf("told %s", found.change)
	}

	if !host.Open("en0") {
		t.Error("a link was given up for failures that were not in a row")
	}
}

// TestASilenceLongAgo: a search that could not be sent long ago does not
// count against a link with the next that cannot.
func TestASilenceLongAgo(t *testing.T) {
	t.Parallel()

	lan, host, browser := network(t, ssdp.Config{})
	lan.Device(address("192.0.2.20"), light(1800)...)

	calls, done, _ := browsing(t, browser, ssdp.RootDevice)

	if found := next(t, calls, done); found.change != ssdp.Found {
		t.Fatalf("told %s", found.change)
	}

	// The last send of the first search fails, at 1.2 seconds.
	time.Sleep(800 * time.Millisecond)
	host.Jam("en0", errBroken)
	time.Sleep(3 * time.Second)

	// Long after, the first two of another fail, and its third leaves.
	other, otherDone, _ := browsing(t, browser, lightUDN)

	time.Sleep(800 * time.Millisecond)
	host.Jam("en0", nil)

	if found := next(t, other, otherDone); found.change != ssdp.Found {
		t.Fatalf("told %s", found.change)
	}

	if !host.Open("en0") {
		t.Error("a link was given up for a failure of long ago")
	}
}

// TestGoodbyes: a goodbye takes the places it was said from, on the
// link it was heard on: not those of another link, nor those another
// address announced.
func TestGoodbyes(t *testing.T) {
	t.Parallel()

	lan, host, browser := network(t, ssdp.Config{})

	// The same device on two networks that number their hosts alike, at
	// the same address on each.
	other := ssdptest.NewLAN()
	host.Plug(other, "en1", 2, prefix("192.0.2.11/24"))

	root := lightUDN + "::" + ssdp.RootDevice
	near := lan.Device(address("192.0.2.20"), ssdptest.Advert{USN: root, Type: ssdp.RootDevice, Location: lightAt})
	there := other.Device(address("192.0.2.20"),
		ssdptest.Advert{USN: root, Type: ssdp.RootDevice, Location: "http://192.0.2.20/elsewhere.xml"})

	near.Mute(true)
	there.Mute(true)

	calls, done, _ := browsing(t, browser, ssdp.RootDevice)

	near.Announce()
	next(t, calls, done)
	there.Announce()

	if both := next(t, calls, done); len(both.of.Locations) != 2 {
		t.Fatalf("on two networks: %+v", both.of.Locations)
	}

	// A stranger on the first network says goodbye under the device's name.
	lan.Device(address("192.0.2.30")).Say(said("byebye", root, ssdp.RootDevice, "", ""))
	nothing(t, calls)

	there.Goodbye()

	if one := next(t, calls, done); one.change != ssdp.Changed || !slices.Equal(one.of.Locations, here()) {
		t.Fatalf("with a goodbye on one network: %s of %+v", one.change, one.of.Locations)
	}

	near.Goodbye()

	if lost := next(t, calls, done); lost.change != ssdp.Lost {
		t.Fatalf("with a goodbye on both: %s", lost.change)
	}
}

// deaf is a network that says a link is down and leaves it open.
type deaf struct {
	*ssdptest.Host

	told  chan func(ssdp.LinkEvent)
	links chan ssdp.Link
}

func (d deaf) Watch(ctx context.Context, handle func(ssdp.LinkEvent)) error {
	d.told <- handle

	return d.Host.Watch(ctx, func(event ssdp.LinkEvent) {
		if event.Change == ssdp.Up {
			d.links <- event.Link
		}

		handle(event)
	})
}

// TestWhatALinkThatIsDownSays: once the network has said a link is down,
// what still arrives on it is not taken.
func TestWhatALinkThatIsDownSays(t *testing.T) {
	t.Parallel()

	lan, host := ssdptest.NewLAN(), ssdptest.NewHost()
	host.Plug(lan, "en0", 1, prefix("192.0.2.10/24"))

	lying := deaf{Host: host, told: make(chan func(ssdp.LinkEvent), 1), links: make(chan ssdp.Link, 1)}

	browser, err := ssdp.Open(t.Context(), ssdp.Config{Network: lying})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = browser.Close() })

	device := lan.Device(address("192.0.2.20"), light(1800)[0])
	device.Mute(true)

	calls, done, _ := browsing(t, browser, ssdp.RootDevice)

	device.Announce()
	next(t, calls, done)

	(<-lying.told)(ssdp.LinkEvent{Link: <-lying.links, Change: ssdp.Down})

	if lost := next(t, calls, done); lost.change != ssdp.Lost {
		t.Fatalf("with the link said down: %s", lost.change)
	}

	device.Announce()
	nothing(t, calls)
}

// TestALinkClosedWithNoWord: a link that is closed is the network's
// doing, whether or not the network has said so yet: what was heard on
// it is lost, and the Browser is not.
func TestALinkClosedWithNoWord(t *testing.T) {
	t.Parallel()

	lan, host := ssdptest.NewLAN(), ssdptest.NewHost()
	host.Plug(lan, "en0", 1, prefix("192.0.2.10/24"))

	wrapped := deaf{Host: host, told: make(chan func(ssdp.LinkEvent), 1), links: make(chan ssdp.Link, 1)}

	browser, err := ssdp.Open(t.Context(), ssdp.Config{Network: wrapped})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = browser.Close() })

	lan.Device(address("192.0.2.20"), light(1800)[0])

	calls, done, _ := browsing(t, browser, ssdp.RootDevice)

	next(t, calls, done)

	if err = (<-wrapped.links).Close(); err != nil {
		t.Fatal(err)
	}

	if lost := next(t, calls, done); lost.change != ssdp.Lost {
		t.Fatalf("with the link closed: %s", lost.change)
	}

	select {
	case err = <-done:
		t.Fatalf("the Browse ended: %v", err)
	case <-time.After(quiet):
	}
}

// ending is a network with no link, whose Watch ends when told to, with
// nothing to say.
type ending chan struct{}

func (e ending) Watch(_ context.Context, handle func(ssdp.LinkEvent)) error {
	handle(ssdp.LinkEvent{Change: ssdp.Settled})
	<-e

	return nil
}

// TestANetworkThatEndsWithNothingToSay: it is a failure all the same.
func TestANetworkThatEndsWithNothingToSay(t *testing.T) {
	t.Parallel()

	over := make(ending)

	browser, err := ssdp.Open(t.Context(), ssdp.Config{Network: over})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = browser.Close() })

	_, done, _ := browsing(t, browser, ssdp.All)

	close(over)

	if err = ended(
		t,
		done,
	); err == nil || errors.Is(err, ssdp.ErrClosed) ||
		!strings.Contains(err.Error(), "the network is lost") {
		t.Fatalf("a Browse whose network ended = %v", err)
	}
}

// TestAPlaceSaidAgain: a place that is known is set anew when it is said
// again, though as many places as are kept are known.
func TestAPlaceSaidAgain(t *testing.T) {
	t.Parallel()

	lan, _, browser := network(t, ssdp.Config{})
	device := lan.Device(address("192.0.2.20"))
	device.Mute(true)

	calls, done, _ := browsing(t, browser, ssdp.All)

	at := func(port int) string { return "http://192.0.2.20:" + strconv.Itoa(1000+port) + "/d.xml" }

	for port := range 8 {
		device.Say(said("alive", "uuid:1", "upnp:rootdevice", at(port), "CACHE-CONTROL: max-age=2\r\n"))
	}

	for places := 0; places != 8; {
		places = len(next(t, calls, done).of.Locations)
	}

	// The first again, for long; the other seven run out.
	device.Say(said("alive", "uuid:1", "upnp:rootdevice", at(0), "CACHE-CONTROL: max-age=1800\r\n"))

	last := next(t, calls, done)
	for len(last.of.Locations) != 1 {
		if last.change != ssdp.Changed {
			t.Fatalf("told %s", last.change)
		}

		last = next(t, calls, done)
	}

	if last.of.Locations[0].URL != at(0) {
		t.Fatalf("the place that is left: %+v", last.of.Locations)
	}

	nothing(t, calls)
}

// TestAskedAfterOnce: what is about to run out is searched for once
// more, not again and again until it has.
func TestAskedAfterOnce(t *testing.T) {
	t.Parallel()

	lan, _, browser := network(t, ssdp.Config{})
	device := lan.Device(address("192.0.2.20"), light(8)[0])

	calls, done, _ := browsing(t, browser, ssdp.RootDevice)

	next(t, calls, done)

	// It answers no more: asked after at six seconds, lost at eight.
	device.Mute(true)

	if lost := next(t, calls, done); lost.change != ssdp.Lost {
		t.Fatalf("told %s", lost.change)
	}

	if sent := len(lan.Searches()); sent != 6 {
		t.Errorf("%d searches were sent, want the first three and three more", sent)
	}
}

// searched is how many searches for a target were sent on a LAN.
func searched(lan *ssdptest.LAN, target string) int {
	sent := 0

	for _, search := range lan.Searches() {
		if strings.Contains(search, "ST: "+target+"\r\n") {
			sent++
		}
	}

	return sent
}

// TestABrowseThatEnded: what it browsed for is asked after no more on
// its account.
func TestABrowseThatEnded(t *testing.T) {
	t.Parallel()

	lan, _, browser := network(t, ssdp.Config{})
	lan.Device(address("192.0.2.20"), light(2)...)

	roots, rootsDone, stop := browsing(t, browser, ssdp.RootDevice)
	all, allDone, _ := browsing(t, browser, ssdp.All)

	next(t, roots, rootsDone)
	stop()

	if err := ended(t, rootsDone); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}

	// Past the time what was found is asked after, twice over: for
	// everything, which is still browsed for, and not for the root
	// devices.
	for range 3 {
		next(t, all, allDone)
	}

	time.Sleep(3500 * time.Millisecond)

	if roots, everything := searched(lan, ssdp.RootDevice), searched(lan, ssdp.All); roots != 3 || everything <= 3 {
		t.Errorf("%d searches for what is browsed for no more, and %d for what is", roots, everything)
	}
}

// TestASearchSharedByOneThatEnded: a Browse that ends does not have the
// search it shared sent again for one that begins.
func TestASearchSharedByOneThatEnded(t *testing.T) {
	t.Parallel()

	lan, _, browser := network(t, ssdp.Config{})

	_, done, stop := browsing(t, browser, ssdp.RootDevice)
	browsing(t, browser, ssdp.RootDevice)

	stop()

	if err := ended(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}

	browsing(t, browser, ssdp.RootDevice)

	time.Sleep(2 * time.Second)

	if sent := searched(lan, ssdp.RootDevice); sent != 3 {
		t.Errorf("%d searches were sent, want 3", sent)
	}
}

// TestWhatAnAdvertisementIs: its name says what it advertises, after
// the device's; a name that is a device's alone is that device; any
// other name leaves it to what the message says. One that is no longer
// what is browsed for is lost to that Browse.
func TestWhatAnAdvertisementIs(t *testing.T) {
	t.Parallel()

	lan, _, browser := network(t, ssdp.Config{})
	device := lan.Device(address("192.0.2.20"))
	device.Mute(true)

	calls, done, _ := browsing(t, browser, ssdp.All)
	roots, rootsDone, _ := browsing(t, browser, ssdp.RootDevice)

	for usn, want := range map[string]string{
		"uuid:1::urn:x:device:Thing:2": "urn:x:device:Thing:2",
		"UUID:2":                       "UUID:2",
		"uuid:3::":                     "uuid:3",
		"odd":                          "upnp:rootdevice",
	} {
		device.Say(said("alive", usn, "upnp:rootdevice", lightAt, ""))

		if found := next(t, calls, done); found.of.USN != usn || found.of.Type != want {
			t.Errorf("%s advertises %q, want %q", usn, found.of.Type, want)
		}
	}

	// The odd one is a root device, to the Browse for those; said as
	// something else, it is one no more.
	if found := next(t, roots, rootsDone); found.change != ssdp.Found || found.of.USN != "odd" {
		t.Fatalf("told %s of %s", found.change, found.of.USN)
	}

	device.Say(said("alive", "odd", "something:else", lightAt, ""))

	if lost := next(
		t,
		roots,
		rootsDone,
	); lost.change != ssdp.Lost || lost.of.USN != "odd" ||
		lost.of.Type != "upnp:rootdevice" {
		t.Fatalf("told %s of %+v", lost.change, lost.of)
	}

	nothing(t, roots)

	// What is known serves under the same name in another case, and a
	// version is one of a type alone: of no other name.
	device.Say(said("alive", "uuid:9::foo:2", "foo:2", lightAt, ""))
	next(t, calls, done)

	loud, loudDone, _ := browsing(t, browser, "SOMETHING:ELSE")
	if found := next(t, loud, loudDone); found.of.USN != "odd" {
		t.Errorf("under a name in capitals: %s", found.of.USN)
	}

	earlier, _, _ := browsing(t, browser, "foo:1")
	nothing(t, earlier)
}

// TestACountOfStarts: thirty-one bits, as the standard has it; one that
// appears where there was none is a change.
func TestACountOfStarts(t *testing.T) {
	t.Parallel()

	lan, _, browser := network(t, ssdp.Config{})
	device := lan.Device(address("192.0.2.20"))
	device.Mute(true)

	calls, done, _ := browsing(t, browser, ssdp.All)

	device.Say(said("alive", "uuid:1", "upnp:rootdevice", lightAt, ""))

	if _, counted := next(t, calls, done).of.Boot(); counted {
		t.Fatal("a count where none was given")
	}

	for _, row := range []struct {
		given   string
		count   uint32
		counted bool
	}{
		// None, and then none of them: a count all the same.
		{"0", 0, true},
		{"2147483647", 2147483647, true},
		{"2147483648", 0, false},
		{" 7 ", 7, true},
		{"-1", 0, false},
		{"seven", 0, false},
	} {
		device.Say(said("alive", "uuid:1", "upnp:rootdevice", lightAt, "BOOTID.UPNP.ORG: "+row.given+"\r\n"))

		changed := next(t, calls, done)
		if count, counted := changed.of.Boot(); changed.change != ssdp.Changed || count != row.count ||
			counted != row.counted {
			t.Errorf("a count of %q: told %s with %d, %t", row.given, changed.change, count, counted)
		}

		// None again, between two: a change each way.
		device.Say(said("alive", "uuid:1", "upnp:rootdevice", lightAt, "BOOTID.UPNP.ORG: 1\r\n"))
		next(t, calls, done)
	}
}

// TestWhatAHandlerIsHanded: its own, to do with as it likes: what it
// does to it changes nothing of what it is told next.
func TestWhatAHandlerIsHanded(t *testing.T) {
	t.Parallel()

	lan, host, browser := network(t, ssdp.Config{})
	host.Plug(lan, "en1", 2, prefix("192.0.2.11/24"))

	device := lan.Device(address("192.0.2.20"), light(1800)[0])
	device.Mute(true)
	device.Boot(3)

	calls := make(chan told, 64)
	done := make(chan error, 1)

	go func() {
		done <- browser.Browse(t.Context(), ssdp.RootDevice, func(change ssdp.Change, of ssdp.Advertisement) {
			calls <- told{change: change, of: of}

			// Turned around, and rewritten.
			slices.Reverse(of.Locations)
			of.Header.Set("Bootid.upnp.org", "99")
		})
	}()

	device.Announce()

	both := next(t, calls, done)
	for len(both.of.Locations) != 2 {
		both = next(t, calls, done)
	}

	// The same, said again and again, is the same.
	for range 3 {
		device.Announce()
	}

	nothing(t, calls)
}

// TestTheHostsOwnLinks: a link of the host's own passes over a datagram
// larger than SSDP has, which on some systems is cut short and on others
// fails the read, and reads the next.
func TestTheHostsOwnLinks(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	links := make(chan ssdp.Link, 64)
	over := make(chan error, 1)
	stopped := make(chan struct{})

	go func() {
		defer close(stopped)

		over <- ssdp.System().Watch(ctx, func(event ssdp.LinkEvent) {
			switch event.Change {
			case ssdp.Up:
				links <- event.Link
			case ssdp.Settled:
				close(links)
			case ssdp.Down:
			}
		})
	}()

	defer func() {
		cancel()
		<-stopped
	}()

	var link ssdp.Link

	select {
	case link = <-links:
	case err := <-over:
		t.Skipf("the host's network cannot be opened here: %v", err)
	case <-time.After(wait):
		t.Fatal("the host's network told of nothing")
	}

	if link == nil {
		t.Skip("the host has no interface SSDP is spoken on")
	}

	// A link says what it is: its interface, and where its searches
	// leave from.
	described, ok := link.(interface{ String() string })
	if !ok {
		t.Fatalf("a %T does not say what it is", link)
	}

	// An interface's name may hold a space: "Ethernet 5".
	port, found := strings.CutPrefix(described.String(), link.Interface().Name+" ")
	if !found {
		t.Fatalf("a link on %q says it is %q", link.Interface().Name, described.String())
	}

	own, err := netip.ParseAddrPort(port)
	if err != nil || own.Addr() != link.Addrs()[0].Addr() {
		t.Fatalf("a link's own port is %q, on an interface at %v", port, link.Addrs())
	}

	var dialing net.Dialer

	conn, err := dialing.DialContext(ctx, "udp4", own.String())
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = conn.Close() }()

	const small = "HTTP/1.1 200 OK\r\nST: upnp:rootdevice\r\nUSN: uuid:1\r\n\r\n"

	// Larger by a byte, and by more; no larger than every system sends.
	for _, datagram := range []string{strings.Repeat("a", ssdp.MaxMessage+1), strings.Repeat("b", 9000), small} {
		if _, err = conn.Write([]byte(datagram)); err != nil {
			t.Skipf("this host cannot send itself a datagram of %d bytes: %v", len(datagram), err)
		}
	}

	type read struct {
		datagram string
		err      error
	}

	answered := make(chan read, 1)

	go func() {
		buffer := make([]byte, ssdp.MaxMessage)

		count, _, readErr := link.Answered(buffer)
		answered <- read{datagram: string(buffer[:count]), err: readErr}
	}()

	select {
	case got := <-answered:
		if got.err != nil || got.datagram != small {
			t.Fatalf("past two datagrams too large, read %d bytes and %v", len(got.datagram), got.err)
		}
	case <-time.After(wait):
		t.Fatal("nothing was read past two datagrams too large")
	}
}
