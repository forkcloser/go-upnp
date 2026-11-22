package upnp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Event is one event of a service: the variables that changed, or every
// evented variable when it is the first.
type Event struct {
	// Seq is the device's count of it within the subscription: 0 is the
	// first, which tells how things stand. An event the device gave no
	// count is told with 0 too.
	Seq uint32
	// Properties is the variables it carries and what each now holds, in
	// the order the device wrote them.
	Properties Arguments
}

// Subscriber is a control point's part in eventing: the handler devices
// deliver events to, and the subscriptions made through it.
//
// It opens no socket. The caller serves it, on an HTTP server of its
// own and under any path, and tells each Watch the URL its device
// reaches that server at. One Subscriber serves every device, whatever
// interface each is reached on. The zero value is not one: NewSubscriber
// makes it.
//
// The server's timeouts are the caller's to set. The Subscriber gives an
// event's body ten seconds; a request that is no event, and a connection
// that sends nothing, are held only to the server's ReadTimeout and
// IdleTimeout, and for ever by a server that sets neither.
type Subscriber struct {
	mu sync.Mutex
	// held is the subscriptions being watched, each by the last element
	// of its callback's path.
	held map[string]*subscription
}

// NewSubscriber makes a Subscriber, with no subscription yet.
func NewSubscriber() *Subscriber {
	return &Subscriber{held: map[string]*subscription{}}
}

// What a subscription asks for and holds.
const (
	// lease is how long a subscription is asked for. The device grants
	// what it will, and the renewal goes by what it granted.
	lease = 30 * time.Minute
	// maxPending is how many events a Watch may leave untold before its
	// subscription is given up as lost.
	maxPending = 64
	// maxEvent is the most that is read of one event.
	maxEvent = 1 << 20
	// eventTime is how long a device has to send one event's body.
	eventTime = 10 * time.Second
	// parting is how long the device is given to take an unsubscription,
	// once the Watch is over.
	parting = 5 * time.Second
	// retry is the least time between two tries to renew a subscription
	// whose device cannot be reached.
	retry = time.Second
	// tokenBytes is the size of what makes a subscription's path its own.
	tokenBytes = 16
)

// subscription is one Watch's: what devices deliver to its path, until
// the Watch tells of it.
type subscription struct {
	// wake holds one token when there is something to take.
	wake chan struct{}

	mu sync.Mutex
	// sid is the subscription's identifier, once the device has answered
	// with it. An event that arrives sooner is kept under the one it
	// carries.
	sid     string
	pending []arrived
	// behind is set when more arrived than is held.
	behind bool
}

// arrived is an event as it was delivered, or what was delivered in the
// place of one and could not be read.
type arrived struct {
	sid   string
	event Event
	// counted says the device gave the event its count.
	counted bool
	// unread is why it could not be read.
	unread error
}

// ServeHTTP takes an event a device delivers, a NOTIFY, to the path of a
// subscription that is being watched. Anything else is answered as GENA
// has it: 412 for a subscription that is not held.
func (s *Subscriber) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != "NOTIFY" {
		writer.Header().Set("Allow", "NOTIFY")
		http.Error(writer, "NOTIFY only", http.StatusMethodNotAllowed)

		return
	}

	s.mu.Lock()
	held := s.held[request.URL.Path[strings.LastIndexByte(request.URL.Path, '/')+1:]]
	s.mu.Unlock()

	sid := request.Header.Get("Sid")
	if held == nil || sid == "" || !held.takes(sid) {
		http.Error(writer, "no such subscription", http.StatusPreconditionFailed)

		return
	}

	// The server is the caller's and so are its timeouts: this one is the
	// event's own, and a server that cannot set it keeps its own.
	_ = http.NewResponseController(writer).SetReadDeadline(time.Now().Add(eventTime))

	body, err := io.ReadAll(io.LimitReader(request.Body, maxEvent+1))

	var properties Arguments

	switch {
	case err != nil:
		err = fmt.Errorf("an event that did not arrive whole: %w", err)
	case len(body) > maxEvent:
		err = fmt.Errorf("%w: an event of more than %d bytes", ErrAnswer, maxEvent)
	default:
		if body, err = normal(body); err == nil {
			properties, err = propertySet(body)
		}
	}

	// An event that cannot be read is an event missed, of the
	// subscription it was sent under: it takes its place among the
	// others, and is dropped with them when it turns out another's.
	if err != nil {
		status := held.deliver(arrived{sid: sid, unread: fmt.Errorf("%w: %w", ErrLost, err)})
		if status == http.StatusOK {
			status = http.StatusBadRequest
		}

		http.Error(writer, "not an event", status)

		return
	}

	seq, seqErr := strconv.ParseUint(strings.TrimSpace(request.Header.Get("Seq")), 10, 32)

	writer.WriteHeader(held.deliver(arrived{
		sid: sid, counted: seqErr == nil, event: Event{Seq: uint32(seq), Properties: properties},
	}))
}

