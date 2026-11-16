package upnp

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/url"
	"strconv"
	"strings"
)

// Device is a device as its description gives it: a root device, or one
// embedded in another.
type Device struct {
	// UDN is who it is: "uuid:" and a UUID, the same wherever the device
	// is and whatever its address.
	UDN string
	// Type is its type: "urn:schemas-upnp-org:device:MediaRenderer:1".
	Type string
	// What it says of itself, for a person to read.
	FriendlyName     string
	Manufacturer     string
	ManufacturerURL  string
	ModelName        string
	ModelNumber      string
	ModelDescription string
	ModelURL         string
	SerialNumber     string
	UPC              string
	// PresentationURL is a page for a person's browser, made absolute.
	// Nothing here asks for it, and it may be anywhere.
	PresentationURL string
	// Icons is its pictures, each URL on the device.
	Icons []Icon
	// Services is its own services, each URL absolute and on the device.
	Services []Service
	// Devices is the devices embedded in it.
	Devices []Device

	// source is the description that was read, from the start of the
	// device's own element, and scope the start of an element that
	// declares the namespaces in force there.
	source []byte
	scope  string
}

// Icon is one picture of a device.
type Icon struct {
	// MIME is its media type: "image/png".
	MIME string
	// Width, Height and Depth are in pixels and bits a pixel; zero when
	// the device gave none.
	Width  int
	Height int
	Depth  int
	// URL is where it is, on the device.
	URL string
}

// Service is one service of a device: what it is and where it is asked.
// It is plain data: a description gives it, or a program that knows
// where its device's services are writes it.
type Service struct {
	// Type is its type: "urn:schemas-upnp-org:service:AVTransport:1".
	Type string
	// ID is its name in the device: "urn:upnp-org:serviceId:AVTransport".
	ID string
	// Control is the URL its actions are posted to.
	Control string
	// Events is the URL subscriptions to it go to. Empty: it has no
	// events.
	Events string
	// SCPD is the URL of its own description.
	SCPD string
}

// Describe reads the description of the root device at location, and of
// every device embedded in it. location is what the device advertises,
// or what its maker documents.
//
// Every URL in the description is made absolute: against the URLBase the
// device gives when that is on location's host, and against location
// otherwise. A service's and an icon's URL then has to be on location's
// host, whatever the port: a description that points anywhere else is
// refused whole, with ErrAnswer, so that nothing a device writes sends a
// request to another host. What a device answers with a redirect is the
// client's to refuse: the package's comment says which client to pass.
// A description may have sixteen levels of devices, the root device
// being the first.
func Describe(ctx context.Context, client Doer, location string) (*Device, error) {
	home, err := absolute(location)
	if err != nil {
		return nil, err
	}

	document, err := get(ctx, client, home)
	if err != nil {
		return nil, err
	}

	elements, err := places(document)
	if err != nil {
		return nil, fmt.Errorf("the description at %s: %w", location, err)
	}

	var root struct {
		XMLName xml.Name    `xml:"root"`
		URLBase string      `xml:"urlbase"`
		Devices []xmlDevice `xml:"device"`
	}

	if err = parse(document, &root); err != nil {
		return nil, fmt.Errorf("the description at %s: %w", location, err)
	}

	if len(root.Devices) != 1 || strings.TrimSpace(root.Devices[0].UDN) == "" {
		return nil, fmt.Errorf("%w: %s is not the description of one root device", ErrAnswer, location)
	}

	from := resolver{home: home, base: home}

	if base, baseErr := absolute(strings.TrimSpace(root.URLBase)); baseErr == nil && from.on(base) {
		from.base = base
	}

	device, err := root.Devices[0].device(from, &elements)
	if err != nil {
		return nil, fmt.Errorf("the description at %s: %w", location, err)
	}

	return &device, nil
}

// Decode reads the device's own element of its description into a value,
// as encoding/xml's Unmarshal does: how a maker's elements, which the
// standard does not name, are read. Names are matched as the device
// wrote them.
func (d *Device) Decode(into any) error {
	if d.source == nil {
		return fmt.Errorf("%w: a device no description was read for", ErrInvalid)
	}

	// The element is read inside one that declares what the description
	// declares above it: a name's namespace is part of the name, and a
	// maker's library tells its elements from another's by it. Reading
	// stops at the element's end, whatever follows it.
	reading := xml.NewDecoder(io.MultiReader(strings.NewReader(d.scope), bytes.NewReader(d.source)))
	reading.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }

	// The element that declares, and then the device's own.
	if _, err := child(reading); err != nil {
		return fmt.Errorf("%w: the device is not in its description: %w", ErrAnswer, err)
	}

	element, err := child(reading)
	if err != nil || element == nil {
		return fmt.Errorf("%w: the device is not in its description", ErrAnswer)
	}

	if err = reading.DecodeElement(into, element); err != nil {
		return fmt.Errorf("decoding the device's description: %w", err)
	}

	return nil
}

