// Package upnptest is a UPnP device for tests: an HTTP server on the
// loopback that serves a description and its services' descriptions and
// answers actions, as the standard has a device do, and that a test can
// make answer anything else.
package upnptest

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"

	upnp "github.com/forkcloser/go-upnp"
)

// What a new Device is: a light with a switch, and a dimmer embedded in
// it.
const (
	UDN         = "uuid:00000000-0000-4000-8000-000000000001"
	EmbeddedUDN = "uuid:00000000-0000-4000-8000-000000000002"

	LightType  = "urn:schemas-upnp-org:device:BinaryLight:1"
	DimmerType = "urn:schemas-upnp-org:device:DimmableLight:1"

	SwitchType  = "urn:schemas-upnp-org:service:SwitchPower:1"
	DimmingType = "urn:schemas-upnp-org:service:Dimming:1"

	// DescriptionPath is where the description is served.
	DescriptionPath = "/description.xml"
	// SwitchControl, SwitchEvents and SwitchSCPD are the switch's URLs,
	// and DimmingControl and DimmingSCPD the dimmer's, which has no events.
	SwitchControl  = "/control/switch"
	SwitchEvents   = "/events/switch"
	SwitchSCPD     = "/scpd/switch.xml"
	DimmingControl = "/control/dimming"
	DimmingSCPD    = "/scpd/dimming.xml"
)

// Description is the description a new Device serves. Its URLs are
// relative, as most devices write them.
const Description = `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
  <specVersion><major>1</major><minor>1</minor></specVersion>
  <device>
    <deviceType>` + LightType + `</deviceType>
    <friendlyName>A light</friendlyName>
    <manufacturer>A maker</manufacturer>
    <manufacturerURL>http://maker.example/</manufacturerURL>
    <modelDescription>A light for tests</modelDescription>
    <modelName>Light</modelName>
    <modelNumber>1</modelNumber>
    <modelURL>http://maker.example/light</modelURL>
    <serialNumber>0001</serialNumber>
    <UDN>` + UDN + `</UDN>
    <UPC>000000000001</UPC>
    <firmwareVersion>1.2.3</firmwareVersion>
    <iconList>
      <icon><mimetype>image/png</mimetype><width>48</width><height>32</height><depth>24</depth><url>/icon.png</url></icon>
    </iconList>
    <serviceList>
      <service>
        <serviceType>` + SwitchType + `</serviceType>
        <serviceId>urn:upnp-org:serviceId:SwitchPower</serviceId>
        <SCPDURL>` + SwitchSCPD + `</SCPDURL>
        <controlURL>` + SwitchControl + `</controlURL>
        <eventSubURL>` + SwitchEvents + `</eventSubURL>
      </service>
    </serviceList>
    <deviceList>
      <device>
        <deviceType>` + DimmerType + `</deviceType>
        <friendlyName>Its dimmer</friendlyName>
        <UDN>` + EmbeddedUDN + `</UDN>
        <firmwareVersion>4.5.6</firmwareVersion>
        <serviceList>
          <service>
            <serviceType>` + DimmingType + `</serviceType>
            <serviceId>urn:upnp-org:serviceId:Dimming</serviceId>
            <SCPDURL>` + DimmingSCPD + `</SCPDURL>
            <controlURL>` + DimmingControl + `</controlURL>
            <eventSubURL></eventSubURL>
          </service>
        </serviceList>
      </device>
    </deviceList>
    <presentationURL>/</presentationURL>
  </device>
</root>`

