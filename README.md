# go-upnp

A Go library for UPnP control points: reading a device's description, calling its actions
over SOAP, receiving its events, and finding devices over SSDP. The standard library
only, and no knowledge of any maker.

Two packages: `upnp`, one device over HTTP, and `ssdp`, finding devices.
[docs/design.md](docs/design.md) says what the API holds itself to, and what is not
built: IPv6 among it.

```go
// A client for devices follows no redirect: one that does sends a
// request wherever a device points.
client := &http.Client{
	Timeout: 10 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

light, err := upnp.Describe(ctx, client, "http://192.0.2.7:49153/description.xml")
if err != nil {
	return err
}

power, ok := light.Service("urn:schemas-upnp-org:service:SwitchPower:1")
if !ok {
	return errors.New("no switch")
}

status, err := power.Call(ctx, client, "GetStatus")
```

A program that knows where its device's services are needs no description:

```go
transport := upnp.Service{
	Type:    "urn:schemas-upnp-org:service:AVTransport:1",
	Control: "http://192.0.2.9:1400/MediaRenderer/AVTransport/Control",
}

_, err := transport.Call(ctx, client, "Play",
	upnp.Argument{Name: "InstanceID", Value: "0"}, upnp.Argument{Name: "Speed", Value: "1"})
```

Events come through a `Subscriber`, an `http.Handler` served on a server of the
program's own, at an address the program gives:

```go
subscriber := upnp.NewSubscriber()

go http.Serve(listener, subscriber)

err := subscriber.Watch(ctx, client, power, "http://192.0.2.5:49200/", func(event upnp.Event) {
	// First how things stand, then every change, in order.
})
// err says why it ended: ctx, a refusal, or upnp.ErrLost.
```

Devices are found through a `ssdp.Browser`, which a program opens once:

```go
browser, err := ssdp.Open(ctx, ssdp.Config{})
if err != nil {
	return err
}
defer browser.Close()

err = browser.Browse(ctx, ssdp.RootDevice, func(change ssdp.Change, found ssdp.Advertisement) {
	// Found, changed or lost: found.UDN is the device, found.Locations
	// where its description is and this host's address on that network.
})
```

[`upnptest`](upnptest/device.go) is a device for tests, and
[`ssdptest`](ssdp/ssdptest/ssdptest.go) a network of them.

## The command

`upnp` asks the library by hand, and says what it finds as JSON, one object on a line.
From this repository, `just upnp` builds it and runs it:

```bash
just upnp browse --for 5s
```

```bash
just upnp describe http://192.0.2.7:49153/description.xml
```

```bash
just upnp call http://192.0.2.7:49153/description.xml urn:schemas-upnp-org:service:SwitchPower:1 GetStatus
```

`upnp` with no argument says the rest: a service's actions, and its events.
