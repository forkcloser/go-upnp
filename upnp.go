// Package upnp is a UPnP control point's dealings with one device, over
// HTTP: reading its description, calling its actions, and receiving its
// events. Finding devices is package ssdp's.
//
// It opens no socket. The HTTP client every request goes through is the
// caller's, a Doer, and so are its timeouts, its redirects and its
// proxies. A device is trusted with nothing: what it sends is read up to
// a bound, and a description that points anywhere but at the host it was
// read from is refused.
//
// That holds for what a device writes, and a redirect is something a
// device says too: a client that follows redirects sends the request,
// an action's arguments with it, wherever the device points. This
// package cannot stop a client it does not make, and refuses what such a
// client brings back from another host, which is after the fact. A
// client for devices follows none:
//
//	client := &http.Client{
//		Timeout: 10 * time.Second,
//		CheckRedirect: func(*http.Request, []*http.Request) error {
//			return http.ErrUseLastResponse
//		},
//	}
//
// A redirect is then an ErrRefused that carries its status.
//
// A Service is plain data. It comes from a description, by Describe, or
// is written by hand by a program that knows where its device's services
// are and never reads a description.
package upnp

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Doer does an HTTP request: an *http.Client is one.
type Doer interface {
	// Do sends a request and returns its answer, as http.Client.Do does.
	Do(request *http.Request) (*http.Response, error)
}

// maxDocument is the most that is read of anything a device sends: a
// description, an answer, an event. The largest known, a media server's
// answer to a browse, is some hundreds of kilobytes.
const maxDocument = 4 << 20

// read is the body of an answer, whole, up to maxDocument.
func read(response *http.Response) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(response.Body, maxDocument+1))
	if err != nil {
		return nil, fmt.Errorf("reading the device's answer: %w", err)
	}

	if len(body) > maxDocument {
		return nil, fmt.Errorf("%w: more than %d bytes", ErrAnswer, maxDocument)
	}

	return normal(body)
}

// strayed says whether an answer came from another host than the one
// that was asked: a client that follows redirects goes where a device
// sends it, and what it brings back from there is not the device's.
func strayed(asked *url.URL, response *http.Response) error {
	if response.Request == nil || response.Request.URL == nil ||
		strings.EqualFold(response.Request.URL.Hostname(), asked.Hostname()) {
		return nil
	}

	return fmt.Errorf(
		"%w: the client followed a redirect to %s, which is not the device", ErrAnswer, response.Request.URL.Host,
	)
}

// get reads one document from a device.
func get(ctx context.Context, client Doer, where *url.URL) ([]byte, error) {
	if client == nil {
		return nil, fmt.Errorf("%w: no client", ErrInvalid)
	}

	target := where.String()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", target, err)
	}

	defer func() { _ = response.Body.Close() }()

	if err = strayed(where, response); err != nil {
		return nil, fmt.Errorf("reading %s: %w", target, err)
	}

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("reading %s: %w", target, &StatusError{Status: response.StatusCode})
	}

	body, err := read(response)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", target, err)
	}

	return body, nil
}

// absolute is a URL a device can be asked at: http or https, with a host.
func absolute(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: the URL %q: %w", ErrInvalid, raw, err)
	}

	if parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Hostname() == "" {
		return nil, fmt.Errorf("%w: %q is not an http URL with a host", ErrInvalid, raw)
	}

	return parsed, nil
}

// matches says whether a device or service type a device has serves for
// the one wanted: the same type, at that version or a later one, since a
// later version does all an earlier one does. Types that are no URN with
// a version are the same or are not.
func matches(has, wanted string) bool {
	if has == wanted {
		return true
	}

	hasKind, hasVersion, hasOK := versioned(has)
	wantedKind, wantedVersion, wantedOK := versioned(wanted)

	return hasOK && wantedOK && strings.EqualFold(hasKind, wantedKind) && hasVersion >= wantedVersion
}

// versioned splits "urn:domain:service:kind:2" into what is ahead of its
// version, and the version.
func versioned(kind string) (string, int, bool) {
	cut := strings.LastIndexByte(kind, ':')
	if cut < 0 {
		return "", 0, false
	}

	version := 0

	for _, digit := range kind[cut+1:] {
		if digit < '0' || digit > '9' || version > maxVersion {
			return "", 0, false
		}

		version = version*10 + int(digit-'0') //nolint:mnd // decimal
	}

	return kind[:cut], version, cut+1 < len(kind)
}

// maxVersion bounds a version read: past it is no version.
const maxVersion = 1 << 20
