package upnp_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	upnp "github.com/forkcloser/go-upnp"
	"github.com/forkcloser/go-upnp/upnptest"
)

// served is a Subscriber on a server of the test's, and the URL devices
// reach it at.
func served(t *testing.T) (*upnp.Subscriber, string) {
	t.Helper()

	subscriber := upnp.NewSubscriber()
	server := httptest.NewServer(subscriber)
	t.Cleanup(server.Close)

	return subscriber, server.URL + "/events/"
}

// evented is the switch's events, written by hand.
func evented(fake *upnptest.Device) upnp.Service {
	return upnp.Service{Type: upnptest.SwitchType, Events: fake.URL() + upnptest.SwitchEvents}
}

// watched starts a Watch and returns what it tells, what it ends with,
// and how to end it.
func watched(
	t *testing.T, subscriber *upnp.Subscriber, fake *upnptest.Device, callback string,
) (chan upnp.Event, chan error, context.CancelFunc) {
	t.Helper()

	return watchedThrough(t, subscriber, fake.Client(), fake, callback)
}

// watchedThrough is watched, through a client of the test's choosing.
func watchedThrough(
	t *testing.T, subscriber *upnp.Subscriber, client upnp.Doer, fake *upnptest.Device, callback string,
) (chan upnp.Event, chan error, context.CancelFunc) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	events := make(chan upnp.Event, 256)
	done := make(chan error, 1)

	go func() {
		done <- subscriber.Watch(ctx, client, evented(fake), callback, func(event upnp.Event) { events <- event })
	}()

	t.Cleanup(cancel)

	return events, done, cancel
}

func next(t *testing.T, events chan upnp.Event, done chan error) upnp.Event {
	t.Helper()

	select {
	case event := <-events:
		return event
	case err := <-done:
		t.Fatalf("the Watch ended: %v", err)
	case <-time.After(wait):
		t.Fatal("no event")
	}

	return upnp.Event{}
}

func ended(t *testing.T, done chan error) error {
	t.Helper()

	select {
	case err := <-done:
		return err
	case <-time.After(wait):
		t.Fatal("the Watch did not end")
	}

	return nil
}

func status(name, value string) upnp.Argument { return upnp.Argument{Name: name, Value: value} }

// TestWatch: how things stand, then each change, in order; the
// subscription is asked for as the standard writes it, at a path of its
// own under the callback, and ended when the Watch is.
func TestWatch(t *testing.T) {
	t.Parallel()

	fake := device(t)
	subscriber, callback := served(t)
	events, done, cancel := watched(t, subscriber, fake, callback)

	first := next(t, events, done)
	if first.Seq != 0 || !slices.Equal(first.Properties, upnp.Arguments{status("Status", "0")}) {
		t.Fatalf("the first event = %+v", first)
	}

	if got := fake.Notify(status("Status", "1"), status("Level", `a <b> & "c"`)); !slices.Equal(got, []int{200}) {
		t.Fatalf("the subscriber answered an event with %v", got)
	}

	second := next(t, events, done)
	if second.Seq != 1 ||
		!slices.Equal(second.Properties, upnp.Arguments{status("Status", "1"), status("Level", `a <b> & "c"`)}) {
		t.Fatalf("the second event = %+v", second)
	}

	cancel()

	if err := ended(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("a Watch whose context ended = %v", err)
	}

	asked := fake.Subscribed()
	if len(asked) != 2 || fake.Subscribers() != 0 {
		t.Fatalf("the device was asked %+v and holds %d subscriptions", asked, fake.Subscribers())
	}

	own := strings.TrimSuffix(strings.TrimPrefix(asked[0].Callback, "<"+callback), ">")
	if asked[0].Method != "SUBSCRIBE" || asked[0].Path != upnptest.SwitchEvents || asked[0].NT != "upnp:event" ||
		asked[0].Timeout != "Second-1800" || asked[0].SID != "" || len(own) != 32 || strings.ContainsAny(own, "/<>") {
		t.Errorf("the subscription was asked for as %+v", asked[0])
	}

	if asked[1].Method != "UNSUBSCRIBE" || asked[1].SID != "uuid:subscription-1" || asked[1].Callback != "" {
		t.Errorf("the subscription was ended as %+v", asked[1])
	}

	// Its path is no longer one.
	if got := notify(
		t,
		"NOTIFY",
		strings.Trim(asked[0].Callback, "<>"),
		"uuid:subscription-1",
		"2",
		"<propertyset/>",
	); got != http.StatusPreconditionFailed {
		t.Errorf("an event after the Watch ended was answered %d, want 412", got)
	}
}