// scopeName is the element a device's own is read inside, and xmlns the
// attribute that declares a namespace.
const (
	scopeName = "scope"
	xmlns     = "xmlns"
)

// All is the device, and every device embedded in it or below, in the
// order the description writes them.
func (d *Device) All() iter.Seq[*Device] {
	return func(yield func(*Device) bool) { d.each(yield) }
}

// Service is the first service of that type in the device or below it,
// at that version or a later one, which serves for it; or the one of
// that ID.
func (d *Device) Service(kind string) (Service, bool) {
	for device := range d.All() {
		for _, service := range device.Services {
			if matches(service.Type, kind) || service.ID == kind {
				return service, true
			}
		}
	}

	return Service{}, false
}

func (d *Device) each(yield func(*Device) bool) bool {
	if !yield(d) {
		return false
	}

	for index := range d.Devices {
		if !d.Devices[index].each(yield) {
			return false
		}
	}

	return true
}

// maxNesting is how many levels of devices a description may have, the
// root device being the first. The standard gives no bound and no
// device is known past three: without one, reading each device of a
// description costs the square of its depth.
const maxNesting = 16

// place is where a device is in its description: the description from
// the start of the device's own element, and the start of an element
// that declares the namespaces in force there.
type place struct {
	element []byte
	scope   string
}

// places is where each device of a description is in it, in the order
// the devices are written: the order encoding/xml fills them in.
func places(document []byte) ([]place, error) {
	var (
		reading = decoder(document)
		within  path
		found   []place
		// levels is how many devices are open.
		levels int
	)

	for {
		start := int(reading.InputOffset())

		token, err := reading.Token()
		if err != nil {
			// The end, or what parse will say of it.
			return found, nil //nolint:nilerr // see above
		}

		switch element := token.(type) {
		case xml.StartElement:
			if !within.enter(element) {
				continue
			}

			levels++
			if levels > maxNesting {
				return nil, fmt.Errorf("%w: more than %d levels of devices", ErrAnswer, maxNesting)
			}

			found = append(found, place{element: document[start:], scope: within.scope()})
		case xml.EndElement:
			if within.leave() {
				levels--
			}
		}
	}
}

// path is the elements open at a place in a description, by name, and
// whether each is one of the description's devices: the root's, or one
// in a device's list. An element a maker calls device anywhere else is
// none.
type path struct {
	open []opened
}

type opened struct {
	name   string
	device bool
	// declared is the namespaces the element declares, as attributes.
	declared []xml.Attr
}

// enter opens an element, and says whether it is one of the devices.
func (p *path) enter(element xml.StartElement) bool {
	const listed = 2 // a list, and the device it is in

	name := strings.ToLower(element.Name.Local)
	depth := len(p.open)
	device := name == "device" &&
		(depth == 1 || depth >= listed && p.open[depth-1].name == "devicelist" && p.open[depth-listed].device)

	entered := opened{name: name, device: device}

	for _, attribute := range element.Attr {
		if attribute.Name.Space == xmlns || attribute.Name.Space == "" && attribute.Name.Local == xmlns {
			entered.declared = append(entered.declared, attribute)
		}
	}

	p.open = append(p.open, entered)

	return device
}

// scope is the start of an element that declares every namespace the
// open elements declare, a later declaration over an earlier one of its
// name.
func (p *path) scope() string {
	var (
		names  []string
		values = map[string]string{}
	)

	for _, element := range p.open {
		for _, attribute := range element.declared {
			name := xmlns
			if attribute.Name.Space != "" {
				name += ":" + attribute.Name.Local
			}

			if _, declared := values[name]; !declared {
				names = append(names, name)
			}

			values[name] = attribute.Value
		}
	}

	var start bytes.Buffer

	start.WriteString("<" + scopeName)

	for _, name := range names {
		start.WriteString(" " + name + `="`)
		_ = xml.EscapeText(&start, []byte(values[name]))
		start.WriteString(`"`)
	}

	start.WriteString(">")

	return start.String()
}

// leave closes the last element opened, and says whether it was one of
// the devices.
func (p *path) leave() bool {
	if len(p.open) == 0 {
		return false
	}

	last := p.open[len(p.open)-1]
	p.open = p.open[:len(p.open)-1]

	return last.device
}

// resolver makes the URLs of a description absolute, and holds them to
// the device.
type resolver struct {
	// home is where the description was read, and base what its URLs are
	// relative to.
	home *url.URL
	base *url.URL
}

// on says whether a URL is on the device: the host the description was
// read from, whatever the port.
func (r resolver) on(target *url.URL) bool {
	return (target.Scheme == "http" || target.Scheme == "https") &&
		strings.EqualFold(target.Hostname(), r.home.Hostname())
}

