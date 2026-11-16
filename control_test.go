package upnp_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	upnp "github.com/forkcloser/go-upnp"
	"github.com/forkcloser/go-upnp/upnptest"
)

// power is the switch of a test device, written by hand as a program
// that reads no description writes it.
func power(fake *upnptest.Device) upnp.Service {
	return upnp.Service{
		Type: upnptest.SwitchType, Control: fake.URL() + upnptest.SwitchControl, SCPD: fake.URL() + upnptest.SwitchSCPD,
	}
}

// TestServiceDescription: a service's actions, each with what goes in
// and what comes out in the order written, and its variables.
func TestServiceDescription(t *testing.T) {
	t.Parallel()

	fake := device(t)

	described, err := power(fake).Describe(t.Context(), fake.Client())
	if err != nil {
		t.Fatal(err)
	}

	both, ok := described.Action("SetBoth")
	if !ok || len(described.Actions) != 3 ||
		!slices.Equal(
			both.In,
			[]upnp.Parameter{{Name: "First", Variable: "Target"}, {Name: "Second", Variable: "Level"}},
		) ||
		!slices.Equal(both.Out, []upnp.Parameter{{Name: "Was", Variable: "Status"}}) {
		t.Errorf("SetBoth = %+v, %t, of %d actions", both, ok, len(described.Actions))
	}

	if _, ok = described.Action("setboth"); ok {
		t.Error("an action was found under a name in another case")
	}

	// A direction and a sendEvents in capitals; an action listed twice is
	// the first.
	loud, err := power(fake).Describe(t.Context(), said(`<scpd><actionList>`+
		`<action><name>A</name><argumentList><argument><name>x</name><direction>OUT</direction></argument></argumentList></action>`+
		`<action><name>A</name></action></actionList>`+
		`<serviceStateTable><stateVariable sendEvents="NO"><name>v</name></stateVariable></serviceStateTable></scpd>`))
	if err != nil {
		t.Fatal(err)
	}

	if first, _ := loud.Action(
		"A",
	); len(first.Out) != 1 || len(first.In) != 0 || len(loud.Variables) != 1 ||
		loud.Variables[0].Evented {
		t.Errorf("a description in capitals = %+v", loud)
	}

	type variable struct {
		name, kind, initial, allowed, bounds string
		evented                              bool
	}

	var got []variable
	for _, one := range described.Variables {
		got = append(got, variable{
			one.Name, one.Type, one.Default, strings.Join(one.Allowed, ","),
			one.Minimum + ".." + one.Maximum + "/" + one.Step, one.Evented,
		})
	}

	want := []variable{
		{"Target", "boolean", "0", "", "../", false},
		{"Status", "boolean", "", "", "../", true},
		// Evented unless it says no.
		{"Level", "ui1", "", "", "0..100/5", true},
		{"Mode", "string", "", "Day,Night", "../", false},
	}
	if !slices.Equal(got, want) {
		t.Errorf("the variables = %+v\nwant %+v", got, want)
	}

	// An argument with no direction goes in: it still has to be sent.
	fake.Serve(upnptest.SwitchSCPD, http.StatusOK,
		strings.Replace(upnptest.SwitchDescription, "<direction>in</direction>", "", 1))

	described, err = power(fake).Describe(t.Context(), fake.Client())
	if err != nil {
		t.Fatal(err)
	}

	if set, _ := described.Action("SetTarget"); len(set.In) != 1 || len(set.Out) != 0 {
		t.Errorf("an argument with no direction = %+v", set)
	}

	fake.Serve(upnptest.SwitchSCPD, http.StatusOK, upnptest.Description)

	if _, err = power(fake).Describe(t.Context(), fake.Client()); !errors.Is(err, upnp.ErrAnswer) {
		t.Errorf("a device's description for a service's = %v, want ErrAnswer", err)
	}

	fake.Serve(upnptest.SwitchSCPD, http.StatusServiceUnavailable, "")

	if _, err = power(fake).Describe(t.Context(), fake.Client()); !errors.Is(err, upnp.ErrRefused) {
		t.Errorf("a description refused = %v, want ErrRefused", err)
	}

	if _, err = (upnp.Service{Type: upnptest.SwitchType}).Describe(
		t.Context(),
		fake.Client(),
	); !errors.Is(
		err,
		upnp.ErrInvalid,
	) {
		t.Errorf("Describe of a service with no description URL = %v, want ErrInvalid", err)
	}
}

