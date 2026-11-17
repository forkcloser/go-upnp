package upnp

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Argument is one argument of an action, or one value of its answer.
type Argument struct {
	// Name is its name, as the service's description gives it.
	Name string
	// Value is its value, as text: UPnP has no other. A value that is a
	// document of its own, as some services answer, is that document.
	Value string
}

// Arguments is the arguments of an action, or the values of its answer,
// in their order: the order is part of the protocol.
type Arguments []Argument

// Get is the value of that name, the first when there are several.
func (a Arguments) Get(name string) (string, bool) {
	for _, argument := range a {
		if argument.Name == name {
			return argument.Value, true
		}
	}

	return "", false
}

// name is what an action or an argument may be called: what XML takes
// for an element's name with no prefix, so that none can write markup.
var name = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)

// Call invokes an action of the service, with its arguments in the order
// given, and returns what the device answered, in the device's order.
//
// The order given has to be the order the service's description lists
// the arguments in: devices read them by place as often as by name.
//
// A device that refuses answers a *FaultError, which is an ErrRefused and
// carries its UPnP error; a refusal that says no more than an HTTP
// status is a *StatusError. A failure to reach the device is the
// client's own error.
func (s Service) Call(ctx context.Context, client Doer, action string, in ...Argument) (Arguments, error) {
	if client == nil {
		return nil, fmt.Errorf("%s: %w: no client", action, ErrInvalid)
	}

	request, err := s.request(ctx, action, in)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}

	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}

	defer func() { _ = response.Body.Close() }()

	if err = strayed(request.URL, response); err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}

	body, err := read(response)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}

	success := response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices
	answer, refused, err := answered(body)

	switch {
	case refused != nil:
		// A fault is one under any status: some devices send theirs under 200.
		refused.Status = response.StatusCode

		return nil, fmt.Errorf("%s: %w", action, refused)
	case !success:
		return nil, fmt.Errorf("%s: %w", action, &StatusError{Status: response.StatusCode})
	case err != nil:
		return nil, fmt.Errorf("%s: %w", action, err)
	}

	return answer, nil
}

// request is the call of an action as HTTP.
func (s Service) request(ctx context.Context, action string, arguments []Argument) (*http.Request, error) {
	where, err := absolute(s.Control)
	if err != nil {
		return nil, fmt.Errorf("the control URL of %s: %w", s.Type, err)
	}

	// The type is written into an attribute and a header, both quoted,
	// and neither takes a control character.
	if s.Type == "" || strings.ContainsAny(s.Type, "\"<>&") || strings.ContainsFunc(s.Type, unicode.IsControl) {
		return nil, fmt.Errorf("%w: the service type %q", ErrInvalid, s.Type)
	}

	if !name.MatchString(action) {
		return nil, fmt.Errorf("%w: an action named %q", ErrInvalid, action)
	}

	var body bytes.Buffer

	body.WriteString(`<?xml version="1.0" encoding="utf-8"?>` +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" ` +
		`s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>` +
		`<u:` + action + ` xmlns:u="` + s.Type + `">`)

	for _, argument := range arguments {
		if !name.MatchString(argument.Name) {
			return nil, fmt.Errorf("%w: an argument named %q", ErrInvalid, argument.Name)
		}

		body.WriteString("<" + argument.Name + ">")

		if err = xml.EscapeText(&body, []byte(argument.Value)); err != nil {
			return nil, fmt.Errorf("%w: the value of %s: %w", ErrInvalid, argument.Name, err)
		}

		body.WriteString("</" + argument.Name + ">")
	}

	body.WriteString(`</u:` + action + `></s:Body></s:Envelope>`)

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, where.String(), bytes.NewReader(body.Bytes()))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	request.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	// Written as the standard writes it: Set would make it Soapaction,
	// which is the same header to HTTP and not to every device.
	request.Header["SOAPACTION"] = []string{`"` + s.Type + "#" + action + `"`}

	return request, nil
}

// answered reads a SOAP answer: the values of the action's response, or
// the fault the device raised in its place.
func answered(body []byte) (Arguments, *FaultError, error) {
	reading := decoder(body)

	if err := enter(reading, "body"); err != nil {
		return nil, nil, err
	}

	response, err := child(reading)
	if err != nil || response == nil {
		return nil, nil, fmt.Errorf("%w: a SOAP body that holds nothing", ErrAnswer)
	}

	if strings.EqualFold(response.Name.Local, "fault") {
		return nil, fault(reading), nil
	}

	answer, err := values(reading)

	return answer, nil, err
}

// values reads the elements inside the one that is open, up to its end,
// each as a name and what it holds.
func values(reading *xml.Decoder) (Arguments, error) {
	var read Arguments

	for {
		element, err := child(reading)
		if err != nil {
			return nil, err
		}

		if element == nil {
			return read, nil
		}

		var value answeredValue

		if err = reading.DecodeElement(&value, element); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrAnswer, err)
		}

		// A value is text. One that holds elements is a device's document
		// sent bare where it should have been escaped: it is handed on as
		// it was written.
		if len(value.Nested) > 0 {
			value.Text = value.Inner
		}

		read = append(read, Argument{Name: element.Name.Local, Value: value.Text})
	}
}

// answeredValue is one value of an answer: its text, and what it holds
// when it holds elements.
type (
	answeredValue struct {
		Text   string           `xml:",chardata"`
		Inner  string           `xml:",innerxml"`
		Nested []answeredNested `xml:",any"`
	}
	answeredNested struct {
		XMLName xml.Name `xml:""`
	}
)

// enter reads up to the start of the first element of that name, in any
// case and at any depth.
func enter(reading *xml.Decoder, wanted string) error {
	for {
		token, err := reading.Token()
		if err != nil {
			return fmt.Errorf("%w: no SOAP %s: %w", ErrAnswer, wanted, err)
		}

		if element, ok := token.(xml.StartElement); ok && strings.EqualFold(element.Name.Local, wanted) {
			return nil
		}
	}
}

// child reads up to the start of the next element inside the one that is
// open, and returns none at that one's end.
func child(reading *xml.Decoder) (*xml.StartElement, error) {
	for {
		token, err := reading.Token()
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrAnswer, err)
		}

		switch element := token.(type) {
		case xml.StartElement:
			return &element, nil
		case xml.EndElement:
			return nil, nil //nolint:nilnil // no element and no error: the end of the one that was open
		}
	}
}

// fault reads what is inside a SOAP fault: the UPnP error, wherever in
// it and in whatever case.
func fault(reading *xml.Decoder) *FaultError {
	var (
		refused FaultError
		path    []string
	)

	for {
		token, err := reading.Token()
		if err != nil {
			// A fault that is cut short, or badly written past its start,
			// is still a fault: the device said no.
			return &refused
		}

		switch element := token.(type) {
		case xml.StartElement:
			path = append(path, strings.ToLower(element.Name.Local))
		case xml.EndElement:
			if len(path) == 0 {
				return &refused
			}

			path = path[:len(path)-1]
		case xml.CharData:
			if len(path) == 0 {
				continue
			}

			switch path[len(path)-1] {
			case "errorcode":
				refused.Code, _ = strconv.Atoi(strings.TrimSpace(string(element)))
			case "errordescription":
				refused.Description = strings.TrimSpace(string(element))
			}
		}
	}
}
