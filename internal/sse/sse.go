// Package sse parses Server-Sent Events as retrace stores and replays them:
// events keep their framing byte for byte (spec §7.3). The rules are those of
// WHATWG HTML, Server-sent events: lines end with "\n", "\r\n" or "\r", and a
// blank line ends an event.
//
// It is a leaf package: it imports only the standard library.
package sse

import "bytes"

// Splitter cuts a byte stream into events. An event ends with a blank line.
// Each event is returned with its framing, exactly as received. The zero
// value is ready to use.
type Splitter struct {
	buf  []byte
	scan int // bytes of buf already searched without finding an event end
}

// Feed appends b and returns the events it completed. The events are copies:
// the caller may keep them.
func (s *Splitter) Feed(b []byte) [][]byte {
	s.buf = append(s.buf, b...)
	var events [][]byte
	for {
		end := eventEnd(s.buf, s.scan)
		if end < 0 {
			// Keep searching from just before the end: a terminator can span
			// two reads.
			s.scan = max(0, len(s.buf)-3)
			return events
		}
		events = append(events, bytes.Clone(s.buf[:end]))
		s.buf = s.buf[end:]
		s.scan = 0
	}
}

// Rest returns the bytes after the last complete event. It is valid until the
// next call to Feed.
func (s *Splitter) Rest() []byte {
	return s.buf
}

// eventEnd returns the index just past the blank line that ends the first
// event in b, searching from from, or -1.
func eventEnd(b []byte, from int) int {
	for i := from; i < len(b); i++ {
		n := lineBreak(b, i)
		if n == 0 {
			continue
		}
		if pendingCR(b, i, n) {
			return -1
		}
		j := i + n
		if m := lineBreak(b, j); m > 0 {
			if pendingCR(b, j, m) {
				return -1
			}
			return j + m
		}
		i = j - 1
	}
	return -1
}

// pendingCR reports whether the line break at b[i] is a "\r" that ends the
// buffer: the next read may start with "\n", making it one "\r\n".
func pendingCR(b []byte, i, n int) bool {
	return n == 1 && b[i] == '\r' && i+1 == len(b)
}

// lineBreak returns the length of the line break at b[i], or 0.
func lineBreak(b []byte, i int) int {
	if i >= len(b) {
		return 0
	}
	switch b[i] {
	case '\n':
		return 1
	case '\r':
		if i+1 < len(b) && b[i+1] == '\n' {
			return 2
		}
		return 1
	}
	return 0
}

// Line is one line of an event and its ending: "\n", "\r\n", "\r", or none
// for a last line without one. Both slices alias the event.
type Line struct {
	Text, EOL []byte
}

// Lines splits an event into its lines, keeping each ending, so that
// concatenating Text and EOL of every line gives the event back.
func Lines(event []byte) []Line {
	var lines []Line
	for len(event) > 0 {
		var l Line
		l, event = nextLine(event)
		lines = append(lines, l)
	}
	return lines
}

// nextLine returns the first line of a non-empty event and the rest of it.
func nextLine(event []byte) (Line, []byte) {
	i := bytes.IndexAny(event, "\r\n")
	if i < 0 {
		return Line{Text: event}, nil
	}
	n := 1
	if event[i] == '\r' && i+1 < len(event) && event[i+1] == '\n' {
		n = 2
	}
	return Line{Text: event[:i], EOL: event[i : i+n]}, event[i+n:]
}

// DataPrefix returns the length of the field name, the colon and the one
// space dropped after it, when line is a data field line. A line "data"
// without a colon is a data field with an empty value.
func DataPrefix(line []byte) (n int, ok bool) {
	field, value, _ := bytes.Cut(line, []byte(":"))
	if string(field) != "data" {
		return 0, false
	}
	return len(line) - len(bytes.TrimPrefix(value, []byte(" "))), true
}

// Data returns the data of an event: the values of its data lines joined with
// "\n". ok is false when the event has no data line. Comment lines and other
// fields are not data.
func Data(event []byte) (data []byte, ok bool) {
	var values [][]byte
	for len(event) > 0 {
		var l Line
		l, event = nextLine(event)
		if n, ok := DataPrefix(l.Text); ok {
			values = append(values, l.Text[n:])
		}
	}
	if values == nil {
		return nil, false
	}
	return bytes.Join(values, []byte("\n")), true
}

// LinesData is Data for an event already split with Lines.
func LinesData(lines []Line) (data []byte, ok bool) {
	var values [][]byte
	for _, l := range lines {
		if n, ok := DataPrefix(l.Text); ok {
			values = append(values, l.Text[n:])
		}
	}
	if values == nil {
		return nil, false
	}
	return bytes.Join(values, []byte("\n")), true
}
