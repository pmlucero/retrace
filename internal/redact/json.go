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

// decodeJSON parses data as exactly one JSON value.
func decodeJSON(data []byte) (jsonValue, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after the JSON value")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (jsonValue, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := &jsonObject{}
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, _ := keyTok.(string)
				v, err := decodeValue(dec)
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
				v, err := decodeValue(dec)
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

// masked returns the redaction marker for v, hashed over its JSON text.
func masked(v jsonValue) jsonValue {
	return mask(string(encodeJSON(v)))
}
