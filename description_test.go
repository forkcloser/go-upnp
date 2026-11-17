package upnp_test

import (
	"encoding/xml"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	upnp "github.com/forkcloser/go-upnp"
	"github.com/forkcloser/go-upnp/upnptest"
)

// wait bounds every wait of a test; generous, for a loaded runner.
const wait = 30 * time.Second

// doer is a device made of one function.
type doer func(request *http.Request) (*http.Response, error)

func (d doer) Do(request *http.Request) (*http.Response, error) { return d(request) }

func device(t *testing.T) *upnptest.Device {
	t.Helper()

	fake := upnptest.New()
	t.Cleanup(fake.Close)

	return fake
}

// TestDescribe: a description is read whole, the devices embedded in it
// too, and every URL in it is absolute.
func TestDescribe(t *testing.T) {
	t.Parallel()

	fake := device(t)

	root, err := upnp.Describe(t.Context(), fake.Client(), fake.Location())
	if err != nil {
		t.Fatal(err)
	}

	got := []string{
		root.UDN, root.Type, root.FriendlyName, root.Manufacturer, root.ManufacturerURL, root.ModelName,
		root.ModelNumber, root.ModelDescription, root.ModelURL, root.SerialNumber, root.UPC, root.PresentationURL,
	}
	want := []string{
		upnptest.UDN, upnptest.LightType, "A light", "A maker", "http://maker.example/", "Light",
		"1", "A light for tests", "http://maker.example/light", "0001", "000000000001", fake.URL() + "/",
	}

	if !slices.Equal(got, want) {
		t.Errorf("the root device = %q\nwant %q", got, want)
	}

	icon := upnp.Icon{MIME: "image/png", Width: 48, Height: 32, Depth: 24, URL: fake.URL() + "/icon.png"}
	if !slices.Equal(root.Icons, []upnp.Icon{icon}) {
		t.Errorf("its icons = %+v", root.Icons)
	}

	power := upnp.Service{
		Type: upnptest.SwitchType, ID: "urn:upnp-org:serviceId:SwitchPower",
		Control: fake.URL() + upnptest.SwitchControl, Events: fake.URL() + upnptest.SwitchEvents,
		SCPD: fake.URL() + upnptest.SwitchSCPD,
	}
	if !slices.Equal(root.Services, []upnp.Service{power}) {
		t.Errorf("its services = %+v", root.Services)
	}

	// A service with no events has no events URL, and not the device's.
	dimming := upnp.Service{
		Type: upnptest.DimmingType, ID: "urn:upnp-org:serviceId:Dimming",
		Control: fake.URL() + upnptest.DimmingControl, SCPD: fake.URL() + upnptest.DimmingSCPD,
	}
	if len(root.Devices) != 1 || root.Devices[0].UDN != upnptest.EmbeddedUDN ||
		root.Devices[0].Type != upnptest.DimmerType || !slices.Equal(root.Devices[0].Services, []upnp.Service{dimming}) {
		t.Errorf("its embedded devices = %+v", root.Devices)
	}

	var all []string
	for one := range root.All() {
		all = append(all, one.UDN)
	}

	if !slices.Equal(all, []string{upnptest.UDN, upnptest.EmbeddedUDN}) {
		t.Errorf("All = %v", all)
	}

	// All stops when told to.
	for range root.All() {
		break
	}
}

