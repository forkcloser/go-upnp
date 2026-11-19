// Package command is the upnp command: the library and nothing more,
// asked by hand, with JSON coming out.
package command

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	upnp "github.com/forkcloser/go-upnp"
	"github.com/forkcloser/go-upnp/ssdp"
)

// How the command ends.
const (
	// Done is a command that did what it was asked.
	Done = 0
	// Failed is one that could not: a device that is not there, one that
	// refused, a network that is not there.
	Failed = 1
	// Misused is one that was not asked properly.
	Misused = 2
)

// Environment is where a command runs.
type Environment struct {
	// Args is what it was called with, the program's name aside.
	Args []string
	// Out is where what is found is said, as JSON, one object on a line;
	// Err where what goes wrong is.
	Out io.Writer
	Err io.Writer
	// Network is the network devices are looked for on. Nil is the
	// host's own.
	Network ssdp.Network
	// Client is what devices are asked through. Nil is one that follows
	// no redirect and waits ten seconds.
	Client upnp.Doer

	// saying is held while a line is said: several things happen at
	// once, and each is said whole.
	saying *sync.Mutex
}

// Usage is how the command is asked.
const Usage = `upnp looks at the UPnP devices of the local network.

  upnp browse [--for DURATION] [--interface NAME]... [TARGET]
                             tell of what is advertised as it comes and goes:
                             every root device, or what TARGET names
                             (ssdp:all, a device or service type, uuid:...)
  upnp describe URL          read the description at URL: the device, those
                             embedded in it, and their services
  upnp service URL TYPE      read the description of the service of that type
                             of the device at URL: its actions and variables
  upnp call URL TYPE ACTION [NAME=VALUE]...
                             call an action of that service, its arguments in
                             the order given, which has to be the service's
  upnp watch --callback CALLBACK URL TYPE
                             tell of that service's events as they come. The
                             callback is where the device reaches this
                             command: http://ADDRESS:PORT/, at this host's
                             address on the device's network, which browse
                             gives as "local". The command listens there.

Everything is said as JSON, one object on a line.
`

// errUsage is a command that was not asked properly.
var errUsage = errors.New("usage")

// timeout is how long a device has to answer the command's own client.
const timeout = 10 * time.Second

// Run runs the command until it is done or ctx ends, and says how it
// ended.
func Run(ctx context.Context, env Environment) int {
	verbs := map[string]func(context.Context, *Environment, []string) error{
		"browse": browse, "describe": describe, "service": service, "call": call, "watch": watch,
	}

	if len(env.Args) == 0 || verbs[env.Args[0]] == nil {
		_, _ = io.WriteString(env.Err, Usage)

		if len(env.Args) > 0 && (env.Args[0] == "help" || env.Args[0] == "--help" || env.Args[0] == "-h") {
			return Done
		}

		return Misused
	}

	env.saying = &sync.Mutex{}

	if env.Client == nil {
		env.Client = &http.Client{
			Timeout:       timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}

	err := verbs[env.Args[0]](ctx, &env, env.Args[1:])
	if err == nil {
		return Done
	}

	_, _ = fmt.Fprintf(env.Err, "upnp %s: %v\n", env.Args[0], err)

	if errors.Is(err, errUsage) || errors.Is(err, upnp.ErrInvalid) || errors.Is(err, ssdp.ErrInvalid) {
		return Misused
	}

	return Failed
}

// say says one thing, whole, on a line.
func (e *Environment) say(what any) {
	line, err := json.Marshal(what)
	if err != nil {
		return
	}

	e.saying.Lock()
	defer e.saying.Unlock()

	_, _ = e.Out.Write(append(line, '\n'))
}

// flags is the flags of a verb, and what follows them. What is wrong
// with them is said by the verb, in one line.
func flags(verb string, args []string, define func(*flag.FlagSet), least, most int) ([]string, error) {
	set := flag.NewFlagSet(verb, flag.ContinueOnError)
	set.SetOutput(io.Discard)

	if define != nil {
		define(set)
	}

	if err := set.Parse(args); err != nil {
		return nil, fmt.Errorf("%w: %w", errUsage, err)
	}

	if set.NArg() < least || most >= 0 && set.NArg() > most {
		return nil, fmt.Errorf("%w: %d arguments", errUsage, set.NArg())
	}

	return set.Args(), nil
}

// names is a flag given several times.
type names []string

func (n *names) String() string { return strings.Join(*n, ",") }

func (n *names) Set(value string) error {
	*n = append(*n, value)

	return nil
}

// What browse says of an advertisement.
type (
	advertised struct {
		Change    string    `json:"change"`
		USN       string    `json:"usn"`
		UDN       string    `json:"udn"`
		Type      string    `json:"type"`
		Server    string    `json:"server,omitempty"`
		Locations []located `json:"locations"`
	}
	located struct {
		URL       string `json:"url"`
		Interface string `json:"interface"`
		Local     string `json:"local,omitempty"`
	}
)

func browse(ctx context.Context, env *Environment, args []string) error {
	var (
		lasting    time.Duration
		interfaces names
	)

	rest, err := flags("browse", args, func(set *flag.FlagSet) {
		set.DurationVar(&lasting, "for", 0, "")
		set.Var(&interfaces, "interface", "")
	}, 0, 1)
	if err != nil {
		return err
	}

	target := ssdp.RootDevice
	if len(rest) == 1 {
		target = rest[0]
	}

	config := ssdp.Config{Network: env.Network}
	if len(interfaces) > 0 {
		config.Interfaces = func(one ssdp.Interface) bool { return slices.Contains(interfaces, one.Name) }
	}

	browser, err := ssdp.Open(ctx, config)
	if err != nil {
		return fmt.Errorf("the network: %w", err)
	}

	defer func() { _ = browser.Close() }()

	if lasting > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, lasting)
		defer cancel()
	}

	err = browser.Browse(ctx, target, func(change ssdp.Change, found ssdp.Advertisement) {
		env.say(heard(change, found))
	})

	// Its time being up, or its being stopped, is how a browse ends.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return nil
	}

	return err //nolint:wrapcheck // the library's own word, under the verb's name
}