// TestCall: an action goes out under the service's type with its
// arguments in the order given and their text escaped, and what the
// device answers comes back in the device's order.
func TestCall(t *testing.T) {
	t.Parallel()

	fake := device(t)
	fake.Answer("SetBoth", func(in upnp.Arguments) (upnp.Arguments, error) {
		first, _ := in.Get("First")

		return upnp.Arguments{
			{Name: "Was", Value: first + ` <&> "was"`},
			{Name: "Was", Value: "twice"},
			{Name: "Empty"},
		}, nil
	})

	out, err := power(fake).Call(t.Context(), fake.Client(), "SetBoth",
		upnp.Argument{Name: "Second", Value: `a <b> & "c"` + "\n"}, upnp.Argument{Name: "First", Value: "1"})
	if err != nil {
		t.Fatal(err)
	}

	want := upnp.Arguments{{Name: "Was", Value: `1 <&> "was"`}, {Name: "Was", Value: "twice"}, {Name: "Empty"}}
	if !slices.Equal(out, want) {
		t.Errorf("the answer = %+v, want %+v", out, want)
	}

	if value, ok := out.Get("Was"); !ok || value != `1 <&> "was"` {
		t.Errorf("Get of a name answered twice = %q, %t; want the first", value, ok)
	}

	if _, ok := out.Get("was"); ok {
		t.Error("Get found a value under a name in another case")
	}

	calls := fake.Calls()
	sent := upnp.Arguments{{Name: "Second", Value: `a <b> & "c"` + "\n"}, {Name: "First", Value: "1"}}

	if len(calls) != 1 || calls[0].Path != upnptest.SwitchControl || calls[0].Type != upnptest.SwitchType ||
		calls[0].Action != "SetBoth" || !slices.Equal(calls[0].In, sent) ||
		calls[0].SOAPAction != `"`+upnptest.SwitchType+`#SetBoth"` || calls[0].ContentType != `text/xml; charset="utf-8"` {
		t.Errorf("what the device was asked = %+v", calls)
	}
}

// TestTheRequestOnTheWire: the header is written as the standard writes
// it, and the body goes with its length, not in chunks.
func TestTheRequestOnTheWire(t *testing.T) {
	t.Parallel()

	var listening net.ListenConfig

	listener, err := listening.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = listener.Close() })

	head := make(chan string, 1)

	go func() {
		conn, gone := listener.Accept()
		if gone != nil {
			return
		}

		defer func() { _ = conn.Close() }()

		reader := bufio.NewReader(conn)

		var (
			lines  []string
			length int
		)

		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil || strings.TrimSpace(line) == "" {
				break
			}

			if value, found := strings.CutPrefix(line, "Content-Length: "); found {
				length, _ = strconv.Atoi(strings.TrimSpace(value))
			}

			lines = append(lines, strings.TrimSpace(line))
		}

		_, _ = io.CopyN(io.Discard, reader, int64(length))

		head <- strings.Join(lines, "\n")

		_, _ = io.WriteString(conn, "HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n")
	}()

	service := upnp.Service{Type: upnptest.SwitchType, Control: "http://" + listener.Addr().String() + "/control"}

	// A success that says nothing is no answer.
	if _, err = service.Call(t.Context(), &http.Client{}, "GetStatus"); !errors.Is(err, upnp.ErrAnswer) {
		t.Errorf("Call answered by nothing = %v, want ErrAnswer", err)
	}

	sent := <-head
	for _, wanted := range []string{
		"POST /control HTTP/1.1", `SOAPACTION: "` + upnptest.SwitchType + `#GetStatus"`,
		`Content-Type: text/xml; charset="utf-8"`, "Content-Length: ",
	} {
		if !strings.Contains(sent, wanted) {
			t.Errorf("the request's head lacks %q:\n%s", wanted, sent)
		}
	}

	if strings.Contains(sent, "Transfer-Encoding") {
		t.Errorf("the request went in chunks:\n%s", sent)
	}
}

