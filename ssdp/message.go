package ssdp

import (
	"bufio"
	"bytes"
	"net/netip"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// kind is what a message is, of those a control point hears.
type kind uint8

const (
	// alive is a device saying it is there: an announcement, or an
	// answer to a search.
	alive kind = iota + 1
	// goodbye is a device saying it is leaving.
	goodbye
)

// message is one SSDP message, read for what a control point needs.
type message struct {
	kind kind
	// usn and what are the advertisement's name and what it advertises:
	// the NT of an announcement, the ST of an answer.
	usn  string
	what string
	// location is the description's URL, checked; server the device's
	// word on itself; lasts how long the advertisement holds.
	location string
	server   string
	lasts    time.Duration
	header   textproto.MIMEHeader
}

// What an advertisement may claim.
const (
	// defaultAge is how long one holds that does not say: the least the
	// standard lets a device say.
	defaultAge = 30 * time.Minute
	// minAge and maxAge bound what a device says.
	minAge = time.Second
	maxAge = 24 * time.Hour
	// maxName bounds a USN, a type and a search target.
	maxName = 512
)

// read is a datagram as a message: an announcement or an answer to a
// search, from a device at that address. Anything else is none: a search
// of another control point's, an answer that is no success, a message
// with no name, and an advertisement whose description is not at the
// address it came from.
func read(datagram []byte, from netip.Addr) (message, bool) {
	reader := bufio.NewReader(bytes.NewReader(datagram))

	first, err := reader.ReadString('\n')
	if err != nil {
		return message{}, false
	}

	start := strings.Fields(first)
	if len(start) < 2 {
		return message{}, false
	}

	announced := strings.EqualFold(start[0], "NOTIFY")
	if !announced && (!strings.HasPrefix(strings.ToUpper(start[0]), "HTTP/") || start[1] != "200") {
		return message{}, false
	}

	// A device that ends its headers with no empty line said everything
	// above it all the same.
	header, err := textproto.NewReader(reader).ReadMIMEHeader()
	if err != nil && len(header) == 0 {
		return message{}, false
	}

	heard := message{
		kind: alive, usn: header.Get("Usn"), what: header.Get("St"), server: header.Get("Server"),
		lasts: age(header.Get("Cache-Control")), header: header,
	}

	if announced {
		heard.what = header.Get("Nt")

		switch strings.ToLower(header.Get("Nts")) {
		case "ssdp:alive", "ssdp:update":
		case "ssdp:byebye":
			heard.kind = goodbye
		default:
			return message{}, false
		}
	}

	if heard.usn == "" || heard.what == "" || len(heard.usn) > maxName || len(heard.what) > maxName {
		return message{}, false
	}

	if heard.kind == goodbye {
		return heard, true
	}

	heard.location = located(header.Get("Location"), from)

	return heard, heard.location != ""
}

// located is the URL of a description, when it is one that may be asked
// for: http or https, at the address the message came from. A device
// that advertises a description anywhere else is not taken at its word.
func located(raw string, from netip.Addr) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" {
		return ""
	}

	host, err := netip.ParseAddr(parsed.Hostname())
	if err != nil || host.Unmap().WithZone("") != from.Unmap().WithZone("") {
		return ""
	}

	return parsed.String()
}

// age is how long an advertisement holds: the max-age of its
// CACHE-CONTROL, within bounds.
func age(control string) time.Duration {
	for directive := range strings.SplitSeq(control, ",") {
		name, value, found := strings.Cut(strings.TrimSpace(directive), "=")
		if !found || !strings.EqualFold(strings.TrimSpace(name), "max-age") {
			continue
		}

		seconds, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || seconds < 0 {
			return defaultAge
		}

		// Bounded as a number of seconds: as a length of time a large one
		// is no longer large.
		return max(time.Duration(min(seconds, int(maxAge/time.Second)))*time.Second, minAge)
	}

	return defaultAge
}

// search is the message that asks devices under a target to answer.
func search(target, userAgent string) []byte {
	asked := "M-SEARCH * HTTP/1.1\r\n" +
		"HOST: " + Group().String() + "\r\n" +
		"MAN: \"ssdp:discover\"\r\n" +
		"MX: " + strconv.Itoa(int(answerWithin/time.Second)) + "\r\n" +
		"ST: " + target + "\r\n"

	if userAgent != "" {
		asked += "USER-AGENT: " + userAgent + "\r\n"
	}

	return []byte(asked + "\r\n")
}

// answerWithin is the time devices are told to spread their answers
// over.
const answerWithin = 2 * time.Second

// sayable says whether a target or a user agent can go into a message:
// one line, of what a header's value may hold.
func sayable(value string) bool {
	if len(value) > maxName {
		return false
	}

	for _, one := range []byte(value) {
		if one < ' ' || one == 0x7f {
			return false
		}
	}

	return true
}

// serves says whether what a device advertises is what a search asks
// for: everything, the same thing, or the same device or service type
// at the version asked for or a later one.
func serves(advertised, target string) bool {
	if target == All || strings.EqualFold(advertised, target) {
		return true
	}

	has, hasVersion, hasOK := versioned(advertised)
	wanted, wantedVersion, wantedOK := versioned(target)

	return hasOK && wantedOK && strings.EqualFold(has, wanted) && hasVersion >= wantedVersion
}

// versioned splits "urn:domain:device:kind:2" into what is ahead of its
// version, and the version.
func versioned(urn string) (string, int, bool) {
	cut := strings.LastIndexByte(urn, ':')
	if cut < 0 || !strings.HasPrefix(strings.ToLower(urn), "urn:") {
		return "", 0, false
	}

	version, err := strconv.Atoi(urn[cut+1:])
	if err != nil || version < 0 {
		return "", 0, false
	}

	return urn[:cut], version, true
}