// heard is an advertisement and what became of it, as it is said.
func heard(change ssdp.Change, found ssdp.Advertisement) advertised {
	said := advertised{
		Change: change.String(), USN: found.USN, UDN: found.UDN, Type: found.Type, Server: found.Server,
		Locations: []located{},
	}

	for _, location := range found.Locations {
		where := located{URL: location.URL, Interface: location.Interface.Name}
		if location.Local.IsValid() {
			where.Local = location.Local.String()
		}

		said.Locations = append(said.Locations, where)
	}

	return said
}

// What describe says of a device.
type (
	described struct {
		UDN          string           `json:"udn"`
		Type         string           `json:"type"`
		Name         string           `json:"name,omitempty"`
		Manufacturer string           `json:"manufacturer,omitempty"`
		Model        string           `json:"model,omitempty"`
		Number       string           `json:"modelNumber,omitempty"`
		Serial       string           `json:"serial,omitempty"`
		Presentation string           `json:"presentation,omitempty"`
		Services     []describedPiece `json:"services"`
		Devices      []described      `json:"devices,omitempty"`
	}
	describedPiece struct {
		Type    string `json:"type"`
		ID      string `json:"id,omitempty"`
		Control string `json:"control,omitempty"`
		Events  string `json:"events,omitempty"`
		SCPD    string `json:"scpd,omitempty"`
	}
)

func tell(device *upnp.Device) described {
	said := described{
		UDN: device.UDN, Type: device.Type, Name: device.FriendlyName, Manufacturer: device.Manufacturer,
		Model: device.ModelName, Number: device.ModelNumber, Serial: device.SerialNumber,
		Presentation: device.PresentationURL, Services: []describedPiece{},
	}

	for _, one := range device.Services {
		said.Services = append(said.Services, describedPiece{
			Type: one.Type, ID: one.ID, Control: one.Control, Events: one.Events, SCPD: one.SCPD,
		})
	}

	for index := range device.Devices {
		said.Devices = append(said.Devices, tell(&device.Devices[index]))
	}

	return said
}

func describe(ctx context.Context, env *Environment, args []string) error {
	rest, err := flags("describe", args, nil, 1, 1)
	if err != nil {
		return err
	}

	device, err := upnp.Describe(ctx, env.Client, rest[0])
	if err != nil {
		return err //nolint:wrapcheck // the library's own word, under the verb's name
	}

	env.say(tell(device))

	return nil
}

// errNoService is a device that has no service of the type asked for.
var errNoService = errors.New("the device has no such service")

// find is the service of a type, of the device described at a URL.
func find(ctx context.Context, env *Environment, location, kind string) (upnp.Service, error) {
	device, err := upnp.Describe(ctx, env.Client, location)
	if err != nil {
		return upnp.Service{}, err //nolint:wrapcheck // the library's own word, under the verb's name
	}

	found, ok := device.Service(kind)
	if !ok {
		return upnp.Service{}, fmt.Errorf("%w: %s", errNoService, kind)
	}

	return found, nil
}

