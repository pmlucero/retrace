package anthropic

import (
	"slices"

	"github.com/pmlucero/retrace/internal/jsonvalue"
)

// Canonicalize returns the canonical form of a Messages API request body,
// which is what its matching fingerprint hashes (ADR 0007 §1, spec §5). Body
// redaction rules must be applied to body first (spec §3.5).
//
// The canonical form of a JSON body is the body without its top-level
// metadata member, with every object's keys sorted, no insignificant
// whitespace, numbers in their original text and strings re-encoded the same
// way whatever escapes they were written with. Every other field is kept,
// stream included: a streamed recording cannot answer a non-streaming
// request, or the other way around.
//
// A body that is not valid JSON is fingerprinted over its raw bytes (spec §5),
// so it is returned as it is: replaying the same bytes finds what was
// recorded for them. A body nested deeper than jsonvalue.MaxDepth cannot be
// decoded safely and is returned as it is too. Such a body is only stored
// when no body redaction rule is set (spec §3.5). Neither can collide with a
// decodable body, whose canonical form is valid JSON nested at most MaxDepth
// levels deep.
func Canonicalize(body []byte) []byte {
	v, err := jsonvalue.Decode(body)
	if err != nil {
		return body
	}
	if obj, ok := v.(*jsonvalue.Object); ok {
		obj.Members = slices.DeleteFunc(obj.Members, func(m jsonvalue.Member) bool { return m.Key == "metadata" })
	}
	return jsonvalue.EncodeCanonical(v)
}
