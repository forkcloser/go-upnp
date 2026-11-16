# The design

This is the design of go-upnp's API. The API itself is in the packages, as Go; this
document holds what the code cannot say: what the API holds itself to, the decisions in
it and why, and what is built and what is not yet.

## What it is for

UPnP is how a device on a local network says what it is, takes commands and tells of its
changes, with no account and no cloud: a description read over HTTP, actions called over
SOAP, events delivered over GENA, and devices found over SSDP ([UPnP Device Architecture
2.0][uda]). go-upnp is the control point's side of it, for a Go program, and knows no
maker: a library for one maker's devices is written on it.

## Where it is

| Package | What it is | Built |
| --- | --- | --- |
| [`upnp`](../upnp.go) | one device, over HTTP: its [description](../description.go), its [services' descriptions](../scpd.go), its [actions](../control.go) | yes |
| [`upnp`](../events.go) | a device's events: a `Subscriber`, and `Watch` | yes |
| `ssdp` | finding devices: a `Browser`, and `Browse` | not yet |
| [`upnptest`](../upnptest/device.go) | a device in a test's process | what is built |

## What the API holds itself to

1. **No socket of its own for one device.** Package `upnp` opens nothing. The HTTP client
   every request goes through is the caller's, a `Doer`, with its timeouts, its redirects
   and its proxies; the HTTP server events arrive at is the caller's too, and the
   `Subscriber` a handler it serves. A program with a network of its own, or a rule about
   which interface a device is reached by, has one place to say so.
2. **One thing a host has once, opened once.** Port 1900 is one to a host. Package `ssdp`
   will be the one thing a program opens and lends: every search shares its sockets and
   what it has heard. The two packages do not import each other.
3. **A device is trusted with nothing.** What it sends is read up to a bound. A
   description's URLs are made absolute and held to the host the description was read
   from, whatever the port: one that points a service or an icon anywhere else is refused
   whole, so that nothing a device writes sends a request to another host. A name that
   goes into a request, an action's or an argument's, is held to what XML takes for a
   name before anything is sent, and a service's type to what a quoted attribute and a
   header take.
4. **A service is plain data.** `Service` is a type, a control URL, an events URL and a
   description URL. A description gives it, or a program that knows where its device's
   services are writes it and never reads a description. Nothing hides a client or a
   connection in it.
5. **Order is part of the protocol.** An action's arguments are a list, sent in the order
   given, and its answer is a list in the device's order: devices read arguments by place
   as often as by name, so a map is not a way to carry them.
6. **A refusal is an error with what the device said.** A SOAP fault is a `*FaultError`
   with the UPnP error code, under whatever HTTP status it came; a status that is no
   success and no fault is a `*StatusError`. Both are `ErrRefused`. A failure to reach the
   device is the client's own error, handed on as it is.
7. **Devices are read as they write.** A byte order mark, names in another case, text
   past the end, an encoding of one byte a character, a fault under 200, a value that is
   a document sent bare: each is a thing devices do, and each is read. What cannot be
   read is `ErrAnswer`, never a guess.
8. **A maker's elements are the maker's library's.** `Device.Decode` reads the device's
   own element into any value, so that what the standard does not name is read by who
   knows it.
9. **Nothing is kept, nothing runs.** No package-level state, no goroutine, no file, no
   environment, no log. A `Watch` runs on the goroutine that called it, and a
   `Subscriber` holds a subscription for as long as its `Watch` lasts.
10. **Watching is one call, and a loss is said.** `Watch` subscribes, tells how things
    stand and then every change, in order, renews ahead of the subscription's end, and
    unsubscribes when it returns. When the subscription can be trusted no more it
    returns `ErrLost`, and does not subscribe again by itself: whether and when to watch
    again is the caller's to decide, and watching again starts from how things stand.
11. **Nothing about the caller's address is guessed.** The URL a device reaches the
    `Subscriber` at is given to each `Watch`: it depends on which of the host's
    addresses is on that device's network, which the caller knows and this package
    cannot. One `Subscriber`, on one server, serves devices on every interface.

## Decisions

- **The standard library only.** XML, HTTP and URLs are all there.
- **A redirect is the client's to refuse.** A redirect is something a device says too,
  and a client that follows them sends the request, an action's arguments with it,
  wherever the device points. This package does not make the client and cannot stop it.
  It says so, shows the client to pass, one whose `CheckRedirect` returns
  `http.ErrUseLastResponse`, and refuses with `ErrAnswer` what a client brings back from
  another host than the one asked: after the fact, so that at least a stranger's answer
  is not taken for the device's. Through a client that follows none, a redirect is an
  `ErrRefused` with its status. `upnptest`'s client is such a one.
- **A document is made UTF-8 once.** One that declares ISO 8859-1 or ASCII is converted
  when it is read, ahead of any parsing, so that a place in it is the same place to
  everything that reads it.
- **A device keeps its own element, and no copy of it.** `Describe` notes where each
  device's element starts in the description and which namespaces are in force there,
  and `Decode` reads that element alone, under those namespaces: a name's namespace is
  part of the name. Reading each device of a description costs what the description
  does. A description may have sixteen levels of devices, the root device being the
  first, where the standard gives no bound and no device is known past three: each
  level's element holds the levels below, and without a bound a hostile description's
  cost is the square of its depth.
- **`URLBase` is taken when it is on the device, and not otherwise.** UPnP 1.0 has it and
  1.1 forbids it; devices that write one at another host are known, and are read against
  the description's own URL.
- **A page for a person may be anywhere.** `PresentationURL` is made absolute and not
  held to the device: nothing here asks for it.
- **The request's header is written `SOAPACTION`.** HTTP does not care about a header's
  case and some devices do; the standard writes it so. The body goes with its length,
  not in chunks, which not every device reads.
- **An argument with no direction goes in.** A device that leaves the direction out still
  has to be sent the argument.
- **A later version serves for an earlier one.** `Device.Service` finds a service of the
  type asked for at that version or above, as the standard has versions work.
- **A fault that is cut short is still a fault.** Once the device has said `Fault`, what
  follows only says why.

- **A subscription has a path of its own.** The callback given and a random name: an
  event is routed by the last element of its path, whatever the handler is served under.
  A device may send the first event before it has answered `SUBSCRIBE`, which is what
  says the subscription's identifier: the event is kept by its path, and told once the
  answer says it is this subscription's.
- **The device's count is held.** The first event is 0 and each is one more than the
  last; one that is not says another did not arrive, and the subscription is lost. An
  event that carries no count is told and holds the count to nothing. What a real
  device's count does over a long subscription was not observed.
- **An event that cannot be read is an event missed.** It ends the watch with `ErrLost`,
  which is also an `ErrAnswer`, as one too large does, and the device is answered 400.
  It takes its place among the events of the subscription it was sent under: one that
  arrives before the device has said which subscription the path is, and turns out
  another's, is dropped with the rest of that other's.
- **Before the device has answered, a path proves nothing.** What fills a subscription's
  path past what is held, before the device has said which subscription it is, is
  refused and loses nothing: it may be anyone's.
- **The count is held in the order events finish arriving.** Two events a device sends
  at once, on two connections, are told in the order their bodies ended, and if that is
  not the order of their counts the subscription is lost. A device that sends its
  events one after the other never shows it.
- **Nothing is told while a renewal waits for its answer.** It is asked on the goroutine
  that tells; events are held meanwhile, up to the sixty-four, and a client with no
  timeout of its own waits for as long as the subscription has left.
- **One subscription holds sixty-four mebibytes at most:** sixty-four events of one each.
- **An event's body has ten seconds to arrive,** where the caller's server lets a
  deadline be set. No test holds it: it was measured once, by hand, a body left short
  ending the watch after ten seconds with `ErrLost`.
- **Events wait for the handler, up to sixty-four.** The handler runs on the caller's
  goroutine and may take its time. Past sixty-four untold the device is answered 503 and
  the subscription is lost, since the device may count an event it could not deliver.
- **Renewal goes by what the device granted,** at half of it; a length that is no number
  of seconds, `infinite` among them, is taken as the half hour that was asked for. A
  device that refuses a renewal has forgotten the subscription; one that cannot be
  reached is asked again at half of what is left, and no sooner than a second later,
  until what is left is too short to try in. Nothing is tried past that. A subscription
  that ran out while the handler was at work is lost, not renewed late.
- **GENA's headers are written in capitals,** as the standard writes them.

## Tests

Black-box, against `upnptest`'s device over the loopback, and against a bare socket for
what goes on the wire. Each of the four readers of what a device sends, a description, a
service's description, an answer and an event, has a fuzz target that holds it to its
errors, and the description's to never pointing off the device. The count's return to 1
after its largest value is not reached by any test: four thousand million events are
not sent.

[uda]: https://openconnectivity.org/upnp-specs/UPnP-arch-DeviceArchitecture-v2.0-20200417.pdf