// TestFindingAService: a service is found by its type, in the device or
// below it, a later version serving for an earlier one and not the
// other way; and by its ID.
func TestFindingAService(t *testing.T) {
	t.Parallel()

	fake := device(t)
	fake.Serve(upnptest.DescriptionPath, http.StatusOK,
		strings.Replace(upnptest.Description, upnptest.DimmingType, "urn:schemas-upnp-org:service:Dimming:2", 1))

	root, err := upnp.Describe(t.Context(), fake.Client(), fake.Location())
	if err != nil {
		t.Fatal(err)
	}

	for wanted, found := range map[string]string{
		upnptest.SwitchType:                          upnptest.SwitchControl,
		"urn:upnp-org:serviceId:SwitchPower":         upnptest.SwitchControl,
		"urn:schemas-upnp-org:service:Dimming:1":     upnptest.DimmingControl,
		"urn:schemas-upnp-org:service:Dimming:2":     upnptest.DimmingControl,
		"urn:schemas-upnp-org:service:Dimming:3":     "",
		"urn:schemas-upnp-org:service:SwitchPower:2": "",
		"urn:schemas-upnp-org:service:Dimming":       "",
		"urn:schemas-upnp-org:service:Dimming:":      "",
		"urn:schemas-upnp-org:service:Dimming:x":     "",
		"Dimming":                                    "",
		"":                                           "",
	} {
		service, ok := root.Service(wanted)
		if ok != (found != "") || ok && service.Control != fake.URL()+found {
			t.Errorf("Service(%q) = %+v, %t; want the one at %q", wanted, service, ok, found)
		}
	}
}

// TestDecode: a maker's elements are read from the device's own element,
// the root's and an embedded one's each its own.
func TestDecode(t *testing.T) {
	t.Parallel()

	fake := device(t)

	root, err := upnp.Describe(t.Context(), fake.Client(), fake.Location())
	if err != nil {
		t.Fatal(err)
	}

	type makers struct {
		Firmware string `xml:"firmwareVersion"`
	}

	var own, embedded makers

	if err = root.Decode(&own); err != nil || own.Firmware != "1.2.3" {
		t.Errorf("Decode of the root device = %+v, %v", own, err)
	}

	if err = root.Devices[0].Decode(&embedded); err != nil || embedded.Firmware != "4.5.6" {
		t.Errorf("Decode of the embedded device = %+v, %v", embedded, err)
	}

	if err = (&upnp.Device{}).Decode(&own); !errors.Is(err, upnp.ErrInvalid) {
		t.Errorf("Decode of a device that was not read = %v, want ErrInvalid", err)
	}

	if err = root.Decode(own); err == nil {
		t.Error("Decode into what is no pointer gave no error")
	}
}

// TestDecodeKnowsItsNamespaces: a device's element is read under the
// namespaces the description declares above it, the root's and an
// enclosing device's, a later declaration over an earlier one: a maker's
// library tells its elements from another's by them.
func TestDecodeKnowsItsNamespaces(t *testing.T) {
	t.Parallel()

	root, err := upnp.Describe(t.Context(), said(
		`<root xmlns="urn:schemas-upnp-org:device-1-0" xmlns:dlna="urn:schemas-dlna-org:device-1-0" xmlns:m="urn:first">`+
			`<device xmlns:m="urn:maker&amp;&quot;co&quot;"><UDN>uuid:1</UDN><dlna:X_DLNADOC>DMR-1.50</dlna:X_DLNADOC><m:model>one</m:model>`+
			`<deviceList><device><UDN>uuid:2</UDN><dlna:X_DLNADOC>DMS-1.50</dlna:X_DLNADOC><m:model>two</m:model>`+
			`<n:other xmlns:n="urn:own">own</n:other></device></deviceList></device></root>`,
	), "http://192.0.2.1/d.xml")
	if err != nil {
		t.Fatal(err)
	}

	type named struct {
		XMLName xml.Name `xml:"urn:schemas-upnp-org:device-1-0 device"`
		UDN     string   `xml:"urn:schemas-upnp-org:device-1-0 UDN"`
		DLNA    string   `xml:"urn:schemas-dlna-org:device-1-0 X_DLNADOC"`
		Model   string   `xml:"urn:maker&\"co\" model"`
		Stale   string   `xml:"urn:first model"`
		Other   string   `xml:"urn:own other"`
	}

	var own, embedded named

	if err = root.Decode(
		&own,
	); err != nil || own.UDN != "uuid:1" || own.DLNA != "DMR-1.50" || own.Model != "one" ||
		own.Stale != "" {
		t.Errorf("Decode of the root device, by its namespaces = %+v, %v", own, err)
	}

	if err = root.Devices[0].Decode(&embedded); err != nil || embedded.UDN != "uuid:2" || embedded.DLNA != "DMS-1.50" ||
		embedded.Model != "two" || embedded.Stale != "" || embedded.Other != "own" {
		t.Errorf("Decode of the embedded device, by its namespaces = %+v, %v", embedded, err)
	}
}