// anywhere is a URL of the description, absolute. One that is no URL is
// none.
func (r resolver) anywhere(raw string) string {
	reference, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || strings.TrimSpace(raw) == "" {
		return ""
	}

	return r.base.ResolveReference(reference).String()
}

// within is a URL of the description that the device is asked at,
// absolute: one that leaves the device is an error. An empty one stays
// empty.
func (r resolver) within(what, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}

	reference, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%w: %s is no URL: %w", ErrAnswer, what, err)
	}

	resolved := r.base.ResolveReference(reference)
	if !r.on(resolved) {
		return "", fmt.Errorf("%w: %s is at %s, which is not the device", ErrAnswer, what, resolved.Host)
	}

	return resolved.String(), nil
}

// A description as it is read, its names in lower case.
type (
	xmlDevice struct {
		UDN              string       `xml:"udn"`
		Type             string       `xml:"devicetype"`
		FriendlyName     string       `xml:"friendlyname"`
		Manufacturer     string       `xml:"manufacturer"`
		ManufacturerURL  string       `xml:"manufacturerurl"`
		ModelName        string       `xml:"modelname"`
		ModelNumber      string       `xml:"modelnumber"`
		ModelDescription string       `xml:"modeldescription"`
		ModelURL         string       `xml:"modelurl"`
		SerialNumber     string       `xml:"serialnumber"`
		UPC              string       `xml:"upc"`
		PresentationURL  string       `xml:"presentationurl"`
		Icons            []xmlIcon    `xml:"iconlist>icon"`
		Services         []xmlService `xml:"servicelist>service"`
		Devices          []xmlDevice  `xml:"devicelist>device"`
	}
	xmlIcon struct {
		MIME   string `xml:"mimetype"`
		Width  string `xml:"width"`
		Height string `xml:"height"`
		Depth  string `xml:"depth"`
		URL    string `xml:"url"`
	}
	xmlService struct {
		Type    string `xml:"servicetype"`
		ID      string `xml:"serviceid"`
		Control string `xml:"controlurl"`
		Events  string `xml:"eventsuburl"`
		SCPD    string `xml:"scpdurl"`
	}
)

// device is what was read as a Device, its URLs absolute, itself and
// those embedded in it each given its own element, the next of those
// that are left.
func (x *xmlDevice) device(from resolver, elements *[]place) (Device, error) {
	if len(*elements) == 0 {
		return Device{}, fmt.Errorf("%w: a device that is not where the description's devices are", ErrAnswer)
	}

	text := strings.TrimSpace
	own := (*elements)[0]
	*elements = (*elements)[1:]

	device := Device{
		UDN: text(x.UDN), Type: text(x.Type), FriendlyName: text(x.FriendlyName),
		Manufacturer: text(x.Manufacturer), ManufacturerURL: text(x.ManufacturerURL),
		ModelName: text(x.ModelName), ModelNumber: text(x.ModelNumber),
		ModelDescription: text(x.ModelDescription), ModelURL: text(x.ModelURL),
		SerialNumber: text(x.SerialNumber), UPC: text(x.UPC),
		PresentationURL: from.anywhere(x.PresentationURL),
		source:          own.element, scope: own.scope,
	}

	for _, icon := range x.Icons {
		where, err := from.within("an icon of "+device.UDN, icon.URL)
		if err != nil {
			return Device{}, err
		}

		device.Icons = append(device.Icons, Icon{
			MIME: text(icon.MIME), Width: number(icon.Width), Height: number(icon.Height), Depth: number(icon.Depth),
			URL: where,
		})
	}

	for _, service := range x.Services {
		made, err := service.service(from)
		if err != nil {
			return Device{}, err
		}

		device.Services = append(device.Services, made)
	}

	for position := range x.Devices {
		embedded, err := x.Devices[position].device(from, elements)
		if err != nil {
			return Device{}, err
		}

		device.Devices = append(device.Devices, embedded)
	}

	return device, nil
}

func (x xmlService) service(from resolver) (Service, error) {
	service := Service{Type: strings.TrimSpace(x.Type), ID: strings.TrimSpace(x.ID)}

	var failed error

	for _, one := range []struct {
		into *string
		what string
		raw  string
	}{
		{&service.Control, "the control URL", x.Control},
		{&service.Events, "the events URL", x.Events},
		{&service.SCPD, "the description URL", x.SCPD},
	} {
		where, err := from.within(one.what+" of "+service.Type, one.raw)
		*one.into = where
		failed = errors.Join(failed, err)
	}

	return service, failed
}

// number is a count a description gives, or zero.
func number(raw string) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value < 0 {
		return 0
	}

	return value
}
