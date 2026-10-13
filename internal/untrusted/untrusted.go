// Package untrusted marks text that comes from the chain, the explorer or
// the status page, none of which this server controls.
//
// Anyone can put arbitrary text into a token name, a contract's source, an
// event string, a revert reason, transaction calldata or a node name, and
// that text can be written to look like instructions to the model reading
// tool output. Every such string is therefore sanitized and wrapped as
// {"untrusted": "..."} in tool results, and the server instructions tell the
// model to treat anything inside that wrapper as data only.
package untrusted

import (
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"
)

// DefaultMax is the length limit of a wrapped string, in bytes, unless a
// caller asks for another.
const DefaultMax = 256

// Text is an untrusted string. It marshals as {"untrusted": "<sanitized>"},
// plus "truncated": true and the original length when it was cut.
type Text struct {
	value     string
	truncated bool
	origLen   int
}

// New sanitizes s and caps it at DefaultMax bytes.
func New(s string) Text { return NewN(s, DefaultMax) }

// NewN sanitizes s and caps it at max bytes.
func NewN(s string, max int) Text {
	clean := Sanitize(s)
	t := Text{value: clean, origLen: len(s)}
	if len(clean) > max {
		cut := max
		for cut > 0 && !utf8.RuneStart(clean[cut]) {
			cut--
		}
		t.value = clean[:cut]
		t.truncated = true
	}
	return t
}

// Ptr returns nil for an empty string, so optional fields are omitted.
func Ptr(s string) *Text {
	if s == "" {
		return nil
	}
	t := New(s)
	return &t
}

// String returns the sanitized text.
func (t Text) String() string { return t.value }

// MarshalJSON implements json.Marshaler.
func (t Text) MarshalJSON() ([]byte, error) {
	if t.truncated {
		return json.Marshal(struct {
			Untrusted     string `json:"untrusted"`
			Truncated     bool   `json:"truncated"`
			OriginalBytes int    `json:"original_bytes"`
		}{t.value, true, t.origLen})
	}
	return json.Marshal(struct {
		Untrusted string `json:"untrusted"`
	}{t.value})
}

// Sanitize makes s valid UTF-8 and removes characters that hide or reorder
// text: control characters other than newline and tab, bidirectional
// overrides and isolates, zero-width characters and other format characters.
func Sanitize(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r == '\r':
			return -1
		case unicode.IsControl(r):
			return -1
		case unicode.Is(unicode.Cf, r): // format: bidi controls, zero-width, BOM, tags
			return -1
		}
		return r
	}, s)
}

// Printable reports whether b decodes as mostly printable UTF-8 text, so
// that bytes such as extraData are shown as text only when they are text.
func Printable(b []byte) bool {
	if len(b) == 0 || !utf8.Valid(b) {
		return false
	}
	printable, total := 0, 0
	for _, r := range string(b) {
		total++
		if unicode.IsPrint(r) || r == ' ' {
			printable++
		}
	}
	return printable*10 >= total*9
}
