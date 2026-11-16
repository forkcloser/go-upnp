package ssdp_test

import (
	"net/url"
	"testing"

	"github.com/forkcloser/go-upnp/ssdp"
	"github.com/forkcloser/go-upnp/ssdp/ssdptest"
)

// FuzzMessage: whatever a device sends to the group is an advertisement
// with a name and a description at the device's own address, or is
// nothing.
func FuzzMessage(f *testing.F) {
	f.Add("NOTIFY * HTTP/1.1\r\nNT: upnp:rootdevice\r\nNTS: ssdp:alive\r\nUSN: uuid:1::upnp:rootdevice\r\n" +
		"LOCATION: http://192.0.2.20/d.xml\r\nCACHE-CONTROL: max-age=1800\r\nBOOTID.UPNP.ORG: 3\r\n\r\n")
	f.Add("HTTP/1.1 200 OK\r\nST: ssdp:all\r\nUSN: uuid:1\r\nLOCATION: http://192.0.2.99/d.xml\r\n\r\n")
	f.Add("NOTIFY * HTTP/1.1\r\nNT: a\r\nNTS: ssdp:byebye\r\nUSN: uuid:1\r\n\r\n")
	f.Add("notify * http/1.1\r\nnt: a\r\nnts: ssdp:alive\r\nusn: b\r\nlocation: http://[::ffff:192.0.2.20]/\r\n")
	f.Add("M-SEARCH * HTTP/1.1\r\nST: ssdp:all\r\n\r\n")
	f.Add("")

	const marker = "uuid:the-last"

	f.Fuzz(func(t *testing.T, datagram string) {
		lan, host := ssdptest.NewLAN(), ssdptest.NewHost()
		host.Plug(lan, "en0", 1, prefix("192.0.2.10/24"))

		browser, err := ssdp.Open(t.Context(), ssdp.Config{Network: host})
		if err != nil {
			t.Fatal(err)
		}

		defer func() { _ = browser.Close() }()

		device := lan.Device(address("192.0.2.20"))
		device.Mute(true)

		calls, done, _ := browsing(t, browser, ssdp.All)

		device.Say(datagram)
		device.Say(
			"NOTIFY * HTTP/1.1\r\nNT: last\r\nNTS: ssdp:alive\r\nUSN: " + marker + "\r\nLOCATION: http://192.0.2.20/last\r\n\r\n",
		)

		for {
			call := next(t, calls, done)
			if call.of.USN == marker {
				return
			}

			if call.of.USN == "" || call.of.Type == "" || len(call.of.Locations) == 0 {
				t.Fatalf("told %s of %+v", call.change, call.of)
			}

			for _, location := range call.of.Locations {
				parsed, parseErr := url.Parse(location.URL)
				if parseErr != nil || address(parsed.Hostname()).Unmap() != address("192.0.2.20") {
					t.Fatalf("an advertisement at %q, which is not its device (%v)", location.URL, parseErr)
				}
			}
		}
	})
}