// TestAnotherSubscriptionsEventAhead: what arrives at a subscription's
// path before the device has said which subscription it is, and turns
// out to be another's, is not told.
func TestAnotherSubscriptionsEventAhead(t *testing.T) {
	t.Parallel()

	subscriber := upnp.NewSubscriber()
	body := func(value string) string {
		if value == "" {
			return "not an event"
		}

		return `<propertyset><property><Status>` + value + `</Status></property></propertyset>`
	}

	deliver := func(target, sid, value string) int {
		request := httptest.NewRequestWithContext(t.Context(), "NOTIFY", target, strings.NewReader(body(value)))
		request.Header.Set("Sid", sid)
		request.Header.Set("Seq", "0")

		answer := httptest.NewRecorder()
		subscriber.ServeHTTP(answer, request)

		return answer.Code
	}

	device := doer(func(request *http.Request) (*http.Response, error) {
		//nolint:staticcheck // SA1008: the header is read under the name it goes on the wire with
		if callback := request.Header["CALLBACK"]; len(callback) == 1 {
			target := strings.Trim(callback[0], "<>")

			// All are taken: nothing says yet whose the path is. What
			// cannot be read is answered as such, and what is more than
			// is held is refused.
			if unread := deliver(target, "uuid:another", ""); unread != http.StatusBadRequest {
				t.Errorf("what is no event, ahead of the answer, was answered %d", unread)
			}

			if first, second := deliver(
				target,
				"uuid:another",
				"theirs",
			), deliver(
				target,
				"uuid:1",
				"ours",
			); first != 200 ||
				second != 200 {
				t.Errorf("events ahead of the answer were answered %d and %d", first, second)
			}

			refused := 0

			for range 70 {
				if deliver(target, "uuid:another", "theirs") == http.StatusServiceUnavailable {
					refused++
				}
			}

			if refused != 9 {
				t.Errorf("%d events past what is held were refused, want 9", refused)
			}
		}

		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Sid": {"uuid:1"}},
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var told []string

	err := subscriber.Watch(ctx, device, upnp.Service{Type: upnptest.SwitchType, Events: "http://192.0.2.1/events"},
		"http://192.0.2.2/", func(event upnp.Event) {
			told = append(told, event.Properties[0].Value)

			cancel()
		})
	if !errors.Is(err, context.Canceled) || !slices.Equal(told, []string{"ours"}) {
		t.Fatalf("a Watch with another subscription's event ahead told %v and ended with %v", told, err)
	}
}

// TestTheFirstEventAheadOfTheAnswer: a device may send the first event
// before it has answered the subscription, which says whose it is: the
// event is told all the same.
func TestTheFirstEventAheadOfTheAnswer(t *testing.T) {
	t.Parallel()

	fake := device(t)
	fake.Early(true)

	subscriber, callback := served(t)
	events, done, _ := watched(t, subscriber, fake, callback)

	if first := next(t, events, done); first.Seq != 0 || len(first.Properties) != 1 {
		t.Fatalf("the first event = %+v", first)
	}

	fake.Notify(status("Status", "1"))

	if second := next(t, events, done); second.Seq != 1 {
		t.Fatalf("the second event = %+v", second)
	}
}

// TestRenewal: a subscription is renewed ahead of its end, by what the
// device granted; a device that has forgotten it refuses, and the Watch
// ends with the loss.
func TestRenewal(t *testing.T) {
	t.Parallel()

	fake := device(t)
	fake.Grant("Second-1")

	subscriber, callback := served(t)
	events, done, _ := watched(t, subscriber, fake, callback)

	next(t, events, done)

	renewed := func() int {
		count := 0

		for _, asked := range fake.Subscribed() {
			if asked.Method == "SUBSCRIBE" && asked.SID == "uuid:subscription-1" && asked.Callback == "" &&
				asked.NT == "" && asked.Timeout == "Second-1800" {
				count++
			}
		}

		return count
	}

	for deadline := time.Now().Add(wait); renewed() < 2; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the subscription was not renewed twice: %+v", fake.Subscribed())
		}
	}

	// Still the same subscription, and still told.
	fake.Notify(status("Status", "1"))

	if event := next(t, events, done); event.Seq != 1 {
		t.Fatalf("an event after a renewal = %+v", event)
	}

	fake.Forget()

	var refused *upnp.StatusError

	err := ended(t, done)
	// Said at once, and not after trying until the subscription ran out.
	if !errors.Is(err, upnp.ErrLost) || !errors.As(err, &refused) || refused.Status != http.StatusPreconditionFailed ||
		!strings.Contains(err.Error(), "did not renew it") {
		t.Fatalf("a Watch whose subscription the device forgot = %v, want ErrLost with 412", err)
	}
}