func service(ctx context.Context, env *Environment, args []string) error {
	rest, err := flags("service", args, nil, 2, 2)
	if err != nil {
		return err
	}

	found, err := find(ctx, env, rest[0], rest[1])
	if err != nil {
		return err
	}

	scpd, err := found.Describe(ctx, env.Client)
	if err != nil {
		return err //nolint:wrapcheck // the library's own word, under the verb's name
	}

	said := serviced{Actions: []servicedAction{}, Variables: []servicedVariable{}}

	for _, action := range scpd.Actions {
		said.Actions = append(said.Actions, servicedAction{
			Name: action.Name, In: parameters(action.In), Out: parameters(action.Out),
		})
	}

	for _, variable := range scpd.Variables {
		said.Variables = append(said.Variables, servicedVariable(variable))
	}

	env.say(said)

	return nil
}

// What service says of a service.
type (
	serviced struct {
		Actions   []servicedAction   `json:"actions"`
		Variables []servicedVariable `json:"variables"`
	}
	servicedAction struct {
		Name string   `json:"name"`
		In   []string `json:"in"`
		Out  []string `json:"out"`
	}
	servicedVariable struct {
		Name    string   `json:"name"`
		Type    string   `json:"type,omitempty"`
		Default string   `json:"default,omitempty"`
		Allowed []string `json:"allowed,omitempty"`
		Minimum string   `json:"minimum,omitempty"`
		Maximum string   `json:"maximum,omitempty"`
		Step    string   `json:"step,omitempty"`
		Evented bool     `json:"evented"`
	}
)

// parameters is the names of an action's arguments, in their order.
func parameters(of []upnp.Parameter) []string {
	said := make([]string, 0, len(of))
	for _, parameter := range of {
		said = append(said, parameter.Name)
	}

	return said
}

// pair is one argument or one value, as it is said.
type pair struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func pairs(arguments upnp.Arguments) []pair {
	said := make([]pair, 0, len(arguments))
	for _, argument := range arguments {
		said = append(said, pair(argument))
	}

	return said
}

func call(ctx context.Context, env *Environment, args []string) error {
	rest, err := flags("call", args, nil, 3, -1)
	if err != nil {
		return err
	}

	arguments := make([]upnp.Argument, 0, len(rest))

	for _, given := range rest[3:] {
		name, value, found := strings.Cut(given, "=")
		if !found {
			return fmt.Errorf("%w: %q is not NAME=VALUE", errUsage, given)
		}

		arguments = append(arguments, upnp.Argument{Name: name, Value: value})
	}

	found, err := find(ctx, env, rest[0], rest[1])
	if err != nil {
		return err
	}

	answer, err := found.Call(ctx, env.Client, rest[2], arguments...)
	if err != nil {
		return err //nolint:wrapcheck // the library's own word, under the verb's name
	}

	env.say(pairs(answer))

	return nil
}

// event is one event, as it is said.
type event struct {
	Seq        uint32 `json:"seq"`
	Properties []pair `json:"properties"`
}

// What the callback's server gives a connection: headerTime for a
// request's headers, requestTime for the whole of it, and idleTime between
// two requests. An event's body has the Subscriber's own, shorter time;
// requestTime is for a request that is no event, which is not answered
// until its body has come: the server waits for it, with no deadline of
// its own.
const (
	headerTime  = 10 * time.Second
	requestTime = 30 * time.Second
	idleTime    = time.Minute
)

// callbackServer is the server events are delivered to. Every read it
// makes has a deadline: a host that sends a body slowly, or holds a
// connection and sends nothing, is let go of.
func callbackServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: headerTime,
		ReadTimeout:       requestTime,
		IdleTimeout:       idleTime,
	}
}

func watch(ctx context.Context, env *Environment, args []string) error {
	var callback string

	rest, err := flags("watch", args, func(set *flag.FlagSet) {
		set.StringVar(&callback, "callback", "", "")
	}, 2, 2)
	if err != nil {
		return err
	}

	reached, err := url.Parse(callback)
	if err != nil || reached.Scheme != "http" || reached.Port() == "" {
		return fmt.Errorf("%w: --callback is http://ADDRESS:PORT/, not %q", errUsage, callback)
	}

	found, err := find(ctx, env, rest[0], rest[1])
	if err != nil {
		return err
	}

	var listening net.ListenConfig

	listener, err := listening.Listen(ctx, "tcp", reached.Host)
	if err != nil {
		return fmt.Errorf("listening at the callback: %w", err)
	}

	subscriber := upnp.NewSubscriber()
	server := callbackServer(subscriber)

	served := make(chan struct{})

	go func() {
		// It ends when the server is closed, which is how it is meant to.
		_ = server.Serve(listener)

		close(served)
	}()

	defer func() {
		_ = server.Close()

		<-served
	}()

	err = subscriber.Watch(ctx, env.Client, found, callback, func(told upnp.Event) {
		env.say(event{Seq: told.Seq, Properties: pairs(told.Properties)})
	})

	// Being stopped is how a watch ends.
	if errors.Is(err, context.Canceled) {
		return nil
	}

	return err //nolint:wrapcheck // the library's own word, under the verb's name
}
