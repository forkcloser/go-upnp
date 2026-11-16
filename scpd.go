package upnp

import (
	"context"
	"encoding/xml"
	"fmt"
	"strings"
)

// SCPD is a service's own description: the actions it takes and the
// state variables behind them.
type SCPD struct {
	// Actions is its actions, in the order it lists them.
	Actions []Action
	// Variables is its state variables.
	Variables []Variable
}

// Action is one action of a service.
type Action struct {
	// Name is its name.
	Name string
	// In is the arguments it takes, in the order they have to be sent.
	In []Parameter
	// Out is the values it answers, in the order the service lists them.
	// A device may answer others: Call returns what came.
	Out []Parameter
}

// Parameter is one argument or one answered value of an action.
type Parameter struct {
	// Name is its name.
	Name string
	// Variable is the state variable that says what it may hold.
	Variable string
}

// Variable is one state variable of a service.
type Variable struct {
	// Name is its name.
	Name string
	// Type is its data type, as the service gives it: "string", "ui2",
	// "boolean".
	Type string
	// Default is the value it starts with, when the service gives one.
	Default string
	// Allowed is the values it may hold, when the service lists them.
	Allowed []string
	// Minimum, Maximum and Step bound a number, when the service gives
	// them, as it wrote them.
	Minimum string
	Maximum string
	Step    string
	// Evented says a change of it is told to subscribers.
	Evented bool
}

// Action is the action of that name.
func (s *SCPD) Action(name string) (Action, bool) {
	for _, action := range s.Actions {
		if action.Name == name {
			return action, true
		}
	}

	return Action{}, false
}

// A service's description as it is read, its names in lower case.
type (
	xmlSCPD struct {
		XMLName   xml.Name      `xml:"scpd"`
		Actions   []xmlAction   `xml:"actionlist>action"`
		Variables []xmlVariable `xml:"servicestatetable>statevariable"`
	}
	xmlAction struct {
		Name      string        `xml:"name"`
		Arguments []xmlArgument `xml:"argumentlist>argument"`
	}
	xmlArgument struct {
		Name      string `xml:"name"`
		Direction string `xml:"direction"`
		Variable  string `xml:"relatedstatevariable"`
	}
	xmlVariable struct {
		Events  string   `xml:"sendevents,attr"`
		Name    string   `xml:"name"`
		Type    string   `xml:"datatype"`
		Default string   `xml:"defaultvalue"`
		Allowed []string `xml:"allowedvaluelist>allowedvalue"`
		Minimum string   `xml:"allowedvaluerange>minimum"`
		Maximum string   `xml:"allowedvaluerange>maximum"`
		Step    string   `xml:"allowedvaluerange>step"`
	}
)

// Describe reads the service's own description, at its SCPD URL.
func (s Service) Describe(ctx context.Context, client Doer) (*SCPD, error) {
	where, err := absolute(s.SCPD)
	if err != nil {
		return nil, fmt.Errorf("the description of %s: %w", s.Type, err)
	}

	document, err := get(ctx, client, where)
	if err != nil {
		return nil, err
	}

	var read xmlSCPD

	if err = parse(document, &read); err != nil {
		return nil, fmt.Errorf("the description of %s at %s: %w", s.Type, s.SCPD, err)
	}

	text := strings.TrimSpace
	described := &SCPD{}

	for _, one := range read.Actions {
		action := Action{Name: text(one.Name)}

		for _, argument := range one.Arguments {
			parameter := Parameter{Name: text(argument.Name), Variable: text(argument.Variable)}

			// What is not said to come out goes in: a device that left the
			// direction out still has to be sent its arguments.
			if strings.EqualFold(text(argument.Direction), "out") {
				action.Out = append(action.Out, parameter)
			} else {
				action.In = append(action.In, parameter)
			}
		}

		described.Actions = append(described.Actions, action)
	}

	for _, one := range read.Variables {
		variable := Variable{
			Name: text(one.Name), Type: text(one.Type), Default: text(one.Default),
			Minimum: text(one.Minimum), Maximum: text(one.Maximum), Step: text(one.Step),
			// Evented unless it says no: that is the standard's default.
			Evented: !strings.EqualFold(text(one.Events), "no"),
		}

		for _, allowed := range one.Allowed {
			variable.Allowed = append(variable.Allowed, text(allowed))
		}

		described.Variables = append(described.Variables, variable)
	}

	return described, nil
}