// errGone is a device that cannot be reached.
var errGone = errors.New("no route to the device")

// TestADeviceThatGoesAway: a subscription whose device cannot be
// reached is asked for again at half of what is left of it, no sooner
// than a second later, and is lost when what is left is too short.
func TestADeviceThatGoesAway(t *testing.T) {
	t.Parallel()

	fake := device(t)
	fake.Grant("Second-6")

	var (
		counting sync.Mutex
		asked    []time.Time
		gone     atomic.Bool
	)

	// Out of reach at once, with no dial: how long a refused connection
	// takes is the system's, and not what is measured here.
	client := doer(func(request *http.Request) (*http.Response, error) {
		if !gone.Load() {
			return fake.Client().Do(request)
		}

		//nolint:staticcheck // SA1008: the header is read under the name it goes on the wire with
		if request.Method == "SUBSCRIBE" && len(request.Header["SID"]) == 1 {
			counting.Lock()

			asked = append(asked, time.Now())
			counting.Unlock()
		}

		return nil, errGone
	})

	subscriber, callback := served(t)
	events, done, _ := watchedThrough(t, subscriber, client, fake, callback)

	next(t, events, done)
	gone.Store(true)

	err := ended(t, done)
	if !errors.Is(err, upnp.ErrLost) || !errors.Is(err, errGone) || errors.Is(err, upnp.ErrRefused) ||
		!strings.Contains(err.Error(), "what was left of it") {
		t.Fatalf("a Watch whose device went away = %v, want ErrLost with why, and not ErrRefused", err)
	}

	counting.Lock()
	defer counting.Unlock()

	// At three seconds, half of the three that are left later, and a
	// second after that; then half a second is left, which is too short.
	// A timer may be late and is never early.
	const second, third = 1250 * time.Millisecond, 900 * time.Millisecond

	if len(asked) < 2 || len(asked) > 3 || asked[1].Sub(asked[0]) < second ||
		len(asked) == 3 && asked[2].Sub(asked[1]) < third {
		t.Errorf("asked to renew at %v", asked)
	}
}

// TestAHandlerThatOutlastsItsSubscription: a subscription that ran out
// while the handler was at work is lost, not renewed late.
func TestAHandlerThatOutlastsItsSubscription(t *testing.T) {
	t.Parallel()

	fake := device(t)
	fake.Grant("Second-1")

	subscriber, callback := served(t)

	// Bounded: renewed late, it would be watched for good.
	ctx, cancel := context.WithTimeout(t.Context(), wait)
	defer cancel()

	err := subscriber.Watch(ctx, fake.Client(), evented(fake), callback, func(upnp.Event) {
		time.Sleep(1500 * time.Millisecond)
	})
	if !errors.Is(err, upnp.ErrLost) || !strings.Contains(err.Error(), "ran out before") {
		t.Fatalf("a Watch whose handler outlasted its subscription = %v", err)
	}
}

// TestARenewalThatStrays: a renewal a client took elsewhere is the
// subscription's loss, said at once.
func TestARenewalThatStrays(t *testing.T) {
	t.Parallel()

	fake, elsewhere := device(t), device(t)
	fake.Grant("Second-1")

	subscriber, callback := served(t)
	events, done, _ := watchedThrough(t, subscriber, &http.Client{}, fake, callback)

	next(t, events, done)
	fake.Redirect(upnptest.SwitchEvents, http.StatusTemporaryRedirect,
		strings.Replace(elsewhere.URL(), "127.0.0.1", "localhost", 1)+upnptest.SwitchEvents)

	err := ended(t, done)
	if !errors.Is(err, upnp.ErrLost) || !errors.Is(err, upnp.ErrAnswer) ||
		!strings.Contains(err.Error(), "did not renew it") {
		t.Fatalf("a Watch whose renewal strayed = %v", err)
	}
}

