package upnptest

import (
	"bytes"
	"context"
	"encoding/xml"
	"net/http"
	"strconv"
	"strings"
	"time"

	upnp "github.com/forkcloser/go-upnp"
)

// Subscription is one request about a subscription a Device was sent: a
// new one, a renewal, or its end.
type Subscription struct {
	// Method is SUBSCRIBE or UNSUBSCRIBE, and Path where it was sent.
	Method string
	Path   string
	// SID, Callback, NT and Timeout are the request's headers, as sent.
	// A new subscription has a callback and no SID; a renewal and an end
	// have the SID alone.
	SID      string
	Callback string
	NT       string
	Timeout  string
}

// subscriber is one subscription the Device holds.
type subscriber struct {
	sid      string
	callback string
	// next is the count its next event carries.
	next uint32
}

// sendTime is how long a subscriber has to take an event.
const sendTime = 5 * time.Second

// GENA's methods.
const (
	subscribe   = "SUBSCRIBE"
	unsubscribe = "UNSUBSCRIBE"
)

// Grant sets what the Device answers a subscription's length with, as
// the TIMEOUT header is written: "Second-1800" in a new Device,
// "Second-infinite", or nothing.
func (d *Device) Grant(timeout string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.grant = timeout
}

// Early makes the Device send a subscription's first event ahead of its
// answer to SUBSCRIBE, as a device may: the subscriber then holds an
// event of a subscription it does not know yet.
func (d *Device) Early(on bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.early = on
}

// Notify changes evented variables and tells every subscriber, each
// under its own count. It returns the HTTP status each answered, in the
// order they subscribed; zero for one that could not be reached.
func (d *Device) Notify(changed ...upnp.Argument) []int {
	d.mu.Lock()

	for _, change := range changed {
		d.set(change)
	}

	type due struct {
		to  subscriber
		seq uint32
	}

	sends := make([]due, 0, len(d.subscribers))

	for _, one := range d.subscribers {
		sends = append(sends, due{to: *one, seq: one.next})
		one.next = after(one.next)
	}

	d.mu.Unlock()

	statuses := make([]int, 0, len(sends))
	for _, one := range sends {
		statuses = append(
			statuses,
			d.deliver(one.to.callback, one.to.sid, strconv.FormatUint(uint64(one.seq), 10), propertySet(changed)),
		)
	}

	return statuses
}

// Send delivers anything at all to every subscriber, as an event under
// the count given and the body given: what no device should send. It
// returns the statuses, as Notify does, and moves no count on.
func (d *Device) Send(seq, body string) []int {
	d.mu.Lock()
	sends := make([]subscriber, 0, len(d.subscribers))

	for _, one := range d.subscribers {
		sends = append(sends, *one)
	}

	d.mu.Unlock()

	statuses := make([]int, 0, len(sends))
	for _, one := range sends {
		statuses = append(statuses, d.deliver(one.callback, one.sid, seq, body))
	}

	return statuses
}

// Skip moves every subscriber's count on by one with no event sent: an
// event that did not arrive.
func (d *Device) Skip() {
	d.mu.Lock()
	defer d.mu.Unlock()

	for _, one := range d.subscribers {
		one.next = after(one.next)
	}
}

// Forget drops every subscription without a word, as a device that
// restarted has: a renewal is then refused.
func (d *Device) Forget() {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.subscribers = nil
}

// Subscribers is how many subscriptions the Device holds.
func (d *Device) Subscribers() int {
	d.mu.Lock()
	defer d.mu.Unlock()

	return len(d.subscribers)
}

// Subscribed is every request about a subscription the Device was sent,
// in the order they came.
func (d *Device) Subscribed() []Subscription {
	d.mu.Lock()
	defer d.mu.Unlock()

	return append([]Subscription(nil), d.subscribed...)
}

// after is the count that follows: after the largest comes 1, since 0 is
// a subscription's first.
func after(seq uint32) uint32 {
	seq++
	if seq == 0 {
		return 1
	}

	return seq
}

// set changes one variable. Called with the lock held.
func (d *Device) set(change upnp.Argument) {
	for index := range d.state {
		if d.state[index].Name == change.Name {
			d.state[index].Value = change.Value

			return
		}
	}

	d.state = append(d.state, change)
}