// TestOneDeviceSeveralCallers: a device and its client serve several
// goroutines at once.
func TestOneDeviceSeveralCallers(t *testing.T) {
	t.Parallel()

	fake := device(t)

	var calling sync.WaitGroup

	for range 8 {
		calling.Go(func() {
			if _, err := upnp.Describe(t.Context(), fake.Client(), fake.Location()); err != nil {
				t.Error(err)
			}
		})
	}

	calling.Wait()
}

// TestDecodeCountsDevicesOnly: an element a maker calls device, where
// the standard has none, is not taken for one of the description's.
func TestDecodeCountsDevicesOnly(t *testing.T) {
	t.Parallel()

	fake := device(t)
	fake.Serve(upnptest.DescriptionPath, http.StatusOK, strings.Replace(upnptest.Description,
		"<iconList>", "<extra><device><firmwareVersion>no</firmwareVersion></device></extra><iconList>", 1))

	root, err := upnp.Describe(t.Context(), fake.Client(), fake.Location())
	if err != nil {
		t.Fatal(err)
	}

	var embedded struct {
		Firmware string `xml:"firmwareVersion"`
	}

	if err = root.Devices[0].Decode(&embedded); err != nil || embedded.Firmware != "4.5.6" {
		t.Errorf("Decode of the embedded device = %+v, %v", embedded, err)
	}
}

// TestWhatDevicesWrite: a description is read as devices write them: a
// byte order mark ahead of it, names in another case, text around it,
// an encoding of one byte a character, and whatever follows its end.
func TestWhatDevicesWrite(t *testing.T) {
	t.Parallel()

	for name, written := range map[string]string{
		"a byte order mark": "\xef\xbb\xbf" + upnptest.Description,
		"whatever follows":  upnptest.Description + "\x00\x00 trailing",
		"names in any case": strings.NewReplacer(
			"<UDN>", "<udn>", "</UDN>", "</udn>", "controlURL", "ControlUrl", "serviceList", "ServiceList",
			"<root ", "<ROOT ", "</root>", "</ROOT>",
		).Replace(upnptest.Description),
		"latin 1": strings.Replace(strings.Replace(upnptest.Description,
			`<?xml version="1.0"?>`, `<?xml version="1.0" encoding="ISO-8859-1"?>`, 1), "A maker", "Caf\xe9", 1),
	} {
		fake := device(t)
		fake.Serve(upnptest.DescriptionPath, http.StatusOK, written)

		root, err := upnp.Describe(t.Context(), fake.Client(), fake.Location())
		if err != nil {
			t.Errorf("with %s: %v", name, err)

			continue
		}

		if root.UDN != upnptest.UDN || len(root.Services) != 1 ||
			root.Services[0].Control != fake.URL()+upnptest.SwitchControl {
			t.Errorf("with %s: %+v", name, root)
		}

		if name == "latin 1" && root.Manufacturer != "Café" {
			t.Errorf("a maker written in Latin 1 = %q", root.Manufacturer)
		}

		var makers struct {
			Firmware string `xml:"firmwareVersion"`
		}

		if err = root.Decode(&makers); err != nil || makers.Firmware != "1.2.3" {
			t.Errorf("with %s: Decode = %+v, %v", name, makers, err)
		}
	}
}

