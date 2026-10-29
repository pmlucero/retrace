package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// IDRewriter gives the responses of a replayed POST the ids of the incoming
// requests they answer (spec §5 step 3). Replay matches the incoming POST to a
// recorded one by fingerprint, which leaves the ids out, so the recorded
// responses carry the ids of the recorded requests.
//
// An IDRewriter is used by one goroutine, for one response.
type IDRewriter struct {
	ids map[string][]json.RawMessage // recorded request id → incoming ids, in order
}

// NewIDRewriter pairs the requests of a recorded POST with those of the
// incoming POST that it answers, by position: their bodies have the same
// fingerprint, so they hold the same kinds of messages in the same order.
// Messages that are not requests take no part.
func NewIDRewriter(recorded, incoming []Message) *IDRewriter {
	r := &IDRewriter{ids: map[string][]json.RawMessage{}}
	i := 0
	for _, rec := range recorded {
		if rec.Kind != Request {
			continue
		}
		for i < len(incoming) && incoming[i].Kind != Request {
			i++
		}
		if i == len(incoming) {
			break
		}
		key := idKey(rec.ID)
		r.ids[key] = append(r.ids[key], incoming[i].ID)
		i++
	}
	return r
}

// Body returns a JSON response body with the id of each response replaced by
// the id of the incoming request it answers. Every other byte is kept as
// recorded. Responses are paired with requests as PairMessages does: when the
// recorded POST repeats an id, its responses take the incoming ids in order.
// A response that answers no request, such as an error with a null id, and a
// body that is not JSON-RPC are kept as they are.
func (r *IDRewriter) Body(body []byte) []byte {
	var out []byte
	last := 0
	for _, s := range messageSpans(body) {
		m, err := parseMessage(body[s.start:s.end])
		if err != nil || m.Kind != Response {
			continue
		}
		id, ok := r.next(m.ID)
		if !ok {
			continue
		}
		out = append(out, body[last:s.start]...)
		out = append(out, m.WithID(id)...)
		last = s.end
	}
	if out == nil {
		return body
	}
	return append(out, body[last:]...)
}

// Event returns one SSE event, as stored in a chunk with its framing, with
// the ids in its data rewritten as Body does. An id holds no line break, so
// data split over several lines keeps its lines, and each line keeps its
// field prefix and its ending.
func (r *IDRewriter) Event(event []byte) []byte {
	lines := splitLines(event)
	data, ok := eventData(lines)
	if !ok {
		return event
	}
	rewritten := r.Body(data)
	if bytes.Equal(rewritten, data) {
		return event
	}
	values := bytes.Split(rewritten, []byte("\n"))
	out := make([]byte, 0, len(event)+len(rewritten)-len(data))
	for _, l := range lines {
		n, ok := dataPrefix(l.text)
		if !ok {
			out = append(append(out, l.text...), l.eol...)
			continue
		}
		// Rewriting adds no line break, since a JSON id holds none, so there
		// is one value per data line.
		out = append(out, l.text[:n]...)
		out = append(out, values[0]...)
		out = append(out, l.eol...)
		values = values[1:]
	}
	return out
}

// next returns the incoming id for a recorded response id, once per request.
func (r *IDRewriter) next(recorded json.RawMessage) (json.RawMessage, bool) {
	key := idKey(recorded)
	ids := r.ids[key]
	if len(ids) == 0 {
		return nil, false
	}
	r.ids[key] = ids[1:]
	return ids[0], true
}

// span is where a message is in a body.
type span struct{ start, end int }

// messageSpans returns where each top-level message of a JSON-RPC body is:
// the whole value for a single message, or each element of a batch. It
// returns nil for a batch that is not valid JSON, as ParseBody rejects it.
func messageSpans(body []byte) []span {
	start := len(body) - len(bytes.TrimLeft(body, " \t\r\n"))
	end := len(bytes.TrimRight(body, " \t\r\n"))
	if start >= end {
		return nil
	}
	if body[start] != '[' {
		return []span{{start, end}}
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if _, err := dec.Token(); err != nil {
		return nil
	}
	var spans []span
	for dec.More() {
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil
		}
		// The decoded value holds no surrounding whitespace or comma.
		e := int(dec.InputOffset())
		spans = append(spans, span{e - len(v), e})
	}
	if _, err := dec.Token(); err != nil { // ']', missing in a cut body
		return nil
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil
	}
	return spans
}
