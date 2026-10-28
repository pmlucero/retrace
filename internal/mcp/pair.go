package mcp

import (
	"github.com/pmlucero/retrace/internal/jsonvalue"
)

// Pair is a request and the response that answers it.
type Pair struct {
	Request Message
	// Response is nil when the request was not answered: the response held
	// no message with its id, e.g. because the stream was cut.
	Response *Message
}

// Pairing is the result of matching the requests of a POST with the
// messages of its response (spec §3.4).
type Pairing struct {
	// Pairs has one entry per request, in request order.
	Pairs []Pair
	// Unmatched holds responses whose id answers no request of the POST,
	// or a request that was already answered. They are reported rather than
	// dropped, so callers can log them; none of them is paired.
	Unmatched []Message
	// ServerMessages holds the requests and notifications the server sent
	// in an SSE response, such as progress notifications or sampling
	// requests. They are not answers, so they are never paired.
	ServerMessages []Message
}

// PairMessages matches each request in requests to its response in
// responses by id. Ids are compared by value: "a" and "a" are the same
// id, while 1 and 1.0 are not, since servers echo ids unchanged. When a POST
// repeats an id, responses with that id answer its requests in order.
//
// Notifications and client responses in requests expect no answer and are
// ignored, as are Invalid messages on either side.
func PairMessages(requests, responses []Message) Pairing {
	var p Pairing
	waiting := map[string][]int{} // id → indexes in p.Pairs still unanswered
	for _, m := range requests {
		if m.Kind != Request {
			continue
		}
		key := idKey(m.ID)
		waiting[key] = append(waiting[key], len(p.Pairs))
		p.Pairs = append(p.Pairs, Pair{Request: m})
	}
	for _, m := range responses {
		switch m.Kind {
		case Request, Notification:
			p.ServerMessages = append(p.ServerMessages, m)
		case Response:
			key := idKey(m.ID)
			idx := waiting[key]
			if len(idx) == 0 {
				p.Unmatched = append(p.Unmatched, m)
				continue
			}
			waiting[key] = idx[1:]
			p.Pairs[idx[0]].Response = &m
		}
	}
	return p
}

// idKey returns a key that equals for ids with the same value: the canonical
// JSON, which normalizes string escapes and keeps number text (ADR 0007).
func idKey(id []byte) string {
	v, err := jsonvalue.Decode(id)
	if err != nil {
		return string(id)
	}
	return string(jsonvalue.EncodeCanonical(v))
}