// TestWhereAURLMayPoint: a base the device gives is taken when it is on
// the device; a service or an icon that is anywhere else refuses the
// description, and a page for a person may be anywhere.
func TestWhereAURLMayPoint(t *testing.T) {
	t.Parallel()

	fake := device(t)
	based := func(base string) string {
		return strings.Replace(upnptest.Description, "<device>", "<URLBase>"+base+"</URLBase><device>", 1)
	}

	host := strings.TrimPrefix(fake.URL(), "http://")
	address, _, _ := strings.Cut(host, ":")

	for name, where := range map[string]struct {
		written string
		control string
	}{
		"a base on the device":           {based(fake.URL() + "/base/"), fake.URL() + upnptest.SwitchControl},
		"a base on another port":         {based("http://" + address + ":1/"), "http://" + address + ":1" + upnptest.SwitchControl},
		"a base elsewhere, not taken":    {based("http://192.0.2.1:80/"), fake.URL() + upnptest.SwitchControl},
		"a base that is no URL":          {based("::"), fake.URL() + upnptest.SwitchControl},
		"a relative URL with no slash":   {strings.Replace(based(fake.URL()+"/base/x"), upnptest.SwitchControl, "control", 1), fake.URL() + "/base/control"},
		"an absolute URL on the device":  {strings.Replace(upnptest.Description, upnptest.SwitchControl, fake.URL()+"/c", 1), fake.URL() + "/c"},
		"a page for a person, elsewhere": {strings.Replace(upnptest.Description, "<presentationURL>/<", "<presentationURL>http://192.0.2.1/<", 1), fake.URL() + upnptest.SwitchControl},
		"a page for a person, no URL":    {strings.Replace(upnptest.Description, "<presentationURL>/<", "<presentationURL>::<", 1), fake.URL() + upnptest.SwitchControl},
	} {
		fake.Serve(upnptest.DescriptionPath, http.StatusOK, where.written)

		root, err := upnp.Describe(t.Context(), fake.Client(), fake.Location())
		if err != nil || root.Services[0].Control != where.control {
			t.Errorf("with %s: %v, control at %v, want %s", name, err, root, where.control)
		}
	}

	for name, written := range map[string]string{
		"a control URL elsewhere": strings.Replace(upnptest.Description, upnptest.SwitchControl, "http://192.0.2.1/control", 1),
		"an events URL elsewhere": strings.Replace(upnptest.Description, upnptest.SwitchEvents, "http://192.0.2.1/events", 1),
		"a description elsewhere": strings.Replace(upnptest.Description, upnptest.SwitchSCPD, "//192.0.2.1/scpd.xml", 1),
		"an icon elsewhere":       strings.Replace(upnptest.Description, "/icon.png", "http://192.0.2.1/icon.png", 1),
		"an embedded one's":       strings.Replace(upnptest.Description, upnptest.DimmingControl, "http://192.0.2.1/control", 1),
		"a URL that is no http":   strings.Replace(upnptest.Description, upnptest.SwitchControl, "file:///etc/passwd", 1),
		"a URL that is none":      strings.Replace(upnptest.Description, upnptest.SwitchControl, "http://[::1", 1),
		"an icon that is none":    strings.Replace(upnptest.Description, "/icon.png", "http://[::1", 1),
	} {
		fake.Serve(upnptest.DescriptionPath, http.StatusOK, written)

		if root, err := upnp.Describe(t.Context(), fake.Client(), fake.Location()); !errors.Is(err, upnp.ErrAnswer) {
			t.Errorf("with %s: %+v, %v; want ErrAnswer", name, root, err)
		}
	}
}

