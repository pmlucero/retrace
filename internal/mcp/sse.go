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
	data, ok := eventData(event)
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

// eventData returns the data of one SSE event: the values of its data lines
// joined with "\n" (WHATWG HTML, Server-sent events). Lines end in "\n",
// "\r\n" or "\r", and one space after the colon is dropped. ok is false when
// the event has no data line.
func eventData(event []byte) (data []byte, ok bool) {
	var lines [][]byte
	for len(event) > 0 {
		line := event
		event = nil
		if i := bytes.IndexAny(line, "\r\n"); i >= 0 {
			n := 1
			if line[i] == '\r' && i+1 < len(line) && line[i+1] == '\n' {
				n = 2
			}
			line, event = line[:i], line[i+n:]
		}
		field, value, _ := bytes.Cut(line, []byte(":"))
		if string(field) == "data" {
			lines = append(lines, bytes.TrimPrefix(value, []byte(" ")))
		}
	}
	if lines == nil {
		return nil, false
	}
	return bytes.Join(lines, []byte("\n")), true
}
