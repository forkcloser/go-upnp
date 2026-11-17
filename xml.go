package upnp

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// decoder reads a device's XML, which normal has made UTF-8 whatever it
// declares.
func decoder(document []byte) *xml.Decoder {
	reading := xml.NewDecoder(bytes.NewReader(document))
	reading.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }

	return reading
}

// normal is a document in UTF-8: itself, or what it says in an encoding
// of one byte a character, the only others a device is known to
// declare. It is done once and ahead of any reading, so that a place in
// the document is the same place to every reader of it.
func normal(document []byte) ([]byte, error) {
	var declared string

	sniffing := xml.NewDecoder(bytes.NewReader(document))
	sniffing.CharsetReader = func(label string, input io.Reader) (io.Reader, error) {
		declared = label

		return input, nil
	}

	// The declaration, when there is one, is ahead of the first element.
	for {
		token, err := sniffing.RawToken()
		if _, started := token.(xml.StartElement); started || err != nil {
			break
		}
	}

	switch strings.ToLower(declared) {
	case "":
		return document, nil
	case "iso-8859-1", "latin1", "us-ascii", "ascii":
		// Every byte is the character of that number.
		converted := make([]byte, 0, len(document))
		for _, one := range document {
			converted = utf8.AppendRune(converted, rune(one))
		}

		return converted, nil
	default:
		return nil, fmt.Errorf("%w: a document in %q", ErrAnswer, declared)
	}
}

// folded hands on every element and attribute with its name in lower
// case: devices do not agree on the case of the names the standard
// gives, and encoding/xml matches them exactly.
type folded struct{ from *xml.Decoder }

func (f folded) Token() (xml.Token, error) {
	token, err := f.from.Token()

	switch element := token.(type) {
	case xml.StartElement:
		element.Name.Local = strings.ToLower(element.Name.Local)

		for index := range element.Attr {
			element.Attr[index].Name.Local = strings.ToLower(element.Attr[index].Name.Local)
		}

		token = element
	case xml.EndElement:
		element.Name.Local = strings.ToLower(element.Name.Local)
		token = element
	}

	return token, err //nolint:wrapcheck // an xml.TokenReader hands its decoder's error on, io.EOF among them
}

// parse reads a document into a value whose fields are tagged with the
// standard's names in lower case.
func parse(document []byte, into any) error {
	if err := xml.NewTokenDecoder(folded{from: decoder(document)}).Decode(into); err != nil {
		return fmt.Errorf("%w: %w", ErrAnswer, err)
	}

	return nil
}
