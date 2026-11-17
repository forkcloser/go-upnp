# go-upnp

A Go library for UPnP control points: reading a device's description, calling its actions
over SOAP, receiving its events, and finding devices over SSDP. The standard library
only, and no knowledge of any maker.

Built so far: descriptions and actions, in package `upnp`. Events and SSDP are designed,
and not built yet. [docs/design.md](docs/design.md) says what the API holds itself to.

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

[`upnptest`](upnptest/device.go) is a device for tests.