// SwitchDescription is the description of the switch a new Device
// serves.
const SwitchDescription = `<?xml version="1.0"?>
<scpd xmlns="urn:schemas-upnp-org:service-1-0">
  <specVersion><major>1</major><minor>1</minor></specVersion>
  <actionList>
    <action>
      <name>SetTarget</name>
      <argumentList>
        <argument><name>newTargetValue</name><direction>in</direction><relatedStateVariable>Target</relatedStateVariable></argument>
      </argumentList>
    </action>
    <action>
      <name>GetStatus</name>
      <argumentList>
        <argument><name>ResultStatus</name><direction>out</direction><retval/><relatedStateVariable>Status</relatedStateVariable></argument>
      </argumentList>
    </action>
    <action>
      <name>SetBoth</name>
      <argumentList>
        <argument><name>First</name><direction>in</direction><relatedStateVariable>Target</relatedStateVariable></argument>
        <argument><name>Second</name><direction>in</direction><relatedStateVariable>Level</relatedStateVariable></argument>
        <argument><name>Was</name><direction>out</direction><relatedStateVariable>Status</relatedStateVariable></argument>
      </argumentList>
    </action>
  </actionList>
  <serviceStateTable>
    <stateVariable sendEvents="no"><name>Target</name><dataType>boolean</dataType><defaultValue>0</defaultValue></stateVariable>
    <stateVariable sendEvents="yes"><name>Status</name><dataType>boolean</dataType></stateVariable>
    <stateVariable><name>Level</name><dataType>ui1</dataType>
      <allowedValueRange><minimum>0</minimum><maximum>100</maximum><step>5</step></allowedValueRange></stateVariable>
    <stateVariable sendEvents="no"><name>Mode</name><dataType>string</dataType>
      <allowedValueList><allowedValue>Day</allowedValue><allowedValue>Night</allowedValue></allowedValueList></stateVariable>
  </serviceStateTable>
</scpd>`

// Call is one action a Device was asked for.
type Call struct {
	// Path is the URL's path it was posted to.
	Path string
	// Type and Action are what the request's body names: the namespace
	// and the name of the element in its SOAP body.
	Type   string
	Action string
	// In is its arguments, in the order they came.
	In upnp.Arguments
	// SOAPAction and ContentType are the request's headers, as sent.
	SOAPAction  string
	ContentType string
}

// Device is a UPnP device on the loopback. A new one serves Description
// and SwitchDescription, and answers every action with UPnP's error 401,
// until a test says otherwise.
type Device struct {
	server *httptest.Server
	client *http.Client

	mu      sync.Mutex
	served  map[string]served
	answers map[string]func(in upnp.Arguments) (upnp.Arguments, error)
	calls   []Call
}

// served is what a path answers in place of what the Device would.
type served struct {
	status   int
	body     string
	location string
}

// New starts a Device. Close ends it.
func New() *Device {
	device := &Device{
		served:  map[string]served{},
		answers: map[string]func(in upnp.Arguments) (upnp.Arguments, error){},
	}

	device.Serve(DescriptionPath, http.StatusOK, Description)
	device.Serve(SwitchSCPD, http.StatusOK, SwitchDescription)

	device.server = httptest.NewServer(http.HandlerFunc(device.handle))
	device.client = device.server.Client()
	device.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	return device
}

// Close stops the Device, and ends what it was still answering.
func (d *Device) Close() {
	d.server.CloseClientConnections()
	d.server.Close()
}

// URL is the Device's address, as "http://127.0.0.1:port".
func (d *Device) URL() string { return d.server.URL }

// Location is the URL of its description.
func (d *Device) Location() string { return d.server.URL + DescriptionPath }

// Client is an HTTP client for it, of the kind a device should be asked
// through: it follows no redirect, and hands one back as it came.
func (d *Device) Client() *http.Client { return d.client }

// Redirect makes a path answer a status with a Location, whatever is
// asked of it: a device sending its caller elsewhere.
func (d *Device) Redirect(path string, status int, location string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.served[path] = served{status: status, location: location}
}

// Serve makes a path answer a status and a body, whatever is asked of
// it: a description of the test's own, a service's, or what no device
// should answer. A status of zero gives the path back to the Device.
func (d *Device) Serve(path string, status int, body string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if status == 0 {
		delete(d.served, path)

		return
	}

	d.served[path] = served{status: status, body: body}
}

// Answer makes the Device answer an action, of any service, with what
// respond returns: the values, in their order, or an error. An error
// that is a *upnp.FaultError is answered as that fault, under its status, or
// 500 when it has none; any other is UPnP's error 501.
func (d *Device) Answer(action string, respond func(in upnp.Arguments) (upnp.Arguments, error)) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.answers[action] = respond
}

// Calls is every action the Device was asked for, in the order they
// came.
func (d *Device) Calls() []Call {
	d.mu.Lock()
	defer d.mu.Unlock()

	return append([]Call(nil), d.calls...)
}