// TestAnEventThatDidNotArrive: a gap in the device's count is a loss,
// and the device is told the subscription is over.
func TestAnEventThatDidNotArrive(t *testing.T) {
	t.Parallel()

	fake := device(t)
	subscriber, callback := served(t)
	events, done, _ := watched(t, subscriber, fake, callback)

	next(t, events, done)
	fake.Skip()
	fake.Notify(status("Status", "1"))

	if err := ended(
		t,
		done,
	); !errors.Is(err, upnp.ErrLost) ||
		!strings.Contains(err.Error(), "event 1 did not arrive") {
		t.Fatalf("a Watch that missed an event = %v", err)
	}

	select {
	case event := <-events:
		t.Errorf("the event past the gap was told: %+v", event)
	default:
	}

	if fake.Subscribers() != 0 {
		t.Error("the device still holds the subscription")
	}
}

// TestAHandlerBehind: a handler may take its time, and events wait for
// it, up to sixty-four: past that the device is refused and the Watch
// ends with the loss, having told what it held.
func TestAHandlerBehind(t *testing.T) {
	t.Parallel()

	fake := device(t)
	subscriber, callback := served(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	entered, release := make(chan struct{}, 1), make(chan struct{})
	told := 0
	done := make(chan error, 1)

	go func() {
		done <- subscriber.Watch(ctx, fake.Client(), evented(fake), callback, func(upnp.Event) {
			told++

			select {
			case entered <- struct{}{}:
			default:
			}

			<-release
		})
	}()

	select {
	case <-entered:
	case <-time.After(wait):
		t.Fatal("the first event was not told")
	}

	refused := 0

	for range 70 {
		if got := fake.Notify(status("Status", "1")); !slices.Equal(got, []int{200}) {
			if !slices.Equal(got, []int{503}) {
				t.Fatalf("an event past what is held was answered %v", got)
			}

			refused++
		}
	}

	close(release)

	err := ended(t, done)
	if !errors.Is(err, upnp.ErrLost) || !strings.Contains(err.Error(), "behind") || refused != 6 || told != 65 {
		t.Fatalf("a Watch far behind = %v, after %d events told and %d refused; want 65 and 6", err, told, refused)
	}
}

// TestASubscriptionRefused: a device that takes no subscription refuses
// with its status; one that grants none it names is no answer.
func TestASubscriptionRefused(t *testing.T) {
	t.Parallel()

	fake := device(t)
	subscriber, callback := served(t)
	nothing := func(upnp.Event) { t.Error("an event was told") }

	fake.Serve(upnptest.SwitchEvents, http.StatusServiceUnavailable, "")

	var refused *upnp.StatusError

	err := subscriber.Watch(t.Context(), fake.Client(), evented(fake), callback, nothing)
	if !errors.Is(err, upnp.ErrRefused) || !errors.As(err, &refused) ||
		refused.Status != http.StatusServiceUnavailable ||
		errors.Is(err, upnp.ErrLost) {
		t.Errorf("a subscription refused = %v", err)
	}

	fake.Serve(upnptest.SwitchEvents, http.StatusOK, "")

	if err = subscriber.Watch(
		t.Context(),
		fake.Client(),
		evented(fake),
		callback,
		nothing,
	); !errors.Is(
		err,
		upnp.ErrAnswer,
	) {
		t.Errorf("a subscription granted with no SID = %v, want ErrAnswer", err)
	}

	// A subscription a client that follows redirects took out at another
	// host is not this service's; through one that follows none, the
	// redirect is the refusal.
	elsewhere := device(t)
	fake.Redirect(upnptest.SwitchEvents, http.StatusTemporaryRedirect,
		strings.Replace(elsewhere.URL(), "127.0.0.1", "localhost", 1)+upnptest.SwitchEvents)

	// Bounded: taken for this service's, such a subscription would be
	// watched for good.
	bounded, stop := context.WithTimeout(t.Context(), wait/6)
	defer stop()

	err = subscriber.Watch(bounded, &http.Client{}, evented(fake), callback, func(upnp.Event) {})
	if !errors.Is(err, upnp.ErrAnswer) {
		t.Errorf("a subscription taken out at another host = %v, want ErrAnswer", err)
	}

	err = subscriber.Watch(t.Context(), fake.Client(), evented(fake), callback, nothing)
	if !errors.As(err, &refused) || refused.Status != http.StatusTemporaryRedirect {
		t.Errorf("a subscription that redirects, through a client that follows none = %v", err)
	}

	for name, call := range map[string]struct {
		client   upnp.Doer
		service  upnp.Service
		callback string
	}{
		"no client":                {nil, evented(fake), callback},
		"a service with no events": {fake.Client(), upnp.Service{Type: upnptest.SwitchType}, callback},
		"no callback":              {fake.Client(), evented(fake), ""},
		"a callback with no host":  {fake.Client(), evented(fake), "/events/"},
	} {
		if err = subscriber.Watch(
			t.Context(),
			call.client,
			call.service,
			call.callback,
			nothing,
		); !errors.Is(
			err,
			upnp.ErrInvalid,
		) {
			t.Errorf("Watch with %s = %v, want ErrInvalid", name, err)
		}
	}

	ended, cancel := context.WithCancel(t.Context())
	cancel()

	fake.Serve(upnptest.SwitchEvents, 0, "")

	if err = subscriber.Watch(
		ended,
		fake.Client(),
		evented(fake),
		callback,
		nothing,
	); !errors.Is(
		err,
		context.Canceled,
	) {
		t.Errorf("Watch under a context that ended = %v", err)
	}
}

// notify delivers what a test wants to a path, as a device would.
func notify(t *testing.T, method, target, sid, seq, body string) int {
	t.Helper()

	request, err := http.NewRequestWithContext(t.Context(), method, target, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	request.Header.Set("Sid", sid)
	request.Header.Set("Seq", seq)

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = response.Body.Close() }()

	_, _ = io.Copy(io.Discard, response.Body)

	return response.StatusCode
}

// TestWhatIsNoEvent: what is not an event of a subscription that is
// watched is refused as GENA has it, and the Watch goes on; an event of
// one that cannot be read ends it.
func TestWhatIsNoEvent(t *testing.T) {
	t.Parallel()

	fake := device(t)
	subscriber, callback := served(t)
	events, done, _ := watched(t, subscriber, fake, callback)

	next(t, events, done)

	own := strings.Trim(fake.Subscribed()[0].Callback, "<>")
	body := `<e:propertyset xmlns:e="urn:schemas-upnp-org:event-1-0"><e:property><Status>1</Status></e:property></e:propertyset>`

	for name, sent := range map[string]struct {
		method, target, sid string
		want                int
	}{
		"another method":           {http.MethodPost, own, "uuid:subscription-1", http.StatusMethodNotAllowed},
		"a path that is no one's":  {"NOTIFY", callback + "0123456789abcdef0123456789abcdef", "uuid:subscription-1", http.StatusPreconditionFailed},
		"the callback itself":      {"NOTIFY", callback, "uuid:subscription-1", http.StatusPreconditionFailed},
		"another subscription's":   {"NOTIFY", own, "uuid:subscription-9", http.StatusPreconditionFailed},
		"no subscription's at all": {"NOTIFY", own, "", http.StatusPreconditionFailed},
	} {
		if got := notify(t, sent.method, sent.target, sent.sid, "1", body); got != sent.want {
			t.Errorf("%s was answered %d, want %d", name, got, sent.want)
		}
	}

	// An event with no count is told, and holds the count to nothing.
	if got := fake.Send("", body); !slices.Equal(got, []int{200}) {
		t.Fatalf("an event with no count was answered %v", got)
	}

	if event := next(t, events, done); !slices.Equal(event.Properties, upnp.Arguments{status("Status", "1")}) {
		t.Fatalf("an event with no count = %+v", event)
	}

	// As devices write them: any case, a property with two variables, one
	// with none, a document sent bare.
	if got := fake.Send(
		"1",
		"\xef\xbb\xbf"+`<PROPERTYSET><PROPERTY><A>1</A><B><x/></B></PROPERTY><property/></PROPERTYSET>`,
	); !slices.Equal(
		got,
		[]int{200},
	) {
		t.Fatalf("an event as devices write them was answered %v", got)
	}

	if event := next(
		t,
		events,
		done,
	); !slices.Equal(
		event.Properties,
		upnp.Arguments{status("A", "1"), status("B", "<x/>")},
	) {
		t.Fatalf("an event as devices write them = %+v", event)
	}

	// What cannot be read ends the Watch: an event was missed.
	for name, sent := range map[string]string{
		"no XML":             "not an event",
		"another document":   `<other><x><A>1</A></x></other>`,
		"cut short":          `<propertyset><property><A>`,
		"an encoding unread": `<?xml version="1.0" encoding="utf-16"?><propertyset/>`,
	} {
		other := device(t)
		told, over, _ := watched(t, subscriber, other, callback)

		next(t, told, over)

		if got := other.Send("1", sent); !slices.Equal(got, []int{400}) {
			t.Errorf("%s was answered %v", name, got)
		}

		if err := ended(t, over); !errors.Is(err, upnp.ErrLost) || !errors.Is(err, upnp.ErrAnswer) {
			t.Errorf("a Watch sent %s = %v, want ErrLost and ErrAnswer", name, err)
		}
	}
}

// TestAnEventTooLarge: an event of a mebibyte is told, and one of a byte
// more ends the Watch.
func TestAnEventTooLarge(t *testing.T) {
	t.Parallel()

	fake := device(t)
	subscriber, callback := served(t)
	events, done, _ := watched(t, subscriber, fake, callback)

	next(t, events, done)

	const around = `<propertyset><property><A></A></property></propertyset>`

	sized := func(size int) string {
		return strings.Replace(around, "<A>", "<A>"+strings.Repeat("a", size-len(around)), 1)
	}

	if got := fake.Send("1", sized(1<<20)); !slices.Equal(got, []int{200}) {
		t.Fatalf("an event of a mebibyte was answered %v", got)
	}

	if event := next(
		t,
		events,
		done,
	); len(event.Properties) != 1 ||
		len(event.Properties[0].Value) != 1<<20-len(around) {
		t.Fatalf("an event of a mebibyte was told as %d properties", len(event.Properties))
	}

	if got := fake.Send("2", sized(1<<20+1)); !slices.Equal(got, []int{400}) {
		t.Fatalf("an event too large was answered %v", got)
	}

	if err := ended(t, done); !errors.Is(err, upnp.ErrLost) || !errors.Is(err, upnp.ErrAnswer) {
		t.Fatalf("a Watch sent an event too large = %v, want ErrLost and ErrAnswer", err)
	}
}

// TestOneSubscriberSeveralDevices: one Subscriber, under one server,
// serves several devices and several watches of one, each told its own.
func TestOneSubscriberSeveralDevices(t *testing.T) {
	t.Parallel()

	one, other := device(t), device(t)
	subscriber, callback := served(t)

	first, firstDone, _ := watched(t, subscriber, one, callback)
	second, secondDone, _ := watched(t, subscriber, other, callback+"elsewhere")
	third, thirdDone, _ := watched(t, subscriber, other, callback)

	next(t, first, firstDone)
	next(t, second, secondDone)
	next(t, third, thirdDone)

	if got := other.Notify(status("Status", "other")); !slices.Equal(got, []int{200, 200}) {
		t.Fatalf("two watches of one device answered %v", got)
	}

	one.Notify(status("Status", "one"))

	for name, told := range map[string]struct {
		events chan upnp.Event
		done   chan error
		want   string
	}{"the first": {first, firstDone, "one"}, "the second": {second, secondDone, "other"}, "the third": {third, thirdDone, "other"}} {
		if event := next(t, told.events, told.done); event.Seq != 1 || event.Properties[0].Value != told.want {
			t.Errorf("%s was told %+v, want %s", name, event, told.want)
		}
	}
}

// TestWhatADeviceGrants: a length that is no number of seconds is one
// all the same.
func TestWhatADeviceGrants(t *testing.T) {
	t.Parallel()

	for _, grant := range []string{
		"Second-infinite", "", "Second--5", "Second-99999999999", "1800", "Second-0", "Second-9223372036854775807",
	} {
		fake := device(t)
		fake.Grant(grant)

		subscriber, callback := served(t)
		events, done, cancel := watched(t, subscriber, fake, callback)

		next(t, events, done)
		fake.Notify(status("Status", "1"))

		if event := next(t, events, done); event.Seq != 1 {
			t.Errorf("granted %q: the second event = %+v", grant, event)
		}

		cancel()

		if err := ended(t, done); !errors.Is(err, context.Canceled) {
			t.Errorf("granted %q: the Watch ended with %v", grant, err)
		}
	}
}