// TestWhatTheStandardLeavesOpen: the clauses of the readers that no
// device of the tests' own exercises.
func TestWhatTheStandardLeavesOpen(t *testing.T) {
	t.Parallel()

	service := func(urls string) string {
		return `<root><device><UDN>uuid:1</UDN><presentationURL>http://elsewhere.example/page</presentationURL>` +
			`<iconList><icon><width>-5</width><url>/i.png</url></icon></iconList><serviceList><service>` +
			`<serviceType>urn:schemas-upnp-org:service:A:1</serviceType>` + urls + `</service></serviceList></device></root>`
	}

	// On the device's host and under another scheme is no URL of a service.
	_, err := upnp.Describe(
		t.Context(),
		said(service(`<controlURL>ftp://192.0.2.1/control</controlURL>`)),
		"http://192.0.2.1/d.xml",
	)
	if !errors.Is(err, upnp.ErrAnswer) {
		t.Errorf("a control URL under ftp on the device's host = %v, want ErrAnswer", err)
	}

	// The host is the same whatever its case; a page for a person may be
	// anywhere; a count below zero is none.
	root, err := upnp.Describe(t.Context(),
		said(service(`<controlURL>http://DEVICE.example:8080/control</controlURL>`)), "http://Device.Example/d.xml")
	if err != nil || root.PresentationURL != "http://elsewhere.example/page" || len(root.Icons) != 1 ||
		root.Icons[0].Width != 0 {
		t.Errorf("the device's host in another case = %+v, %v", root, err)
	}

	// A list of devices that is in no device holds none of the
	// description's; siblings are told in the order written, and each
	// decodes its own element.
	root, err = upnp.Describe(t.Context(), said(
		`<root><deviceList><device><UDN>uuid:stray</UDN></device></deviceList><device><UDN>uuid:1</UDN>`+
			`<junk><deviceList><device><UDN>uuid:stray2</UDN></device></deviceList></junk>`+
			`<deviceList><device><UDN>uuid:2</UDN><deviceList><device><UDN>uuid:3</UDN></device></deviceList></device>`+
			`<device><UDN>uuid:4</UDN></device></deviceList><deviceList><device><UDN>uuid:5</UDN></device></deviceList>`+
			`</device></root>`,
	), "http://192.0.2.1/d.xml")
	if err != nil {
		t.Fatal(err)
	}

	var seen []string

	for one := range root.All() {
		var own struct {
			UDN string `xml:"UDN"`
		}

		if err = one.Decode(&own); err != nil || own.UDN != one.UDN {
			t.Errorf("Decode of %s read %q, %v", one.UDN, own.UDN, err)
		}

		seen = append(seen, one.UDN)
	}

	if !slices.Equal(seen, []string{"uuid:1", "uuid:2", "uuid:3", "uuid:4", "uuid:5"}) {
		t.Errorf("All = %v", seen)
	}

	// A type's kind is matched whatever its case, and a version of too
	// many digits is none.
	root, err = upnp.Describe(t.Context(), said(`<root><device><UDN>uuid:1</UDN><serviceList>`+
		`<service><serviceType>urn:x:service:B:99999999999999999999999</serviceType><controlURL>/b</controlURL></service>`+
		`<service><serviceType>URN:SCHEMAS-UPNP-ORG:SERVICE:A:2</serviceType><controlURL>/a</controlURL></service>`+
		`</serviceList></device></root>`), "http://192.0.2.1/d.xml")
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := root.Service("urn:schemas-upnp-org:service:A:1"); !ok {
		t.Error("a type in capitals at a later version does not serve")
	}

	if _, ok := root.Service("urn:x:service:B:1"); ok {
		t.Error("a version of twenty-three digits serves for version 1")
	}
}

// TestHowDeepDevicesNest: devices embedded sixteen deep are read, each
// with its own element, and deeper is no description.
func TestHowDeepDevicesNest(t *testing.T) {
	t.Parallel()

	nested := func(depth int) string {
		var document strings.Builder

		document.WriteString("<root>")

		for level := range depth {
			document.WriteString("<device><UDN>uuid:" + strconv.Itoa(level) + "</UDN><deviceList>")
		}

		for range depth {
			document.WriteString("</deviceList></device>")
		}

		return document.String() + "</root>"
	}

	root, err := upnp.Describe(t.Context(), said(nested(16)), "http://192.0.2.1/d.xml")
	if err != nil {
		t.Fatal(err)
	}

	count := 0

	for one := range root.All() {
		var own struct {
			UDN string `xml:"UDN"`
		}

		if err = one.Decode(&own); err != nil || own.UDN != "uuid:"+strconv.Itoa(count) {
			t.Fatalf("the device %d deep decoded %q, %v", count, own.UDN, err)
		}

		count++
	}

	if count != 16 {
		t.Errorf("%d devices were read of 16", count)
	}

	if _, err = upnp.Describe(
		t.Context(),
		said(nested(17)),
		"http://192.0.2.1/d.xml",
	); !errors.Is(
		err,
		upnp.ErrAnswer,
	) {
		t.Errorf("devices seventeen deep = %v, want ErrAnswer", err)
	}
}

