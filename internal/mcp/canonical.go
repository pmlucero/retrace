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
// A body that is not valid JSON, that is nested deeper than
// jsonvalue.MaxDepth, or that is neither a JSON-RPC message nor a non-empty
// batch is fingerprinted over its raw bytes together with the server name
// (spec §5): its form is the server name as a JSON string, a newline and the
// body as it is. That form starts with a quote, so it cannot collide with
// the form of a JSON-RPC body, which starts with a brace, and the JSON string
// ends unambiguously, so two servers never share a form.
func Canonicalize(server string, body []byte) []byte {
	// Decoding the whole body first applies the depth limit to it as a
	// whole, and lets canonicalMessage decode its parts without failing.
	if _, err := jsonvalue.Decode(body); err != nil {
		return rawForm(server, body)
	}
	parsed, err := ParseBody(body)
	if err != nil {
		return rawForm(server, body)
	}
	msgs := make(jsonvalue.Array, len(parsed.Messages))
	for i, m := range parsed.Messages {
		v, err := canonicalMessage(m)
		if err != nil {
			return rawForm(server, body) // impossible: body decoded above
		}
		msgs[i] = v
	}
	form := &jsonvalue.Object{Members: []jsonvalue.Member{{Key: "server", Value: server}}}
	if parsed.Batch {
		form.Members = append(form.Members, jsonvalue.Member{Key: "batch", Value: msgs})
	} else {
		form.Members = append(form.Members, jsonvalue.Member{Key: "message", Value: msgs[0]})
	}
	return jsonvalue.EncodeCanonical(form)
}

// rawForm is the canonical form of a body that is not JSON-RPC.
func rawForm(server string, body []byte) []byte {
	form := append(jsonvalue.EncodeCanonical(server), '\n')
	return append(form, body...)
}

// canonicalMessage returns the part of m that identifies it for matching.
func canonicalMessage(m Message) (jsonvalue.Value, error) {
	obj := &jsonvalue.Object{}
	add := func(key string, raw []byte) error {
		v, err := jsonvalue.Decode(raw)
		if err != nil {
			return err
		}
		if key == "params" {
			if p, ok := v.(*jsonvalue.Object); ok {
				p.Members = slices.DeleteFunc(p.Members, func(mb jsonvalue.Member) bool { return mb.Key == "_meta" })
			}
		}
		obj.Members = append(obj.Members, jsonvalue.Member{Key: key, Value: v})
		return nil
	}
	var err error
	switch m.Kind {
	case Request, Notification:
		kind := "request"
		if m.Kind == Notification {
			kind = "notification"
		}
		obj.Members = append(obj.Members,
			jsonvalue.Member{Key: "kind", Value: kind},
			jsonvalue.Member{Key: "method", Value: m.Method})
		if m.Params != nil {
			err = add("params", m.Params)
		}
	case Response:
		obj.Members = append(obj.Members, jsonvalue.Member{Key: "kind", Value: "response"})
		if m.Error != nil {
			err = add("error", m.Error)
		} else {
			err = add("result", m.Result)
		}
	default:
		obj.Members = append(obj.Members, jsonvalue.Member{Key: "kind", Value: "invalid"})
		err = add("element", m.Raw)
	}
	if err != nil {
		return nil, err
	}
	return obj, nil
}