// Watch subscribes to a service's events and tells handle each one, in
// order, on the goroutine that called it: first how things stand, every
// evented variable, which is the device's first event; then every
// change. handle may take its time, and may call the device. Watch
// renews the subscription ahead of its end for as long as ctx lasts, and
// unsubscribes when it returns.
//
// callback is the URL this device reaches the Subscriber at, which only
// the caller knows: the server the Subscriber is served on, at this
// host's address on the network the device is on, and the path it is
// served under. The subscription's own path is added to it.
//
// Watch returns ctx's error when ctx ends, once it has unsubscribed: the
// device is given five seconds to take that, and as long again to answer
// when ctx ends before it has said which subscription it granted. Sooner,
// Watch returns an ErrRefused when the device takes no subscription, and
// an ErrLost when the subscription can be trusted no more: the device
// forgot it, an event did not arrive or could not be read, it could not
// be renewed in time, or handle fell more than sixty-four events behind.
// It does not subscribe again by itself: watching again does, and starts
// from how things stand.
func (s *Subscriber) Watch(
	ctx context.Context, client Doer, service Service, callback string, handle func(Event),
) error {
	if client == nil {
		return fmt.Errorf("%w: no client", ErrInvalid)
	}

	events, err := absolute(service.Events)
	if err != nil {
		return fmt.Errorf("the events URL of %s: %w", service.Type, err)
	}

	reached, err := absolute(callback)
	if err != nil {
		return fmt.Errorf("the callback: %w", err)
	}

	token := make([]byte, tokenBytes)
	_, _ = rand.Read(token)

	name := hex.EncodeToString(token)
	held := &subscription{wake: make(chan struct{}, 1)}

	s.mu.Lock()
	s.held[name] = held
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.held, name)
		s.mu.Unlock()
	}()

	watching := watch{client: client, events: events.String(), held: held}

	// A subscription asked for is seen through past ctx's end: one that has
	// already ended asks for none.
	if ctx.Err() != nil {
		return fmt.Errorf("subscribing to %s: %w", service.Type, context.Cause(ctx))
	}

	if err = watching.subscribe(ctx, reached.JoinPath(name).String()); err != nil {
		return fmt.Errorf("subscribing to %s: %w", service.Type, err)
	}

	defer watching.unsubscribe(ctx)

	// Granted as ctx ended: there is nothing to watch, and one to end.
	if ctx.Err() != nil {
		return fmt.Errorf("subscribing to %s: %w", service.Type, context.Cause(ctx))
	}

	return watching.run(ctx, handle)
}

// watch is one Watch at work.
type watch struct {
	client Doer
	events string
	held   *subscription
	sid    string
	// renew is when to renew, and ends when the subscription does.
	renew time.Time
	ends  time.Time
	// next is the count the next event has to carry.
	next uint32
}

// run tells of events and renews, until something ends it.
func (w *watch) run(ctx context.Context, handle func(Event)) error {
	timer := time.NewTimer(time.Until(w.renew))
	defer timer.Stop()

	for {
		events, behind := w.held.take()

		if err := w.tell(events, handle); err != nil {
			return err
		}

		if behind {
			return fmt.Errorf("%w: its handler is more than %d events behind", ErrLost, maxPending)
		}

		select {
		case <-w.held.wake:
		case <-ctx.Done():
			return fmt.Errorf("watching events: %w", context.Cause(ctx))
		case <-timer.C:
			if err := w.extend(ctx); err != nil {
				return err
			}

			timer.Reset(time.Until(w.renew))
		}
	}
}

