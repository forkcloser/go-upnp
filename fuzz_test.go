package upnp_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	upnp "github.com/forkcloser/go-upnp"
	"github.com/forkcloser/go-upnp/upnptest"
)

// said is a device that answers everything with one body.
type said string

func (s said) Do(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(s)))}, nil
}

// home is where the fuzzed device is.
const home = "192.0.2.1"

// FuzzDescribe: whatever a device serves as its description is read or
// refused as no description, and what is read asks nothing of any host
// but the device.
func FuzzDescribe(f *testing.F) {
	f.Add(upnptest.Description)
	f.Add(
		strings.Replace(upnptest.Description, "<device>", "<URLBase>http://192.0.2.1:8080/base/</URLBase><device>", 1),
	)
	f.Add(strings.Replace(upnptest.Description, upnptest.SwitchControl, "http://192.0.2.9/control", 1))
	f.Add(`<?xml version="1.0" encoding="ISO-8859-1"?><root><device><UDN>uuid:` + "\xe9" + `</UDN></device></root>`)
	f.Add(
		"\xef\xbb\xbf<ROOT><DEVICE><udn>uuid:1</udn><deviceList><device><UDN>uuid:2</UDN></device></deviceList></DEVICE></ROOT>",
	)
	f.Add("")

	f.Fuzz(func(t *testing.T, document string) {
		root, err := upnp.Describe(t.Context(), said(document), "http://"+home+":49152/description.xml")
		if err != nil {
			if !errors.Is(err, upnp.ErrAnswer) {
				t.Fatalf("a description that is not read = %v, want ErrAnswer", err)
			}

			return
		}

		if root.UDN == "" {
			t.Fatal("a root device with no UDN was read")
		}

		for device := range root.All() {
			var whatever struct {
				Name string `xml:"friendlyName"`
			}

			// Read from the same document: it holds every device counted.
			if err = device.Decode(&whatever); err != nil {
				t.Fatalf("Decode of %q, which Describe read: %v", device.UDN, err)
			}

			asked := make([]string, 0, len(device.Icons))
			for _, icon := range device.Icons {
				asked = append(asked, icon.URL)
			}

			for _, service := range device.Services {
				asked = append(asked, service.Control, service.Events, service.SCPD)
			}

			for _, where := range asked {
				if where == "" {
					continue
				}

				parsed, parseErr := url.Parse(where)
				if parseErr != nil || parsed.Hostname() != home || parsed.Scheme != "http" && parsed.Scheme != "https" {
					t.Fatalf("a URL of %q leaves the device: %q (%v)", device.UDN, where, parseErr)
				}
			}
		}
	})
}

// FuzzServiceDescription: whatever a device serves as a service's
// description is read or refused as none.
func FuzzServiceDescription(f *testing.F) {
	f.Add(upnptest.SwitchDescription)
	f.Add(
		`<SCPD><actionList><action><name>A</name><argumentList><argument><name>a</name></argument></argumentList></action></actionList></SCPD>`,
	)
	f.Add("")

	f.Fuzz(func(t *testing.T, document string) {
		service := upnp.Service{Type: upnptest.SwitchType, SCPD: "http://" + home + "/scpd.xml"}

		if _, err := service.Describe(t.Context(), said(document)); err != nil && !errors.Is(err, upnp.ErrAnswer) {
			t.Fatalf("a description that is not read = %v, want ErrAnswer", err)
		}
	})
}

// FuzzAnswer: whatever a device answers an action with is values, a
// refusal, or no answer.
func FuzzAnswer(f *testing.F) {
	f.Add(`<s:Envelope xmlns:s="x"><s:Body><u:R xmlns:u="y"><A>1</A><B><x/></B></u:R></s:Body></s:Envelope>`)
	f.Add(
		`<e><Body><Fault><detail><UPnPError><errorCode>402</errorCode><errorDescription>no</errorDescription></UPnPError></detail></Fault></Body></e>`,
	)
	f.Add(`<e><Body><Fault><detail>`)
	f.Add("\xef\xbb\xbf<e><BODY><r><A><![CDATA[<y/>]]></A></r></BODY></e>trailing")
	f.Add("")

	f.Fuzz(func(t *testing.T, answer string) {
		service := upnp.Service{Type: upnptest.SwitchType, Control: "http://" + home + "/control"}

		out, err := service.Call(t.Context(), said(answer), "GetStatus")
		if err != nil {
			if !errors.Is(err, upnp.ErrAnswer) && !errors.Is(err, upnp.ErrRefused) {
				t.Fatalf("an answer that is not read = %v, want ErrAnswer or ErrRefused", err)
			}

			return
		}

		for _, value := range out {
			if value.Name == "" {
				t.Fatalf("a value with no name was read: %+v", out)
			}
		}
	})
}

// granting is a device that grants every subscription, and says where
// each was told to deliver.
type granting chan string

func (g granting) Do(request *http.Request) (*http.Response, error) {
	//nolint:staticcheck // SA1008: the header is read under the name it goes on the wire with
	if callback := request.Header["CALLBACK"]; len(callback) == 1 {
		g <- strings.Trim(callback[0], "<>")
	}

	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Sid": {"uuid:1"}, "Timeout": {"Second-1800"}},
		Body:       io.NopCloser(strings.NewReader("")),
	}, nil
}

// FuzzEvent: whatever a device delivers as an event is told, or ends the
// Watch as no event.
func FuzzEvent(f *testing.F) {
	f.Add(`<e:propertyset xmlns:e="urn:schemas-upnp-org:event-1-0"><e:property><A>1</A></e:property></e:propertyset>`)
	f.Add("\xef\xbb\xbf<PROPERTYSET><PROPERTY><A>1</A><B><x/></B></PROPERTY><property/></PROPERTYSET>trailing")
	f.Add(`<propertyset><property><A>`)
	f.Add("")

	f.Fuzz(func(t *testing.T, body string) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		subscriber := upnp.NewSubscriber()
		callbacks := make(granting, 1)
		told := make(chan upnp.Event, 1)
		done := make(chan error, 1)

		go func() {
			done <- subscriber.Watch(
				ctx, callbacks, upnp.Service{Type: upnptest.SwitchType, Events: "http://" + home + "/events"},
				"http://192.0.2.2/", func(event upnp.Event) { told <- event },
			)
		}()

		request := httptest.NewRequestWithContext(ctx, "NOTIFY", <-callbacks, strings.NewReader(body))
		request.Header.Set("Sid", "uuid:1")
		request.Header.Set("Seq", "0")

		answer := httptest.NewRecorder()
		subscriber.ServeHTTP(answer, request)

		switch answer.Code {
		case http.StatusOK:
			for _, property := range (<-told).Properties {
				if property.Name == "" {
					t.Fatal("a variable with no name was told")
				}
			}

			cancel()

			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("a Watch that was told its event ended with %v", err)
			}
		case http.StatusBadRequest:
			if err := <-done; !errors.Is(err, upnp.ErrAnswer) || !errors.Is(err, upnp.ErrLost) {
				t.Fatalf("a Watch sent what is no event ended with %v, want ErrLost and ErrAnswer", err)
			}
		default:
			t.Fatalf("an event was answered %d", answer.Code)
		}
	})
}
