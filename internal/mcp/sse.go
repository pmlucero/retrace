package mcp

import (
	"bytes"
	"fmt"

	"github.com/pmlucero/retrace/internal/sse"
)

// EventMessages returns the JSON-RPC messages in the data of one SSE event,
// as stored in a chunk with its framing (spec §7.3). The data may hold a
// single message or a batch. An event without data, or with empty data such
// as the priming event a server sends for resumability, has no messages.
// Data that is not JSON-RPC is an error.
func EventMessages(event []byte) ([]Message, error) {
	data, ok := sse.Data(event)
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