// tell hands on what arrived, in its order, up to what could not be
// read or did not come in its turn.
func (w *watch) tell(events []arrived, handle func(Event)) error {
	for _, one := range events {
		if one.unread != nil {
			return one.unread
		}

		if err := w.inOrder(one); err != nil {
			return err
		}

		handle(one.event)
	}

	return nil
}

// inOrder holds an event to the count it has to carry: one that does not
// says another did not arrive. After the largest count comes 1, not 0.
func (w *watch) inOrder(one arrived) error {
	if !one.counted {
		return nil
	}

	if one.event.Seq != w.next {
		return fmt.Errorf("%w: event %d did not arrive, event %d did", ErrLost, w.next, one.event.Seq)
	}

	w.next++
	if w.next == 0 {
		w.next = 1
	}

	return nil
}

// subscribe asks the device for the subscription. The request outlasts
// ctx by parting: the device may have granted the subscription by the time
// ctx ends, and only its answer says which one there is to end.
func (w *watch) subscribe(ctx context.Context, callback string) error {
	asking, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()

	stop := context.AfterFunc(ctx, func() {
		select {
		case <-asking.Done():
		case <-time.After(parting):
			cancel()
		}
	})
	defer stop()

	request, err := http.NewRequestWithContext(asking, "SUBSCRIBE", w.events, http.NoBody)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	// Written as the standard writes them, which some devices require.
	request.Header["CALLBACK"] = []string{"<" + callback + ">"}
	request.Header["NT"] = []string{"upnp:event"}
	request.Header["TIMEOUT"] = []string{timeout(lease)}

	sid, err := w.ask(request)

	switch {
	case err != nil && ctx.Err() != nil:
		// Not answered in the time left to it: ctx's end is what ended it.
		return fmt.Errorf("the device had not answered: %w", context.Cause(ctx))
	case err != nil:
		return err
	case sid == "":
		return fmt.Errorf("%w: a subscription with no SID", ErrAnswer)
	}

	w.sid = sid
	w.held.granted(sid)

	return nil
}

// extend renews the subscription. A device that refuses has forgotten
// it; one that cannot be reached is asked again at half of what is left
// of the subscription, and no sooner than a second later, until what is
// left is too short to try in.
func (w *watch) extend(ctx context.Context) error {
	remaining := time.Until(w.ends)
	if remaining <= 0 {
		return fmt.Errorf("%w: it ran out before it could be renewed", ErrLost)
	}

	within, cancel := context.WithTimeout(ctx, remaining)
	defer cancel()

	request, err := http.NewRequestWithContext(within, "SUBSCRIBE", w.events, http.NoBody)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	request.Header["SID"] = []string{w.sid}
	request.Header["TIMEOUT"] = []string{timeout(lease)}

	_, err = w.ask(request)

	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return fmt.Errorf("watching events: %w", context.Cause(ctx))
	case errors.Is(err, ErrRefused), errors.Is(err, ErrAnswer):
		return fmt.Errorf("%w: the device did not renew it: %w", ErrLost, err)
	}

	// Not reached.
	w.renew = time.Now().Add(max(time.Until(w.ends)/2, retry))

	if !w.renew.Before(w.ends) {
		return fmt.Errorf("%w: it could not be renewed in what was left of it: %w", ErrLost, err)
	}

	return nil
}

