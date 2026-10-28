package mcp

import (
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
	// Decoding the whole body first applies the depth limit to it as a
	// whole, and lets canonicalMessage decode its parts without failing.
	whole, err := jsonvalue.Decode(body)
	if err != nil {
		return rawForm(server, body)
	}
	form := &jsonvalue.Object{Members: []jsonvalue.Member{{Key: "server", Value: server}}}
	parsed, err := ParseBody(body)
	if err != nil {
		form.Members = append(form.Members, jsonvalue.Member{Key: "body", Value: whole})
		return jsonvalue.EncodeCanonical(form)
	}
	msgs := make(jsonvalue.Array, len(parsed.Messages))
	for i, m := range parsed.Messages {
		v, err := canonicalMessage(m)
		if err != nil {
			return rawForm(server, body) // impossible: body decoded above
		}
		msgs[i] = v
	}
	if parsed.Batch {
		form.Members = append(form.Members, jsonvalue.Member{Key: "batch", Value: msgs})
	} else {
		form.Members = append(form.Members, jsonvalue.Member{Key: "message", Value: msgs[0]})
	}
	return jsonvalue.EncodeCanonical(form)
}

// rawForm is the canonical form of a body that is not valid JSON or is too
// deep to decode.
func rawForm(server string, body []byte) []byte {
	form := append(jsonvalue.EncodeCanonical(server), '\n')
	return append(form, body...)
}

// canonicalMessage returns the part of m that identifies it for matching.
func canonicalMessage(m Message) (jsonvalue.Value, error) {
	var kind, key string
	var raw []byte
	switch m.Kind {
	case Request:
		kind, key, raw = "request", "params", m.Params
	case Notification:
		kind, key, raw = "notification", "params", m.Params
	case Response:
		kind, key, raw = "response", "result", m.Result
		if m.Error != nil {
			key, raw = "error", m.Error
		}
	default:
		kind, key, raw = "invalid", "element", m.Raw
	}
	isCall := m.Kind == Request || m.Kind == Notification
	obj := &jsonvalue.Object{Members: []jsonvalue.Member{{Key: "kind", Value: kind}}}
	if isCall {
		obj.Members = append(obj.Members, jsonvalue.Member{Key: "method", Value: m.Method})
	}
	if raw == nil {
		return obj, nil // a request or notification without params
	}
	v, err := jsonvalue.Decode(raw)
	if err != nil {
		return nil, err
	}
	if p, ok := v.(*jsonvalue.Object); ok && isCall {
		// Spec §5 step 1: params._meta carries per-call data, such as a
		// progress token, that is not part of what is asked.
		p.Members = slices.DeleteFunc(p.Members, func(mb jsonvalue.Member) bool { return mb.Key == "_meta" })
	}
	obj.Members = append(obj.Members, jsonvalue.Member{Key: key, Value: v})
	return obj, nil
}
