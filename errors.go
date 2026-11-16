package upnp

import (
	"errors"
	"fmt"
	"net/http"
)

// The errors, to test with errors.Is.
var (
	// ErrInvalid: what was asked cannot be said: a URL that is none, a
	// name XML cannot carry, no client. Nothing was sent.
	ErrInvalid = errors.New("not valid in UPnP")
	// ErrAnswer: the device answered what UPnP does not allow: no XML, no
	// SOAP body, a description without a UDN, a URL that leaves the
	// device, more than a document may hold.
	ErrAnswer = errors.New("the device's answer is not UPnP")
	// ErrRefused: the device said no. errors.As gives a *StatusError, or
	// a *FaultError when it said why.
	ErrRefused = errors.New("the device refused")
	// ErrLost: a subscription can no longer be trusted to have told
	// everything: the device forgot it, an event did not arrive or could
	// not be read, it could not be renewed in time, or the handler fell
	// too far behind. Watching again starts from how things stand.
	ErrLost = errors.New("the subscription is lost")
)

// StatusError is an HTTP status that is no success, from a device that
// said no more than that.
type StatusError struct {
	// Status is the status: 404, 412, 503.
	Status int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("the device refused: HTTP %d %s", e.Status, http.StatusText(e.Status))
}

// Is makes a StatusError an ErrRefused.
func (*StatusError) Is(target error) bool { return target == ErrRefused }

// FaultError is a device's refusal of an action, as it said it.
type FaultError struct {
	// Code is the UPnP error: 401 for an action the service does not
	// have, 402 for arguments it does not take, 501 for one that failed,
	// 600 and above as the service defines them. Zero when the device
	// gave none.
	Code int
	// Description is the device's word for it, when it gave one.
	Description string
	// Status is the HTTP status the fault came under: 500, and from some
	// devices 200.
	Status int
}

func (f *FaultError) Error() string {
	if f.Description == "" {
		return fmt.Sprintf("the device refused: UPnP error %d", f.Code)
	}

	return fmt.Sprintf("the device refused: UPnP error %d, %s", f.Code, f.Description)
}

// Is makes a FaultError an ErrRefused.
func (*FaultError) Is(target error) bool { return target == ErrRefused }
