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
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
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
	return encode(v, false)
}

// EncodeCanonical writes v like Encode, but with the members of every object
// sorted by key, comparing the keys' UTF-8 bytes. Duplicate keys keep their
// relative order. Values that differ only in member order, whitespace or
// string escapes give the same bytes, while numbers keep their text
// (ADR 0007 §1). Decoding replaces invalid UTF-8 and lone surrogate escapes
// with U+FFFD, so strings that differ only there are equal too.
func EncodeCanonical(v Value) []byte {
	return encode(v, true)
}

// scratch reuses encoding buffers, so a large body grows one buffer once
// instead of once per call.
var scratch = sync.Pool{New: func() any { b := make([]byte, 0, 4096); return &b }}

// maxPooled keeps one huge body from pinning its buffer in the pool.
const maxPooled = 1 << 20

func encode(v Value, sorted bool) []byte {
	p := scratch.Get().(*[]byte)
	b := appendValue((*p)[:0], v, sorted)
	out := bytes.Clone(b)
	if cap(b) <= maxPooled {
		*p = b
		scratch.Put(p)
	}
	return out
}

func appendValue(b []byte, v Value, sorted bool) []byte {
	switch t := v.(type) {
	case *Object:
		members := t.Members
		if sorted {
			cmp := func(x, y Member) int { return strings.Compare(x.Key, y.Key) }
			if !slices.IsSortedFunc(members, cmp) {
				members = slices.Clone(members)
				slices.SortStableFunc(members, cmp)
			}
		}
		b = append(b, '{')
		for i, m := range members {
			if i > 0 {
				b = append(b, ',')
			}
			b = appendQuoted(b, m.Key)
			b = append(b, ':')
			b = appendValue(b, m.Value, sorted)
		}
		b = append(b, '}')
	case Array:
		b = append(b, '[')
		for i, e := range t {
			if i > 0 {
				b = append(b, ',')
			}
			b = appendValue(b, e, sorted)
		}
		b = append(b, ']')
	case string:
		b = appendQuoted(b, t)
	case json.Number:
		b = append(b, t.String()...)
	case bool:
		b = strconv.AppendBool(b, t)
	case nil:
		b = append(b, "null"...)
	}
	return b
}

const hexDigits = "0123456789abcdef"

// appendQuoted appends s as a JSON string exactly as encoding/json writes it
// with HTML escaping off: short escapes for the quote, the backslash and
// \b \f \n \r \t, \u00xx for other control characters, U+2028 and U+2029
// escaped, and each invalid UTF-8 byte written as �. A test compares it
// with encoding/json.
func appendQuoted(b []byte, s string) []byte {
	b = append(b, '"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			if c >= ' ' && c != '"' && c != '\\' {
				i++
				continue
			}
			b = append(b, s[start:i]...)
			switch c {
			case '"', '\\':
				b = append(b, '\\', c)
			case '\b':
				b = append(b, '\\', 'b')
			case '\f':
				b = append(b, '\\', 'f')
			case '\n':
				b = append(b, '\\', 'n')
			case '\r':
				b = append(b, '\\', 'r')
			case '\t':
				b = append(b, '\\', 't')
			default:
				b = append(b, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b = append(b, s[start:i]...)
			b = append(b, `�`...)
			i += size
			start = i
			continue
		}
		if r == ' ' || r == ' ' {
			b = append(b, s[start:i]...)
			b = append(b, `\u202`...)
			b = append(b, hexDigits[r&0xf])
			i += size
			start = i
			continue
		}
		i += size
	}
	b = append(b, s[start:]...)
	return append(b, '"')
}
