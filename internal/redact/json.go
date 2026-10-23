package redact

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// jsonValue is a decoded JSON value that keeps what encoding/json's generic
// decoding loses: member order and number text. It is one of *jsonObject,
// jsonArray, string, json.Number, bool or nil.
type jsonValue any

type jsonObject struct {
	members []jsonMember
}

type jsonMember struct {
	key   string
	value jsonValue
}

type jsonArray []jsonValue

// maxDepth caps nesting while decoding. It is below encoding/json's own
// limit (10000), whose error cannot be told apart from malformed JSON: a body
// that is merely too deep must be refused, never stored as "not JSON"
// (spec §3.5). Real API bodies nest a few dozen levels at most.
const maxDepth = 1000

// errTooDeep reports a body nested deeper than maxDepth.
var errTooDeep = fmt.Errorf("JSON nested deeper than %d levels", maxDepth)

// decodeJSON parses data as exactly one JSON value.
func decodeJSON(data []byte) (jsonValue, error) {
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

func decodeValue(dec *json.Decoder, depth int) (jsonValue, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		if depth >= maxDepth {
			return nil, errTooDeep
		}
		switch t {
		case '{':
			obj := &jsonObject{}
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
				obj.members = append(obj.members, jsonMember{key: key, value: v})
			}
			_, err := dec.Token() // '}'
			return obj, err
		case '[':
			arr := jsonArray{}
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

// encodeJSON writes v compactly, in member order, without HTML escaping.
func encodeJSON(v jsonValue) []byte {
	var b bytes.Buffer
	writeValue(&b, v)
	return b.Bytes()
}

func writeValue(b *bytes.Buffer, v jsonValue) {
	switch t := v.(type) {
	case *jsonObject:
		b.WriteByte('{')
		for i, m := range t.members {
			if i > 0 {
				b.WriteByte(',')
			}
			writeString(b, m.key)
			b.WriteByte(':')
			writeValue(b, m.value)
		}
		b.WriteByte('}')
	case jsonArray:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			writeValue(b, e)
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

// masked returns the redaction marker for v. A string is hashed as it is, like
// a header value, so a secret gets the same marker wherever it appears; other
// values are hashed over their JSON text (spec §3.5).
func masked(v jsonValue) jsonValue {
	if s, ok := v.(string); ok {
		return mask(s)
	}
	return mask(string(encodeJSON(v)))
}
