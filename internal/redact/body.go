package redact

import (
	"bytes"
	"errors"
	"fmt"
	"slices"

	"github.com/pmlucero/retrace/internal/jsonvalue"
	"github.com/pmlucero/retrace/internal/sse"
)

// BodyRules returns the body redaction rules as written, for the cassette's
// redaction metadata (spec §7.2).
func (r *Rules) BodyRules() []string {
	out := make([]string, len(r.body))
	for i, p := range r.body {
		out[i] = p.String()
	}
	return out
}

// ErrNotRedactable is returned for a body that may hold values the rules
// select but cannot be redacted safely. It must not be stored.
var ErrNotRedactable = errors.New("body cannot be redacted")

// Body returns data with the values selected by the body rules masked
// (spec §3.5). A body that is not JSON, or has no match, is returned as it is.
// A JSON body nested too deeply returns ErrNotRedactable.
func (r *Rules) Body(data []byte) ([]byte, error) {
	if len(r.body) == 0 {
		return data, nil
	}
	if r.cannotMatch(data) {
		return data, nil
	}
	v, err := jsonvalue.Decode(data)
	if errors.Is(err, jsonvalue.ErrTooDeep) {
		return nil, fmt.Errorf("%w: %w", ErrNotRedactable, err)
	}
	if err != nil {
		return data, nil
	}
	var sel []selection
	for _, p := range r.body {
		sel = p.selectAll(&v, sel)
	}
	if maskSelections(sel) == 0 {
		return data, nil
	}
	return jsonvalue.Encode(v), nil
}

// SSEEvent returns one raw SSE event with the body rules applied to its data.
// Other fields and the line framing are kept. Data split over several lines
// is joined as the SSE format defines, and written back as one line if
// anything in it was masked.
func (r *Rules) SSEEvent(event []byte) ([]byte, error) {
	if len(r.body) == 0 {
		return event, nil
	}
	if r.cannotMatchEvent(event) {
		return event, nil
	}
	lines := sse.Lines(event)
	var dataIdx []int
	for i, l := range lines {
		if _, ok := sse.DataPrefix(l.Text); ok {
			dataIdx = append(dataIdx, i)
		}
	}
	joined, ok := sse.LinesData(lines)
	if !ok {
		return event, nil
	}
	red, err := r.Body(joined)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(red, joined) {
		return event, nil
	}

	var out bytes.Buffer
	for i, l := range lines {
		switch {
		case i == dataIdx[0]:
			out.WriteString("data: ")
			out.Write(red)
			out.Write(l.EOL)
		case slices.Contains(dataIdx, i):
			// merged into the first data line
		default:
			out.Write(l.Text)
			out.Write(l.EOL)
		}
	}
	return out.Bytes(), nil
}