func (d *Device) handle(writer http.ResponseWriter, request *http.Request) {
	d.mu.Lock()
	fixed, isFixed := d.served[request.URL.Path]
	d.mu.Unlock()

	switch {
	case isFixed:
		writer.Header().Set("Content-Type", `text/xml; charset="utf-8"`)

		if fixed.location != "" {
			writer.Header().Set("Location", fixed.location)
		}

		writer.WriteHeader(fixed.status)
		_, _ = io.WriteString(writer, fixed.body)
	case request.Method == http.MethodPost:
		d.control(writer, request)
	default:
		http.NotFound(writer, request)
	}
}

// A SOAP request as it is read: the one element of its body, and the
// elements that one holds.
type (
	asked struct {
		Body askedBody `xml:"Body"`
	}
	askedBody struct {
		Action askedAction `xml:",any"`
	}
	askedAction struct {
		XMLName   xml.Name        `xml:""`
		Arguments []askedArgument `xml:",any"`
	}
	askedArgument struct {
		XMLName xml.Name `xml:""`
		Value   string   `xml:",chardata"`
	}
)

// control answers one action.
func (d *Device) control(writer http.ResponseWriter, request *http.Request) {
	var envelope asked

	body, _ := io.ReadAll(request.Body)
	if xml.Unmarshal(body, &envelope) != nil || envelope.Body.Action.XMLName.Local == "" {
		http.Error(writer, "not a SOAP request", http.StatusBadRequest)

		return
	}

	call := Call{
		Path:        request.URL.Path,
		Type:        envelope.Body.Action.XMLName.Space,
		Action:      envelope.Body.Action.XMLName.Local,
		SOAPAction:  request.Header.Get("SoapAction"),
		ContentType: request.Header.Get("Content-Type"),
	}

	for _, argument := range envelope.Body.Action.Arguments {
		call.In = append(call.In, upnp.Argument{Name: argument.XMLName.Local, Value: argument.Value})
	}

	d.mu.Lock()
	d.calls = append(d.calls, call)
	respond := d.answers[call.Action]
	d.mu.Unlock()

	if respond == nil {
		refuse(writer, &upnp.FaultError{Code: invalidAction, Description: "Invalid Action"})

		return
	}

	out, err := respond(call.In)
	if err != nil {
		var refused *upnp.FaultError
		if !errors.As(err, &refused) {
			refused = &upnp.FaultError{Code: actionFailed, Description: "Action Failed"}
		}

		refuse(writer, refused)

		return
	}

	var answer bytes.Buffer

	answer.WriteString(envelopeOpen + `<u:` + call.Action + `Response xmlns:u="` + call.Type + `">`)

	for _, value := range out {
		answer.WriteString("<" + value.Name + ">")
		_ = xml.EscapeText(&answer, []byte(value.Value))
		answer.WriteString("</" + value.Name + ">")
	}

	answer.WriteString(`</u:` + call.Action + `Response>` + envelopeClose)

	writer.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	_, _ = writer.Write(answer.Bytes())
}

// UPnP's errors for an action the service does not have, and for one
// that failed.
const (
	invalidAction = 401
	actionFailed  = 501
)

const (
	envelopeOpen = `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" ` +
		`s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`
	envelopeClose = `</s:Body></s:Envelope>`
)

// refuse answers a fault as the standard writes one.
func refuse(writer http.ResponseWriter, refused *upnp.FaultError) {
	status := refused.Status
	if status == 0 {
		status = http.StatusInternalServerError
	}

	var description bytes.Buffer

	_ = xml.EscapeText(&description, []byte(refused.Description))

	writer.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	writer.WriteHeader(status)
	_, _ = io.WriteString(writer, envelopeOpen+
		`<s:Fault><faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring><detail>`+
		`<UPnPError xmlns="urn:schemas-upnp-org:control-1-0"><errorCode>`+strconv.Itoa(refused.Code)+`</errorCode>`+
		`<errorDescription>`+strings.TrimSpace(description.String())+`</errorDescription></UPnPError>`+
		`</detail></s:Fault>`+envelopeClose)
}
