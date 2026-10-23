package redact

import (
	"bytes"
	"slices"
	"strings"
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

// Body returns data with the values selected by the body rules masked
// (spec §3.5). A body that is not JSON, or has no match, is returned as it is.
func (r *Rules) Body(data []byte) []byte {
	if len(r.body) == 0 {
		return data
	}
	v, err := decodeJSON(data)
	if err != nil {
		return data
	}
	n := 0
	for _, p := range r.body {
		n += p.apply(&v)
	}
	if n == 0 {
		return data
	}
	return encodeJSON(v)
}

// SSEEvent returns one raw SSE event with the body rules applied to its data.
// Other fields and the line framing are kept. Data split over several lines
// is joined as the SSE format defines, and written back as one line if
// anything in it was masked.
func (r *Rules) SSEEvent(event []byte) []byte {
	if len(r.body) == 0 {
		return event
	}
	lines := splitLines(event)
	var (
		dataIdx []int
		data    []string
	)
	for i, l := range lines {
		if value, ok := dataValue(l.text); ok {
			dataIdx = append(dataIdx, i)
			data = append(data, value)
		}
	}
	if len(dataIdx) == 0 {
		return event
	}
	joined := []byte(strings.Join(data, "\n"))
	red := r.Body(joined)
	if bytes.Equal(red, joined) {
		return event
	}

	var out bytes.Buffer
	for i, l := range lines {
		switch {
		case i == dataIdx[0]:
			out.WriteString("data: ")
			out.Write(red)
			out.WriteString(l.eol)
		case slices.Contains(dataIdx, i):
			// merged into the first data line
		default:
			out.WriteString(l.text)
			out.WriteString(l.eol)
		}
	}
	return out.Bytes()
}

type sseLine struct {
	text, eol string
}

// splitLines splits an SSE event into lines, keeping each line's ending
// ("\n", "\r\n" or "\r").
func splitLines(event []byte) []sseLine {
	var lines []sseLine
	s := string(event)
	for s != "" {
		i := strings.IndexAny(s, "\r\n")
		if i < 0 {
			lines = append(lines, sseLine{text: s})
			break
		}
		eol := s[i : i+1]
		if s[i] == '\r' && i+1 < len(s) && s[i+1] == '\n' {
			eol = "\r\n"
		}
		lines = append(lines, sseLine{text: s[:i], eol: eol})
		s = s[i+len(eol):]
	}
	return lines
}

// dataValue returns the value of a "data" field line: everything after the
// colon, minus one leading space (SSE format).
func dataValue(line string) (string, bool) {
	if line == "data" {
		return "", true
	}
	v, ok := strings.CutPrefix(line, "data:")
	if !ok {
		return "", false
	}
	return strings.TrimPrefix(v, " "), true
}
