package mcp

import (
	"bytes"
	"fmt"
)

// EventMessages returns the JSON-RPC messages in the data of one SSE event,
// as stored in a chunk with its framing (spec §7.3). The data may hold a
// single message or a batch. An event without data, or with empty data such
// as the priming event a server sends for resumability, has no messages.
// Data that is not JSON-RPC is an error.
func EventMessages(event []byte) ([]Message, error) {
	data, ok := eventData(splitLines(event))
	if !ok || len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	body, err := ParseBody(data)
	if err != nil {
		return nil, fmt.Errorf("parsing SSE event data: %w", err)
	}
	return body.Messages, nil
}

// StreamMessages returns the JSON-RPC messages of an SSE response, in order,
// from its events as stored in chunks. An event whose data is not JSON-RPC is
// skipped, so one bad event does not hide the rest of the stream.
func StreamMessages(events [][]byte) []Message {
	var msgs []Message
	for _, ev := range events {
		m, err := EventMessages(ev)
		if err != nil {
			continue
		}
		msgs = append(msgs, m...)
	}
	return msgs
}

// sseLine is one line of an SSE event and its ending: "\n", "\r\n", "\r",
// or none for a last line without one.
type sseLine struct {
	text, eol []byte
}

// splitLines splits an SSE event into its lines.
func splitLines(event []byte) []sseLine {
	var lines []sseLine
	for len(event) > 0 {
		i := bytes.IndexAny(event, "\r\n")
		if i < 0 {
			lines = append(lines, sseLine{text: event})
			break
		}
		n := 1
		if event[i] == '\r' && i+1 < len(event) && event[i+1] == '\n' {
			n = 2
		}
		lines = append(lines, sseLine{text: event[:i], eol: event[i : i+n]})
		event = event[i+n:]
	}
	return lines
}

// dataPrefix returns the length of the field name, the colon and the one
// space dropped after it, when line is a data field line.
func dataPrefix(line []byte) (n int, ok bool) {
	field, value, _ := bytes.Cut(line, []byte(":"))
	if string(field) != "data" {
		return 0, false
	}
	return len(line) - len(bytes.TrimPrefix(value, []byte(" "))), true
}

// eventData returns the data of an SSE event: the values of its data lines
// joined with "\n" (WHATWG HTML, Server-sent events). ok is false when the
// event has no data line.
func eventData(lines []sseLine) (data []byte, ok bool) {
	var values [][]byte
	for _, l := range lines {
		if n, ok := dataPrefix(l.text); ok {
			values = append(values, l.text[n:])
		}
	}
	if values == nil {
		return nil, false
	}
	return bytes.Join(values, []byte("\n")), true
}
