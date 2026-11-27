package mcp

import (
	"bytes"
	"encoding/json"
	"slices"

	"github.com/pmlucero/retrace/internal/jsonvalue"
)

// Canonicalize returns the canonical form of an MCP POST body sent to server,
// which is what its matching fingerprint hashes (ADR 0007 §1, spec §5 step 1).
// Body redaction rules must be applied to body first (spec §3.5).
//
// The canonical form of a JSON-RPC body is a JSON object holding the server
// name and either the message, for a single message, or the list of its
// messages in order, for a batch:
//
//	{"message":{...},"server":"fs"}
//	{"batch":[{...},{...}],"server":"fs"}
//
// so a batch of one and the bare message differ. Each message is reduced to
// what identifies it, without its JSON-RPC id and "jsonrpc" member:
//
//   - a request: its kind, method and params without params._meta;
//   - a notification: the same with its own kind, so it never matches a
//     request, which expects a response;
//   - a response from the client: its kind and its result or error;
//   - a batch element that is not a JSON-RPC message: its kind and the whole
//     element, which is valid JSON because the body is.
//
// Every other member is kept, _meta nested anywhere else included. The form
// is encoded with jsonvalue.EncodeCanonical: keys sorted, no insignificant
// whitespace, numbers in their original text and strings re-encoded the
// same way whatever escapes they were written with.
//
// A body that is valid JSON but neither a JSON-RPC message nor a non-empty
// batch has no method or params to keep, so its form holds the whole body,
// canonicalized: {"body":...,"server":"fs"}.
//
// A body that is not valid JSON, or that is nested deeper than
// jsonvalue.MaxDepth, is fingerprinted over its raw bytes together with the
// server name (spec §5): its form is the server name as a JSON string, a
// newline and the body as it is. That form starts with a quote, so it cannot
// collide with the form of a JSON body, which starts with a brace, and the
// JSON string ends unambiguously, so two servers never share a form.
func Canonicalize(server string, body []byte) []byte {
	// One decode serves the depth limit, the classification and the form:
	// the messages' parts are taken from this tree, not decoded again.
	whole, err := jsonvalue.Decode(body)
	if err != nil {
		return rawForm(server, body)
	}
	form := &jsonvalue.Object{Members: []jsonvalue.Member{{Key: "server", Value: server}}}
	if arr, ok := whole.(jsonvalue.Array); ok && len(arr) > 0 {
		msgs := make(jsonvalue.Array, len(arr))
		for i, e := range arr {
			msgs[i] = canonicalElement(e)
		}
		form.Members = append(form.Members, jsonvalue.Member{Key: "batch", Value: msgs})
		return jsonvalue.EncodeCanonical(form)
	}
	// A single message, or a body that is none: an empty batch, or a value
	// that is not a valid message.
	if cm, ok := classify(whole); ok {
		form.Members = append(form.Members, jsonvalue.Member{Key: "message", Value: cm})
	} else {
		form.Members = append(form.Members, jsonvalue.Member{Key: "body", Value: whole})
	}
	return jsonvalue.EncodeCanonical(form)
}

// rawForm is the canonical form of a body that is not valid JSON or is too
// deep to decode.
func rawForm(server string, body []byte) []byte {
	form := append(jsonvalue.EncodeCanonical(server), '\n')
	return append(form, body...)
}

// canonicalElement is the canonical form of a batch element: that of the
// message it is, or of an invalid element.
func canonicalElement(e jsonvalue.Value) jsonvalue.Value {
	if cm, ok := classify(e); ok {
		return cm
	}
	return &jsonvalue.Object{Members: []jsonvalue.Member{
		{Key: "kind", Value: "invalid"},
		{Key: "element", Value: e},
	}}
}

// classify applies the rules of parseMessage to a decoded value and returns
// the part of the message that identifies it for matching. It reports false
// when v is not a valid JSON-RPC message. With duplicate members the last one
// counts, as in parseMessage; the values are taken from the tree, which keeps
// the duplicates inside them. params._meta is removed from the tree in place.
func classify(v jsonvalue.Value) (jsonvalue.Value, bool) {
	o, ok := v.(*jsonvalue.Object)
	if !ok {
		return nil, false
	}
	var id, method, params, result, errv jsonvalue.Value
	var hasID, hasMethod, hasParams, hasResult, hasErr bool
	for _, m := range o.Members {
		switch m.Key {
		case "id":
			id, hasID = m.Value, true
		case "method":
			method, hasMethod = m.Value, true
		case "params":
			params, hasParams = m.Value, true
		case "result":
			result, hasResult = m.Value, true
		case "error":
			errv, hasErr = m.Value, true
		}
	}
	_, idString := id.(string)
	_, idNumber := id.(json.Number)
	idOK := idString || idNumber
	switch {
	case hasMethod:
		// A null method reads as the empty string, as json.Unmarshal does
		// into a string.
		var name string
		switch mv := method.(type) {
		case string:
			name = mv
		case nil:
		default:
			return nil, false
		}
		if hasResult || hasErr {
			return nil, false
		}
		kind := "notification"
		if hasID {
			// MCP request ids are strings or integers, never null.
			if !idOK {
				return nil, false
			}
			kind = "request"
		}
		obj := &jsonvalue.Object{Members: []jsonvalue.Member{
			{Key: "kind", Value: kind},
			{Key: "method", Value: name},
		}}
		if hasParams {
			if p, ok := params.(*jsonvalue.Object); ok {
				// Spec §5 step 1: params._meta carries per-call data, such
				// as a progress token, that is not part of what is asked.
				p.Members = slices.DeleteFunc(p.Members, func(mb jsonvalue.Member) bool { return mb.Key == "_meta" })
			}
			obj.Members = append(obj.Members, jsonvalue.Member{Key: "params", Value: params})
		}
		return obj, true
	case hasID && hasResult != hasErr:
		// An error response to a request whose id could not be read has a
		// null id (JSON-RPC 2.0 §5).
		if !idOK && id != nil {
			return nil, false
		}
		key, val := "result", result
		if hasErr {
			key, val = "error", errv
		}
		return &jsonvalue.Object{Members: []jsonvalue.Member{
			{Key: "kind", Value: "response"},
			{Key: key, Value: val},
		}}, true
	}
	return nil, false
}

// DiffForm returns the part of a canonical form that a miss diff compares
// (spec §5 step 4): the canonical message, the array of a batch's canonical
// messages, or the canonical body, without the server name both sides share.
// For a body fingerprinted over its raw bytes, it is the body as it is.
func DiffForm(canonical []byte) []byte {
	v, err := jsonvalue.Decode(canonical)
	if err != nil {
		// The raw form: the server name as a JSON string, which holds no
		// raw newline, then a newline and the body.
		if i := bytes.IndexByte(canonical, '\n'); i >= 0 {
			return canonical[i+1:]
		}
		return canonical
	}
	if obj, ok := v.(*jsonvalue.Object); ok {
		for _, m := range obj.Members {
			if m.Key == "message" || m.Key == "batch" || m.Key == "body" {
				return jsonvalue.EncodeCanonical(m.Value)
			}
		}
	}
	return canonical
}