// TestManyDevices: reading each device of a description of many costs
// what the description does, and not its square.
func TestManyDevices(t *testing.T) {
	t.Parallel()

	const devices = 20000

	var document strings.Builder

	document.WriteString("<root><device><UDN>uuid:0</UDN><deviceList>")

	for index := range devices {
		document.WriteString("<device><UDN>uuid:" + strconv.Itoa(index+1) + "</UDN></device>")
	}

	document.WriteString("</deviceList></device></root>")

	root, err := upnp.Describe(t.Context(), said(document.String()), "http://192.0.2.1/d.xml")
	if err != nil {
		t.Fatal(err)
	}

	began := time.Now()
	count := 0

	for one := range root.All() {
		var own struct {
			UDN string `xml:"UDN"`
		}

		if err = one.Decode(&own); err != nil || own.UDN != one.UDN {
			t.Fatalf("Decode of %s read %q, %v", one.UDN, own.UDN, err)
		}

		count++
	}

	// By the square this is minutes; as it is, a fraction of a second.
	if took := time.Since(began); count != devices+1 || took > wait {
		t.Errorf("%d devices decoded in %s", count, took)
	}
}

// TestARedirect: a client that follows no redirect hands one back as a
// refusal; one that follows them goes where the device sends it, and
// what it brings back from another host is not taken.
func TestARedirect(t *testing.T) {
	t.Parallel()

	fake, elsewhere := device(t), device(t)
	following := &http.Client{}

	// The same machine under another name is another host.
	other := strings.Replace(elsewhere.URL(), "127.0.0.1", "localhost", 1)

	elsewhere.Answer("GetStatus", func(upnp.Arguments) (upnp.Arguments, error) {
		return upnp.Arguments{{Name: "ResultStatus", Value: "from elsewhere"}}, nil
	})

	fake.Redirect(upnptest.DescriptionPath, http.StatusFound, other+upnptest.DescriptionPath)
	fake.Redirect(upnptest.SwitchSCPD, http.StatusFound, other+upnptest.SwitchSCPD)
	fake.Redirect(upnptest.SwitchControl, http.StatusTemporaryRedirect, other+upnptest.SwitchControl)

	var refused *upnp.StatusError

	_, err := upnp.Describe(t.Context(), fake.Client(), fake.Location())
	if !errors.As(err, &refused) || refused.Status != http.StatusFound {
		t.Errorf("a description that redirects, through a client that follows none = %v", err)
	}

	_, err = power(fake).Describe(t.Context(), fake.Client())
	if !errors.As(err, &refused) || refused.Status != http.StatusFound {
		t.Errorf("a service's description that redirects, through a client that follows none = %v", err)
	}

	_, err = power(fake).Call(t.Context(), fake.Client(), "GetStatus")
	if !errors.As(err, &refused) || refused.Status != http.StatusTemporaryRedirect || len(elsewhere.Calls()) != 0 {
		t.Errorf(
			"an action that redirects, through a client that follows none = %v; elsewhere was asked %v",
			err,
			elsewhere.Calls(),
		)
	}

	if root, describeErr := upnp.Describe(
		t.Context(),
		following,
		fake.Location(),
	); !errors.Is(
		describeErr,
		upnp.ErrAnswer,
	) {
		t.Errorf("a description from another host = %+v, %v; want ErrAnswer", root, describeErr)
	}

	if described, describeErr := power(fake).Describe(t.Context(), following); !errors.Is(describeErr, upnp.ErrAnswer) {
		t.Errorf("a service's description from another host = %+v, %v; want ErrAnswer", described, describeErr)
	}

	if out, callErr := power(fake).Call(t.Context(), following, "GetStatus"); !errors.Is(callErr, upnp.ErrAnswer) {
		t.Errorf("an answer from another host = %+v, %v; want ErrAnswer", out, callErr)
	}

	// The host that answered is the one asked whatever its case.
	moved := doer(func(request *http.Request) (*http.Response, error) {
		landed := request.Clone(request.Context())
		landed.URL.Host = "DEVICE.Example:8080"

		response, _ := said(`<e><Body><r><A>1</A></r></Body></e>`).Do(request)
		response.Request = landed

		return response, nil
	})

	out, err := upnp.Service{
		Type:    upnptest.SwitchType,
		Control: "http://device.example/control",
	}.Call(
		t.Context(),
		moved,
		"GetStatus",
	)
	if err != nil || len(out) != 1 {
		t.Errorf("an answer from the host asked, in another case = %+v, %v", out, err)
	}

	// On the device, at another port, is the device.
	fake.Redirect(upnptest.SwitchControl, http.StatusTemporaryRedirect, elsewhere.URL()+upnptest.SwitchControl)

	out, err = power(fake).Call(t.Context(), following, "GetStatus")
	if value, _ := out.Get("ResultStatus"); err != nil || value != "from elsewhere" {
		t.Errorf("an answer from the device's host at another port = %+v, %v", out, err)
	}
}

