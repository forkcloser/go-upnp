package command_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	upnp "github.com/forkcloser/go-upnp"
	"github.com/forkcloser/go-upnp/internal/command"
	"github.com/forkcloser/go-upnp/ssdp"
	"github.com/forkcloser/go-upnp/ssdp/ssdptest"
	"github.com/forkcloser/go-upnp/upnptest"
)

// wait bounds every wait of a test; generous, for a loaded runner.
const wait = 30 * time.Second

// output is what a command says, read while it is still saying.
type output struct {
	mu   sync.Mutex
	said bytes.Buffer
}

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.said.Write(p)
}

func (o *output) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.said.String()
}

// run runs the command to its end.
func run(t *testing.T, env command.Environment, args ...string) (code int, said, failed string) {
	t.Helper()

	var out, wrong output

	env.Args, env.Out, env.Err = args, &out, &wrong

	return command.Run(t.Context(), env), out.String(), wrong.String()
}

func device(t *testing.T) *upnptest.Device {
	t.Helper()

	fake := upnptest.New()
	t.Cleanup(fake.Close)

	return fake
}

// TestHowItIsAsked: with nothing, or with what it does not know, the
// command says how it is asked.
func TestHowItIsAsked(t *testing.T) {
	t.Parallel()

	for name, asked := range map[string]struct {
		args []string
		code int
	}{
		"nothing":                    {nil, command.Misused},
		"a verb it does not have":    {[]string{"dance"}, command.Misused},
		"help":                       {[]string{"help"}, command.Done},
		"a flag it does not have":    {[]string{"browse", "--loud"}, command.Misused},
		"too many arguments":         {[]string{"describe", "a", "b"}, command.Misused},
		"too few":                    {[]string{"call", "http://192.0.2.1/", "type"}, command.Misused},
		"an argument with no value":  {[]string{"call", "http://192.0.2.1/", "type", "Action", "novalue"}, command.Misused},
		"a watch with no callback":   {[]string{"watch", "http://192.0.2.1/", "type"}, command.Misused},
		"a callback with no port":    {[]string{"watch", "--callback", "http://192.0.2.1/", "http://192.0.2.1/", "type"}, command.Misused},
		"a URL that is none":         {[]string{"describe", "nowhere"}, command.Misused},
		"a target that is two lines": {[]string{"browse", "a\r\nb"}, command.Misused},
	} {
		code, out, failed := run(t, command.Environment{Network: ssdptest.NewHost()}, asked.args...)
		if code != asked.code || out != "" || failed == "" {
			t.Errorf("%s: ended %d, said %q and %q", name, code, out, failed)
		}
	}
}

// TestBrowse: what is on the network is said, a line each, for as long
// as asked.
func TestBrowse(t *testing.T) {
	t.Parallel()

	lan, host := ssdptest.NewLAN(), ssdptest.NewHost()
	host.Plug(lan, "en0", 1, netip.MustParsePrefix("192.0.2.10/24"))
	host.Plug(lan, "en1", 2, netip.MustParsePrefix("192.0.2.11/24"))
	lan.Device(netip.MustParseAddr("192.0.2.20"), ssdptest.Advert{
		USN: "uuid:1::" + ssdp.RootDevice, Type: ssdp.RootDevice, Location: "http://192.0.2.20/d.xml",
	})

	code, out, failed := run(t, command.Environment{Network: host}, "browse", "--for", "300ms", "--interface", "en1")

	const want = `{"change":"found","usn":"uuid:1::upnp:rootdevice","udn":"uuid:1","type":"upnp:rootdevice",` +
		`"server":"test/1.0 UPnP/1.1 device/1.0","locations":[{"url":"http://192.0.2.20/d.xml","interface":"en1","local":"192.0.2.11"}]}` + "\n"

	if code != command.Done || out != want || failed != "" {
		t.Errorf("browse ended %d, said %q and %q", code, out, failed)
	}

	// Another target, and nothing under it.
	code, out, failed = run(t, command.Environment{Network: host}, "browse", "--for", "300ms", "uuid:2")
	if code != command.Done || out != "" || failed != "" {
		t.Errorf("browse for what is not there ended %d, said %q and %q", code, out, failed)
	}
}

// What describe says, as far as the test reads it.
type (
	describedDevice struct {
		UDN      string             `json:"udn"`
		Services []describedService `json:"services"`
		Devices  []describedDevice  `json:"devices"`
	}
	describedService struct {
		Type    string `json:"type"`
		Control string `json:"control"`
	}
)