// TestARefusal: a fault is an ErrRefused that carries the device's code,
// under 500 and under 200 alike; a status that is no success and no
// fault is one that carries the status.
func TestARefusal(t *testing.T) {
	t.Parallel()

	fake := device(t)
	client := fake.Client()

	var (
		refused *upnp.FaultError
		status  *upnp.StatusError
	)

	// No action of that name: the device's own 401.
	_, err := power(fake).Call(t.Context(), client, "Nothing")
	if !errors.Is(err, upnp.ErrRefused) || !errors.As(err, &refused) ||
		*refused != (upnp.FaultError{Code: 401, Description: "Invalid Action", Status: 500}) || errors.As(err, &status) {
		t.Errorf("an action the device does not have = %v (%+v)", err, refused)
	}

	fake.Answer("GetStatus", func(upnp.Arguments) (upnp.Arguments, error) {
		return nil, &upnp.FaultError{Code: 714, Description: "No <such> thing", Status: http.StatusOK}
	})

	_, err = power(fake).Call(t.Context(), client, "GetStatus")
	if !errors.As(err, &refused) ||
		*refused != (upnp.FaultError{Code: 714, Description: "No <such> thing", Status: 200}) {
		t.Errorf("a fault under 200 = %v (%+v)", err, refused)
	}

	if refused == nil {
		t.Fatal("no fault")
	}

	if text := refused.Error(); text != "the device refused: UPnP error 714, No <such> thing" {
		t.Errorf("a fault reads %q", text)
	}

	if text := (&upnp.FaultError{Code: 501}).Error(); text != "the device refused: UPnP error 501" {
		t.Errorf("a fault with no description reads %q", text)
	}

	if text := (&upnp.StatusError{Status: http.StatusNotFound}).Error(); text != "the device refused: HTTP 404 Not Found" {
		t.Errorf("a status reads %q", text)
	}

	fake.Answer("GetStatus", func(upnp.Arguments) (upnp.Arguments, error) { return nil, io.ErrUnexpectedEOF })

	_, err = power(fake).Call(t.Context(), client, "GetStatus")
	if !errors.As(err, &refused) || refused.Code != 501 || !strings.Contains(err.Error(), "GetStatus") {
		t.Errorf("an action that failed = %v", err)
	}

	for name, answer := range map[string]struct {
		status int
		body   string
		code   int
	}{
		"a fault with no code":       {500, `<s:Envelope xmlns:s="x"><s:Body><s:Fault><faultstring>no</faultstring></s:Fault></s:Body></s:Envelope>`, 0},
		"a fault in any case":        {500, `<Envelope><BODY><FAULT><DETAIL><UPNPERROR><ERRORCODE> 402 </ERRORCODE><ERRORDESCRIPTION> trimmed </ERRORDESCRIPTION></UPNPERROR></DETAIL></FAULT></BODY></Envelope>`, 402},
		"a fault cut short":          {500, `<Envelope><Body><Fault><detail><UPnPError><errorCode>603</errorCode>`, 603},
		"a fault whose code is none": {500, `<Envelope><Body><Fault><detail><UPnPError><errorCode>many</errorCode></UPnPError></detail></Fault></Body></Envelope>`, 0},
	} {
		fake.Serve(upnptest.SwitchControl, answer.status, answer.body)

		_, err = power(fake).Call(t.Context(), client, "GetStatus")
		if !errors.Is(err, upnp.ErrRefused) || !errors.As(err, &refused) || refused.Code != answer.code ||
			refused.Status != answer.status {
			t.Errorf("%s = %v (%+v)", name, err, refused)
		}

		if name == "a fault in any case" && (refused == nil || refused.Description != "trimmed") {
			t.Errorf("a description with space around it = %q", refused.Description)
		}
	}

	for name, answer := range map[string]struct {
		status int
		body   string
	}{
		"forbidden, and no XML":         {http.StatusForbidden, "Forbidden"},
		"an error, and nothing":         {http.StatusInternalServerError, ""},
		"an error, and another answer":  {http.StatusBadGateway, `<Envelope><Body><Other/></Body></Envelope>`},
		"a redirect the client left be": {http.StatusNotModified, ""},
	} {
		fake.Serve(upnptest.SwitchControl, answer.status, answer.body)

		_, err = power(fake).Call(t.Context(), client, "GetStatus")
		if !errors.Is(err, upnp.ErrRefused) || !errors.As(err, &status) || status.Status != answer.status {
			t.Errorf("%s = %v", name, err)
		}
	}
}

