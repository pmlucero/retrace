// Package jsonvalue decodes and encodes JSON while keeping what
// encoding/json's generic decoding loses: member order and number text.
// Redaction (spec §3.5) needs both to write a body back as it was, apart from
// the masked values; request canonicalization (ADR 0007) needs number text.
// Sharing one decoder means both parse a body the same way.
package jsonvalue

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// Value is a decoded JSON value. It is one of *Object, Array, string,
// json.Number, bool or nil.
type Value any

// Object is a JSON object with its members in their original order,
// duplicates included.
type Object struct {
	Members []Member
}

// Member is one name/value pair of an Object.
type Member struct {
	Key   string
	Value Value
}

// Array is a JSON array.
type Array []Value

// MaxDepth caps nesting while decoding. It is below encoding/json's own
// limit (10000), whose error cannot be told apart from malformed JSON: a body
// that is merely too deep must be refused, never stored as "not JSON"
// (spec §3.5). Real API bodies nest a few dozen levels at most.
const MaxDepth = 1000

// ErrTooDeep reports a body nested deeper than MaxDepth.
var ErrTooDeep = fmt.Errorf("JSON nested deeper than %d levels", MaxDepth)

// Decode parses data as exactly one JSON value.
func Decode(data []byte) (Value, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeValue(dec, 0)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after the JSON value")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder, depth int) (Value, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		if depth >= MaxDepth {
			return nil, ErrTooDeep
		}
		switch t {
		case '{':
			obj := &Object{}
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, _ := keyTok.(string)
				v, err := decodeValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				obj.Members = append(obj.Members, Member{Key: key, Value: v})
			}
			_, err := dec.Token() // '}'
			return obj, err
		case '[':
			arr := Array{}
			for dec.More() {
				v, err := decodeValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			_, err := dec.Token() // ']'
			return arr, err
		}
		return nil, fmt.Errorf("unexpected %v", t)
	default:
		return t, nil // string, json.Number, bool or nil
	}
}

// Encode writes v compactly, in member order, without HTML escaping.
func Encode(v Value) []byte {
	var b bytes.Buffer
	writeValue(&b, v, false)
	return b.Bytes()
}

// EncodeCanonical writes v like Encode, but with the members of every object
// sorted by key, comparing the keys' UTF-8 bytes. Duplicate keys keep their
// relative order. Values that differ only in member order, whitespace or
// string escapes give the same bytes, while numbers keep their text
// (ADR 0007 §1). Decoding replaces invalid UTF-8 and lone surrogate escapes
// with U+FFFD, so strings that differ only there are equal too.
func EncodeCanonical(v Value) []byte {
	var b bytes.Buffer
	writeValue(&b, v, true)
	return b.Bytes()
}

func writeValue(b *bytes.Buffer, v Value, sorted bool) {
	switch t := v.(type) {
	case *Object:
		members := t.Members
		if sorted {
			members = slices.Clone(members)
			slices.SortStableFunc(members, func(x, y Member) int { return strings.Compare(x.Key, y.Key) })
		}
		b.WriteByte('{')
		for i, m := range members {
			if i > 0 {
				b.WriteByte(',')
			}
			writeString(b, m.Key)
			b.WriteByte(':')
			writeValue(b, m.Value, sorted)
		}
		b.WriteByte('}')
	case Array:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			writeValue(b, e, sorted)
		}
		b.WriteByte(']')
	case string:
		writeString(b, t)
	case json.Number:
		b.WriteString(t.String())
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case nil:
		b.WriteString("null")
	}
}

func writeString(b *bytes.Buffer, s string) {
	enc := json.NewEncoder(b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)       // a string always encodes
	b.Truncate(b.Len() - 1) // Encode appends a newline
}