// TestNotADescription: what is no description of one root device is an
// ErrAnswer, a status that is no success an ErrRefused that carries it,
// and what cannot be asked is not asked.
func TestNotADescription(t *testing.T) {
	t.Parallel()

	fake := device(t)

	for name, written := range map[string]string{
		"no XML":             "not XML",
		"nothing":            "",
		"another document":   upnptest.SwitchDescription,
		"no device":          `<root xmlns="urn:schemas-upnp-org:device-1-0"></root>`,
		"two root devices":   strings.Replace(upnptest.Description, "</root>", "<device><UDN>uuid:2</UDN></device></root>", 1),
		"no UDN":             strings.Replace(upnptest.Description, upnptest.UDN, " ", 1),
		"cut short":          upnptest.Description[:len(upnptest.Description)/2],
		"an encoding unread": strings.Replace(upnptest.Description, `<?xml version="1.0"?>`, `<?xml version="1.0" encoding="utf-16"?>`, 1),
		"more than is read":  upnptest.Description + strings.Repeat(" ", 4<<20-len(upnptest.Description)+1),
	} {
		fake.Serve(upnptest.DescriptionPath, http.StatusOK, written)

		if root, err := upnp.Describe(t.Context(), fake.Client(), fake.Location()); !errors.Is(err, upnp.ErrAnswer) {
			t.Errorf("with %s: %+v, %v; want ErrAnswer", name, root, err)
		}
	}

	// Exactly what is read, and no more.
	fake.Serve(upnptest.DescriptionPath, http.StatusOK,
		upnptest.Description+strings.Repeat(" ", 4<<20-len(upnptest.Description)))

	if _, err := upnp.Describe(t.Context(), fake.Client(), fake.Location()); err != nil {
		t.Errorf("a description of four mebibytes = %v", err)
	}

	fake.Serve(upnptest.DescriptionPath, http.StatusNotFound, "")

	var status *upnp.StatusError

	_, err := upnp.Describe(t.Context(), fake.Client(), fake.Location())
	if !errors.Is(err, upnp.ErrRefused) || !errors.As(err, &status) || status.Status != http.StatusNotFound {
		t.Errorf("a description that is not there = %v, want an ErrRefused that carries 404", err)
	}

	for name, location := range map[string]string{
		"no URL":      "::",
		"no scheme":   "127.0.0.1/description.xml",
		"another one": "ftp://127.0.0.1/description.xml",
		"no host":     "http:///description.xml",
		"nothing":     "",
	} {
		if _, err = upnp.Describe(t.Context(), fake.Client(), location); !errors.Is(err, upnp.ErrInvalid) {
			t.Errorf("Describe at %s = %v, want ErrInvalid", name, err)
		}
	}

	if _, err = upnp.Describe(t.Context(), nil, fake.Location()); !errors.Is(err, upnp.ErrInvalid) {
		t.Errorf("Describe with no client = %v, want ErrInvalid", err)
	}

	fake.Close()

	_, err = upnp.Describe(t.Context(), fake.Client(), fake.Location())
	if err == nil || errors.Is(err, upnp.ErrAnswer) || errors.Is(err, upnp.ErrRefused) ||
		errors.Is(err, upnp.ErrInvalid) {
		t.Errorf("Describe of a device that is gone = %v, want the client's own error", err)
	}
}