// TestAnswersDevicesWrite: an answer is read as devices write them.
func TestAnswersDevicesWrite(t *testing.T) {
	t.Parallel()

	fake := device(t)

	for name, answer := range map[string]struct {
		body string
		want upnp.Arguments
	}{
		"no values": {
			`<s:Envelope xmlns:s="x"><s:Body><u:GetStatusResponse xmlns:u="y"/></s:Body></s:Envelope>`, nil,
		},
		"a body in any case, and another response's name": {
			`<ENVELOPE><BODY><Whatever><A>1</A></Whatever></BODY></ENVELOPE>`, upnp.Arguments{{Name: "A", Value: "1"}},
		},
		"a byte order mark and a header": {
			"\xef\xbb\xbf" + `<?xml version="1.0"?><e><Header><x/></Header><Body><r><A>1</A></r></Body></e>`,
			upnp.Arguments{{Name: "A", Value: "1"}},
		},
		"a document escaped, and one in CDATA": {
			`<e><Body><r><A>&lt;x a="1"&gt;&amp;&lt;/x&gt;</A><B><![CDATA[<y/>]]></B></r></Body></e>`,
			upnp.Arguments{{Name: "A", Value: `<x a="1">&</x>`}, {Name: "B", Value: "<y/>"}},
		},
		"a document sent bare": {
			`<e><Body><r><A><x a="1">t &amp; u</x><y/></A><B> kept </B></r></Body></e>`,
			upnp.Arguments{{Name: "A", Value: `<x a="1">t &amp; u</x><y/>`}, {Name: "B", Value: " kept "}},
		},
		"whatever follows": {
			`<e><Body><r><A>1</A></r></Body></e>` + "\x00 trailing", upnp.Arguments{{Name: "A", Value: "1"}},
		},
	} {
		fake.Serve(upnptest.SwitchControl, http.StatusOK, answer.body)

		out, err := power(fake).Call(t.Context(), fake.Client(), "GetStatus")
		if err != nil || !slices.Equal(out, answer.want) {
			t.Errorf("%s = %+v, %v; want %+v", name, out, err, answer.want)
		}
	}

	for name, body := range map[string]string{
		"no XML":             "OK",
		"no body":            `<e><Header/></e>`,
		"an empty body":      `<e><Body></Body></e>`,
		"cut short":          `<e><Body><r><A>1`,
		"cut in a value":     `<e><Body><r><A><x>`,
		"more than is read":  `<e><Body><r><A>` + strings.Repeat("a", 4<<20) + `</A></r></Body></e>`,
		"an encoding unread": `<?xml version="1.0" encoding="utf-16"?><e><Body><r/></Body></e>`,
	} {
		fake.Serve(upnptest.SwitchControl, http.StatusOK, body)

		if out, err := power(fake).Call(t.Context(), fake.Client(), "GetStatus"); !errors.Is(err, upnp.ErrAnswer) {
			t.Errorf("%s = %+v, %v; want ErrAnswer", name, out, err)
		}
	}
}

