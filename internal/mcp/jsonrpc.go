// Package mcp reads MCP traffic over Streamable HTTP (ADR 0004): the JSON-RPC
// 2.0 messages in a POST body, in a JSON response or in the events of an SSE
// response, and the pairing of each request with its response (spec §3.4).
//
// The package is pure: it parses bytes that the proxy or the cassette hand
// it. Parsing is lenient where a stored body may have been redacted
// (spec §3.5): the "jsonrpc" member is not checked, and method-specific
// helpers treat a value of an unexpected type as absent.
package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Kind tells what a JSON-RPC message is.
type Kind int

// The kinds of JSON-RPC messages.
const (
	// Invalid is a batch element that is not a JSON-RPC message. A single
	// message that is not valid is an error instead.
	Invalid Kind = iota
	// Request has a method and an id, and expects a response.
	Request
	// Notification has a method and no id.
	Notification
	// Response has an id and either a result or an error.
	Response
)

// Message is one JSON-RPC message. The JSON members are kept as they appear
// in the body, so replay can rewrite the id (spec §5 step 3) and derivation
// sees values exactly as stored.
type Message struct {
	Kind Kind
	// Raw is the message as it appears in the body.
	Raw json.RawMessage
	// ID is the id's JSON text, e.g. 7 or "abc". It is nil for a
	// notification, and may be null for an error response.
	ID     json.RawMessage
	Method string          // requests and notifications
	Params json.RawMessage // nil when absent
	Result json.RawMessage // responses; nil when absent
	Error  json.RawMessage // responses; nil when absent

	idStart, idEnd int // the id's span in Raw, when there is an id
}

// WithID returns Raw with the id replaced by id, keeping every other byte as
// recorded, so replay can answer with the incoming request's id
// (spec §5 step 3). A message without an id is returned unchanged.
func (m Message) WithID(id json.RawMessage) json.RawMessage {
	if m.ID == nil {
		return m.Raw
	}
	out := make([]byte, 0, len(m.Raw)-len(m.ID)+len(id))
	out = append(out, m.Raw[:m.idStart]...)
	out = append(out, id...)
	return append(out, m.Raw[m.idEnd:]...)
}

// Body is the content of a POST body or a JSON response.
type Body struct {
	Messages []Message
	// Batch reports whether the messages came as a JSON array. A batch of
	// one is still a batch, and its response is an array too.
	Batch bool
}

// ParseBody parses a body holding a single JSON-RPC message or a batch.
//
// A body that is not JSON, a single message that is not valid, and an empty
// batch are errors: callers then treat the body as opaque. Inside a batch,
// an element that is not a valid message is kept with Kind Invalid, so the
// other elements are still usable and positions are preserved.
func ParseBody(body []byte) (Body, error) {
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var elems []json.RawMessage
		if err := json.Unmarshal(body, &elems); err != nil {
			return Body{}, fmt.Errorf("parsing JSON-RPC batch: %w", err)
		}
		if len(elems) == 0 {
			return Body{}, errors.New("parsing JSON-RPC batch: empty batch")
		}
		msgs := make([]Message, len(elems))
		for i, e := range elems {
			m, err := parseMessage(e)
			if err != nil {
				m = Message{Kind: Invalid, Raw: e}
			}
			msgs[i] = m
		}
		return Body{Messages: msgs, Batch: true}, nil
	}
	m, err := parseMessage(body)
	if err != nil {
		return Body{}, err
	}
	return Body{Messages: []Message{m}}, nil
}

// parseMessage parses one JSON-RPC message. Unknown members are ignored.
func parseMessage(data []byte) (Message, error) {
	var raw struct {
		ID     json.RawMessage `json:"id"`
		Method json.RawMessage `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return Message{}, fmt.Errorf("parsing JSON-RPC message: %w", err)
	}
	data = bytes.TrimSpace(data)
	if data[0] != '{' { // null decodes into a struct without error
		return Message{}, errors.New("parsing JSON-RPC message: not an object")
	}
	m := Message{
		Raw:    json.RawMessage(data),
		ID:     raw.ID,
		Params: raw.Params,
		Result: raw.Result,
		Error:  raw.Error,
	}
	if raw.ID != nil {
		m.idStart, m.idEnd = idSpan(data)
	}
	switch {
	case raw.Method != nil:
		if err := json.Unmarshal(raw.Method, &m.Method); err != nil {
			return Message{}, errors.New("parsing JSON-RPC message: method is not a string")
		}
		if raw.Result != nil || raw.Error != nil {
			return Message{}, errors.New("parsing JSON-RPC message: method together with result or error")
		}
		m.Kind = Notification
		if raw.ID != nil {
			// MCP request ids are strings or integers, never null.
			if !isStringOrNumber(raw.ID) {
				return Message{}, errors.New("parsing JSON-RPC message: request id is not a string or number")
			}
			m.Kind = Request
		}
	case raw.ID != nil && (raw.Result != nil) != (raw.Error != nil):
		// An error response to a request whose id could not be read has a
		// null id (JSON-RPC 2.0 §5).
		if !isStringOrNumber(raw.ID) && string(raw.ID) != "null" {
			return Message{}, errors.New("parsing JSON-RPC message: response id is not a string, number or null")
		}
		m.Kind = Response
	default:
		return Message{}, errors.New("parsing JSON-RPC message: neither a request, a notification nor a response")
	}
	return m, nil
}

// idSpan returns where the value of the top-level "id" member is in obj, a
// valid JSON object. With duplicate members it is the last one, the one
// encoding/json decodes.
func idSpan(obj []byte) (start, end int) {
	dec := json.NewDecoder(bytes.NewReader(obj))
	_, _ = dec.Token() // '{'
	for dec.More() {
		key, _ := dec.Token()
		var v json.RawMessage
		if dec.Decode(&v) != nil {
			break
		}
		if key == "id" {
			end = int(dec.InputOffset())
			start = end - len(v)
		}
	}
	return start, end
}

func isStringOrNumber(v json.RawMessage) bool {
	if len(v) == 0 {
		return false
	}
	c := v[0]
	return c == '"' || c == '-' || (c >= '0' && c <= '9')
}