// TestADevice: its description, a service's, and an action's answer.
func TestADevice(t *testing.T) {
	t.Parallel()

	fake := device(t)
	fake.Answer("SetBoth", func(in upnp.Arguments) (upnp.Arguments, error) {
		return upnp.Arguments{{Name: "Was", Value: in[0].Value + in[1].Value}}, nil
	})

	code, out, failed := run(t, command.Environment{}, "describe", fake.Location())

	var described describedDevice

	if err := json.Unmarshal([]byte(out), &described); err != nil || code != command.Done || failed != "" ||
		described.UDN != upnptest.UDN || len(described.Services) != 1 ||
		described.Services[0].Control != fake.URL()+upnptest.SwitchControl ||
		len(described.Devices) != 1 || described.Devices[0].UDN != upnptest.EmbeddedUDN {
		t.Errorf("describe ended %d, said %q and %q (%v)", code, out, failed, err)
	}

	code, out, failed = run(t, command.Environment{}, "service", fake.Location(), upnptest.SwitchType)
	if code != command.Done || failed != "" ||
		!strings.Contains(out, `{"name":"SetBoth","in":["First","Second"],"out":["Was"]}`) ||
		!strings.Contains(
			out,
			`{"name":"Level","type":"ui1","minimum":"0","maximum":"100","step":"5","evented":true}`,
		) {
		t.Errorf("service ended %d, said %q and %q", code, out, failed)
	}

	code, out, failed = run(
		t,
		command.Environment{},
		"call",
		fake.Location(),
		upnptest.SwitchType,
		"SetBoth",
		"First=a=b",
		"Second=c",
	)
	if code != command.Done || out != `[{"name":"Was","value":"a=bc"}]`+"\n" || failed != "" {
		t.Errorf("call ended %d, said %q and %q", code, out, failed)
	}

	// A refusal, a service the device does not have, a device that is gone.
	code, out, failed = run(t, command.Environment{}, "call", fake.Location(), upnptest.SwitchType, "Nothing")
	if code != command.Failed || out != "" || !strings.Contains(failed, "UPnP error 401") {
		t.Errorf("an action refused ended %d, said %q and %q", code, out, failed)
	}

	code, _, failed = run(t, command.Environment{}, "service", fake.Location(), "urn:x:service:None:1")
	if code != command.Failed || !strings.Contains(failed, "no such service") {
		t.Errorf("a service that is not there ended %d, said %q", code, failed)
	}

	fake.Close()

	if code, out, failed = run(
		t,
		command.Environment{},
		"describe",
		fake.Location(),
	); code != command.Failed || out != "" ||
		failed == "" {
		t.Errorf("a device that is gone ended %d, said %q and %q", code, out, failed)
	}
}

// TestWatch: the command listens at the callback it is given, and says
// each event until it is stopped.
func TestWatch(t *testing.T) {
	t.Parallel()

	fake := device(t)

	// A port that is free, for a moment at least.
	var listening net.ListenConfig

	free, err := listening.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	callback := "http://" + free.Addr().String() + "/"
	_ = free.Close()

	var out, failed output

	ctx, cancel := context.WithCancel(t.Context())
	ended := make(chan int, 1)

	go func() {
		ended <- command.Run(ctx, command.Environment{
			Args: []string{"watch", "--callback", callback, fake.Location(), upnptest.SwitchType}, Out: &out, Err: &failed,
		})
	}()

	said := func(want string) {
		t.Helper()

		for deadline := time.Now().Add(wait); out.String() != want; time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("watch said %q and %q, want %q", out.String(), failed.String(), want)
			}
		}
	}

	const first = `{"seq":0,"properties":[{"name":"Status","value":"0"}]}` + "\n"

	said(first)
	fake.Notify(upnp.Argument{Name: "Status", Value: "1"})
	said(first + `{"seq":1,"properties":[{"name":"Status","value":"1"}]}` + "\n")

	cancel()

	select {
	case code := <-ended:
		if code != command.Done || failed.String() != "" || fake.Subscribers() != 0 {
			t.Errorf(
				"watch ended %d, said %q; the device holds %d subscriptions",
				code,
				failed.String(),
				fake.Subscribers(),
			)
		}
	case <-time.After(wait):
		t.Fatal("watch did not end")
	}

	// The callback is held by another: the command says so.
	held, err := listening.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = held.Close() }()

	code, _, why := run(
		t,
		command.Environment{},
		"watch",
		"--callback",
		"http://"+held.Addr().String()+"/",
		fake.Location(),
		upnptest.SwitchType,
	)
	if code != command.Failed || !strings.Contains(why, "listening at the callback") {
		t.Errorf("a callback another holds ended %d, said %q", code, why)
	}
}