// TestWhatIsNotSent: what cannot be said is refused before anything is
// sent, so that no name writes markup into a request.
func TestWhatIsNotSent(t *testing.T) {
	t.Parallel()

	fake := device(t)
	good := power(fake)
	argument := func(name string) []upnp.Argument { return []upnp.Argument{{Name: name, Value: "1"}} }

	for name, call := range map[string]struct {
		service upnp.Service
		action  string
		in      []upnp.Argument
	}{
		"an action that is markup":    {good, `X xmlns:u="y"><Evil/`, nil},
		"an action with a prefix":     {good, "u:GetStatus", nil},
		"no action":                   {good, "", nil},
		"an action that starts wrong": {good, "1Get", nil},
		"an argument that is markup":  {good, "GetStatus", argument("a><b")},
		"an argument with no name":    {good, "GetStatus", argument("")},
		"a type with a quote":         {upnp.Service{Type: `x"y`, Control: good.Control}, "GetStatus", nil},
		"a type with an ampersand":    {upnp.Service{Type: "x&y", Control: good.Control}, "GetStatus", nil},
		"a type that opens a tag":     {upnp.Service{Type: "x<y", Control: good.Control}, "GetStatus", nil},
		"a type that closes one":      {upnp.Service{Type: "x>y", Control: good.Control}, "GetStatus", nil},
		"a type that ends its line":   {upnp.Service{Type: "x\ny", Control: good.Control}, "GetStatus", nil},
		"a type that returns":         {upnp.Service{Type: "x\ry", Control: good.Control}, "GetStatus", nil},
		"a type with a tab":           {upnp.Service{Type: "x\ty", Control: good.Control}, "GetStatus", nil},
		"a type with a zero":          {upnp.Service{Type: "x\x00y", Control: good.Control}, "GetStatus", nil},
		"a type with a control":       {upnp.Service{Type: "x\x01y", Control: good.Control}, "GetStatus", nil},
		"a type with a delete":        {upnp.Service{Type: "x\x7fy", Control: good.Control}, "GetStatus", nil},
		"no type":                     {upnp.Service{Control: good.Control}, "GetStatus", nil},
		"no control URL":              {upnp.Service{Type: upnptest.SwitchType}, "GetStatus", nil},
		"a control URL that is none":  {upnp.Service{Type: upnptest.SwitchType, Control: "/control"}, "GetStatus", nil},
	} {
		if _, err := call.service.Call(
			t.Context(),
			fake.Client(),
			call.action,
			call.in...,
		); !errors.Is(
			err,
			upnp.ErrInvalid,
		) {
			t.Errorf("%s = %v, want ErrInvalid", name, err)
		}
	}

	if _, err := good.Call(t.Context(), nil, "GetStatus"); !errors.Is(err, upnp.ErrInvalid) {
		t.Errorf("Call with no client = %v, want ErrInvalid", err)
	}

	if calls := fake.Calls(); len(calls) != 0 {
		t.Errorf("the device was asked %+v", calls)
	}

	// The names XML takes are taken.
	fake.Answer("Get_Status-2.x", func(in upnp.Arguments) (upnp.Arguments, error) { return in, nil })

	if out, err := good.Call(
		t.Context(),
		fake.Client(),
		"Get_Status-2.x",
		argument("_a.b-c")...,
	); err != nil ||
		len(out) != 1 {
		t.Errorf("an action and an argument named as XML allows = %+v, %v", out, err)
	}
}

// TestTheClientsOwnError: a device that cannot be reached, and a context
// that ended, are the client's errors and none of this package's.
func TestTheClientsOwnError(t *testing.T) {
	t.Parallel()

	fake := device(t)

	ended, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := power(fake).Call(ended, fake.Client(), "GetStatus"); !errors.Is(err, context.Canceled) {
		t.Errorf("Call under a context that ended = %v", err)
	}

	service := power(fake)

	fake.Close()

	_, err := service.Call(t.Context(), fake.Client(), "GetStatus")
	if err == nil || errors.Is(err, upnp.ErrAnswer) || errors.Is(err, upnp.ErrRefused) ||
		errors.Is(err, upnp.ErrInvalid) {
		t.Errorf("Call of a device that is gone = %v, want the client's own error", err)
	}
}