// subscription answers SUBSCRIBE and UNSUBSCRIBE as GENA has a device do.
func (d *Device) subscription(writer http.ResponseWriter, request *http.Request) {
	asked := Subscription{
		Method: request.Method, Path: request.URL.Path,
		SID: request.Header.Get("Sid"), Callback: request.Header.Get("Callback"),
		NT: request.Header.Get("Nt"), Timeout: request.Header.Get("Timeout"),
	}

	d.mu.Lock()
	d.subscribed = append(d.subscribed, asked)
	grant, early := d.grant, d.early

	var (
		status  = http.StatusOK
		created *subscriber
		first   string
	)

	switch {
	case asked.SID != "" && (asked.Callback != "" || asked.NT != ""):
		status = http.StatusBadRequest
	case asked.SID != "":
		if !d.known(asked.SID, request.Method == unsubscribe) {
			status = http.StatusPreconditionFailed
		}
	case request.Method == unsubscribe, asked.NT != "upnp:event",
		!strings.HasPrefix(asked.Callback, "<"), !strings.HasSuffix(asked.Callback, ">"):
		status = http.StatusPreconditionFailed
	default:
		d.issued++
		created = &subscriber{
			sid: "uuid:subscription-" + strconv.Itoa(d.issued), callback: strings.Trim(asked.Callback, "<>"), next: 1,
		}
		d.subscribers = append(d.subscribers, created)
		first = propertySet(d.state)
	}

	d.mu.Unlock()

	if status != http.StatusOK {
		writer.WriteHeader(status)

		return
	}

	if created == nil {
		writer.Header().Set("Sid", asked.SID)
		setTimeout(writer, grant)
		writer.WriteHeader(http.StatusOK)

		return
	}

	// The first event tells how things stand, under the count 0.
	if early {
		//nolint:contextcheck // an event is the Device's to send, not the request's
		d.deliver(created.callback, created.sid, "0", first)
	}

	writer.Header().Set("Sid", created.sid)
	setTimeout(writer, grant)
	writer.WriteHeader(http.StatusOK)

	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}

	if !early {
		//nolint:contextcheck // an event sent after its answer outlives the request
		d.sending.Go(func() { d.deliver(created.callback, created.sid, "0", first) })
	}
}

func setTimeout(writer http.ResponseWriter, grant string) {
	if grant != "" {
		writer.Header().Set("Timeout", grant)
	}
}

// known says whether a subscription is held, and drops it when it is
// ending. Called with the lock held.
func (d *Device) known(sid string, ending bool) bool {
	for index, one := range d.subscribers {
		if one.sid != sid {
			continue
		}

		if ending {
			d.subscribers = append(d.subscribers[:index], d.subscribers[index+1:]...)
		}

		return true
	}

	return false
}

// deliver sends one event, and returns the status it was answered with.
func (d *Device) deliver(callback, sid, seq, body string) int {
	sending, cancel := context.WithTimeout(d.life, sendTime)
	defer cancel()

	request, err := http.NewRequestWithContext(sending, "NOTIFY", callback, strings.NewReader(body))
	if err != nil {
		return 0
	}

	request.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	request.Header["NT"] = []string{"upnp:event"}
	request.Header["NTS"] = []string{"upnp:propchange"}
	request.Header["SID"] = []string{sid}

	if seq != "" {
		request.Header["SEQ"] = []string{seq}
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0
	}

	_ = response.Body.Close()

	return response.StatusCode
}

// propertySet is an event's body, as the standard writes one.
func propertySet(properties upnp.Arguments) string {
	var body bytes.Buffer

	body.WriteString(`<?xml version="1.0"?><e:propertyset xmlns:e="urn:schemas-upnp-org:event-1-0">`)

	for _, property := range properties {
		body.WriteString("<e:property><" + property.Name + ">")
		_ = xml.EscapeText(&body, []byte(property.Value))
		body.WriteString("</" + property.Name + "></e:property>")
	}

	body.WriteString(`</e:propertyset>`)

	return body.String()
}
