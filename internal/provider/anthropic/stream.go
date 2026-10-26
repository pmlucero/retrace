package anthropic

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Stream reassembles a Message from the SSE events of a streamed response
// (ADR 0003: message_start, content_block_*, message_delta, message_stop,
// error). Feed it every event in order; ping and unknown events are ignored.
// The zero value is ready to use.
type Stream struct {
	msg    Message
	blocks []*streamBlock // by content block index
}

type streamBlock struct {
	typ     string
	text    strings.Builder
	id      string
	name    string
	input   []byte          // the block's initial input
	partial strings.Builder // input_json_delta fragments
	stopped bool
}

// Feed adds one raw SSE event, with its framing, as stored in a chunk.
func (s *Stream) Feed(event []byte) {
	data, ok := sseData(event)
	if !ok {
		return
	}
	// Every field is decoded on its own, so one with an unexpected type
	// (redacted, spec §3.5) does not hide the others.
	var ev struct {
		Type         json.RawMessage `json:"type"`
		Index        json.RawMessage `json:"index"`
		Message      json.RawMessage `json:"message"`
		ContentBlock json.RawMessage `json:"content_block"`
		Delta        json.RawMessage `json:"delta"`
		Usage        json.RawMessage `json:"usage"`
		Error        json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	var index *int
	if n := 0; json.Unmarshal(ev.Index, &n) == nil {
		index = &n
	}
	switch stringValue(ev.Type) {
	case "message_start":
		var m struct {
			Model json.RawMessage `json:"model"`
			Usage json.RawMessage `json:"usage"`
		}
		_ = json.Unmarshal(ev.Message, &m)
		s.msg.Model = stringValue(m.Model)
		s.msg.Usage = mergeUsage(s.msg.Usage, m.Usage)
	case "content_block_start":
		if b := s.block(index); b != nil {
			var cb contentBlock
			_ = json.Unmarshal(ev.ContentBlock, &cb) // a block that is not an object has no fields
			b.typ, b.id, b.name, b.input = cb.Type, stringValue(cb.ID), stringValue(cb.Name), cb.Input
			b.text.WriteString(stringValue(cb.Text))
		}
	case "content_block_delta":
		var d struct {
			Type        string          `json:"type"`
			Text        json.RawMessage `json:"text"`
			PartialJSON json.RawMessage `json:"partial_json"`
		}
		b := s.block(index)
		if b == nil || json.Unmarshal(ev.Delta, &d) != nil {
			return
		}
		switch d.Type {
		case "text_delta":
			b.text.WriteString(stringValue(d.Text))
		case "input_json_delta":
			b.partial.WriteString(stringValue(d.PartialJSON))
		}
	case "content_block_stop":
		if b := s.block(index); b != nil {
			b.stopped = true
		}
	case "message_delta":
		var d struct {
			StopReason json.RawMessage `json:"stop_reason"`
		}
		_ = json.Unmarshal(ev.Delta, &d)
		if r := stringValue(d.StopReason); r != "" {
			s.msg.StopReason = r
		}
		s.msg.Usage = mergeUsage(s.msg.Usage, ev.Usage)
	case "message_stop":
		s.msg.Complete = true
	case "error":
		s.msg.Error = apiError(ev.Error)
	}
}

// maxBlocks bounds the content block index a stream may use, so a malformed
// index cannot allocate without limit.
const maxBlocks = 1 << 16

func (s *Stream) block(index *int) *streamBlock {
	if index == nil || *index < 0 || *index >= maxBlocks {
		return nil
	}
	for len(s.blocks) <= *index {
		s.blocks = append(s.blocks, nil)
	}
	if s.blocks[*index] == nil {
		s.blocks[*index] = &streamBlock{}
	}
	return s.blocks[*index]
}

// Message returns the message reassembled so far. Only tool_use blocks that
// were stopped are included: a cut stream may leave one half written.
func (s *Stream) Message() Message {
	msg := s.msg
	msg.ToolUses = nil
	var text strings.Builder
	for _, b := range s.blocks {
		switch {
		case b == nil:
		case b.typ == "text":
			text.WriteString(b.text.String())
		case b.typ == "tool_use" && b.stopped:
			input := []byte(b.partial.String())
			if len(input) == 0 {
				input = b.input
			}
			msg.ToolUses = append(msg.ToolUses, ToolUse{ID: b.id, Name: b.name, Input: toolInput(input)})
		}
	}
	msg.Text = text.String()
	return msg
}

// sseData returns the data of one SSE event: its data lines joined with
// "\n" (WHATWG HTML, Server-sent events). ok is false when it has none.
func sseData(event []byte) ([]byte, bool) {
	var data [][]byte
	for len(event) > 0 {
		line := event
		i := bytes.IndexAny(event, "\r\n")
		if i >= 0 {
			line = event[:i]
			n := 1
			if event[i] == '\r' && i+1 < len(event) && event[i+1] == '\n' {
				n = 2
			}
			event = event[i+n:]
		} else {
			event = nil
		}
		field, value, _ := bytes.Cut(line, []byte(":"))
		if string(field) == "data" {
			data = append(data, bytes.TrimPrefix(value, []byte(" ")))
		}
	}
	if data == nil {
		return nil, false
	}
	return bytes.Join(data, []byte("\n")), true
}
