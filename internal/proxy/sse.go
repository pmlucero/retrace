package proxy

import "bytes"

// sseSplitter cuts a byte stream into Server-Sent Events. An event ends with
// a blank line, and lines end with "\n", "\r\n" or "\r" (WHATWG HTML, Server-
// sent events). Each event is returned with its framing, exactly as received
// (spec §7.3).
type sseSplitter struct {
	buf  []byte
	scan int // bytes of buf already searched without finding an event end
}

// feed appends b and returns the events it completed.
func (s *sseSplitter) feed(b []byte) [][]byte {
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

// rest returns the bytes after the last complete event.
func (s *sseSplitter) rest() []byte {
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