// ask sends a SUBSCRIBE, new or renewing, and takes what the device
// granted: the SID, and how long, which sets when to renew.
func (w *watch) ask(request *http.Request) (string, error) {
	response, err := w.client.Do(request)
	if err != nil {
		return "", err //nolint:wrapcheck // the client's own error, as it is
	}

	defer func() { _ = response.Body.Close() }()

	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxEvent))

	// A subscription a client took out elsewhere is not this service's.
	if err = strayed(request.URL, response); err != nil {
		return "", err
	}

	if response.StatusCode != http.StatusOK {
		return "", &StatusError{Status: response.StatusCode}
	}

	now := time.Now()
	length := granted(response.Header.Get("Timeout"))
	w.ends = now.Add(length)
	w.renew = now.Add(length / 2)

	return strings.TrimSpace(response.Header.Get("Sid")), nil
}

// unsubscribe tells the device the subscription is over. The Watch is
// over whatever the device answers, and its context may have ended.
func (w *watch) unsubscribe(ctx context.Context) {
	parted, cancel := context.WithTimeout(context.WithoutCancel(ctx), parting)
	defer cancel()

	request, err := http.NewRequestWithContext(parted, "UNSUBSCRIBE", w.events, http.NoBody)
	if err != nil {
		return
	}

	request.Header["SID"] = []string{w.sid}

	response, err := w.client.Do(request)
	if err != nil {
		return
	}

	_ = response.Body.Close()
}

// timeout is a length of time as SUBSCRIBE writes it.
func timeout(length time.Duration) string {
	return "Second-" + strconv.Itoa(int(length/time.Second))
}

// granted is what a device granted: "Second-1800". What is no number of
// seconds, "Second-infinite" among it, is the length asked for, so that
// a subscription is renewed all the same.
func granted(header string) time.Duration {
	_, number, found := strings.Cut(header, "-")

	seconds, err := strconv.Atoi(strings.TrimSpace(number))
	if !found || err != nil || seconds <= 0 || seconds > maxLease {
		return lease
	}

	return time.Duration(seconds) * time.Second
}

// maxLease bounds what a device may grant, in seconds: a week.
const maxLease = 7 * 24 * 60 * 60

// takes says whether an event under that identifier is this
// subscription's: any, until the device has said which it is.
func (h *subscription) takes(sid string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.sid == "" || h.sid == sid
}

// granted records the identifier the device gave, and drops what
// arrived under another while it was not known.
func (h *subscription) granted(sid string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.sid = sid

	kept := h.pending[:0]

	for _, one := range h.pending {
		if one.sid == sid {
			kept = append(kept, one)
		}
	}

	h.pending = kept
}

// deliver keeps what arrived for the Watch, and returns the status to
// answer the device with. More than is held is refused; once the device
// has said which subscription this is, that is the subscription's loss,
// since the device may count what it could not deliver.
func (h *subscription) deliver(one arrived) int {
	h.mu.Lock()
	defer h.mu.Unlock()

	// The device may have said which subscription it is while the event
	// was read.
	if h.sid != "" && h.sid != one.sid {
		return http.StatusPreconditionFailed
	}

	if len(h.pending) >= maxPending {
		// Before the device has said so, what fills the path may be
		// anyone's: it is refused, and proves nothing.
		if h.sid != "" {
			h.behind = true

			h.signal()
		}

		return http.StatusServiceUnavailable
	}

	h.pending = append(h.pending, one)
	h.signal()

	return http.StatusOK
}

// signal wakes the Watch. Called with the lock held.
func (h *subscription) signal() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

// take is what arrived since the last, and whether more arrived than
// was held.
func (h *subscription) take() ([]arrived, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	taken := h.pending
	h.pending = nil

	return taken, h.behind
}

// propertySet reads an event's body: every variable it carries, in its
// order.
func propertySet(body []byte) (Arguments, error) {
	reading := decoder(body)

	set, err := child(reading)
	if err != nil || set == nil || !strings.EqualFold(set.Name.Local, "propertyset") {
		return nil, fmt.Errorf("%w: an event that is no property set", ErrAnswer)
	}

	var properties Arguments

	for {
		property, err := child(reading)
		if err != nil {
			return nil, err
		}

		if property == nil {
			return properties, nil
		}

		// One variable a property, as the standard has it, or as many as
		// the device wrote.
		variables, err := values(reading)
		if err != nil {
			return nil, err
		}

		properties = append(properties, variables...)
	}
}
