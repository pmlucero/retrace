package redact

import (
	"bytes"
	"unicode/utf8"

	"github.com/pmlucero/retrace/internal/jsonvalue"
)

// The prefilter skips the JSON decode of a body in which no rule can match.
// A rule selects a value only through the member names in its path, so a
// body that holds none of those names, spelled as bytes, has nothing to
// mask. Skipping must never change what Body returns, so the filter gives up
// (the body is decoded as before) whenever the bytes could spell a name some
// other way, or the decode could fail with ErrTooDeep.

// literalNames returns, for each rule, one member name the rule needs in a
// body to select anything, or nil when the prefilter cannot be used. Any one
// name of the path would be sound; the longest is taken because it is the
// least likely to appear by chance. It returns nil if any rule has a
// wildcard or an index step, or a name that JSON writes differently than the
// rule does.
func literalNames(rules []Path) [][]byte {
	var out [][]byte
	for _, p := range rules {
		var longest string
		for _, s := range p.steps {
			if s.kind != stepChild && s.kind != stepRecursive || !plainName(s.name) {
				return nil
			}
			if len(s.name) > len(longest) {
				longest = s.name
			}
		}
		out = append(out, []byte(longest))
	}
	return out
}

// plainName reports whether a key called name is always written in JSON as
// those exact bytes, unless it holds a \u or \/ escape. It is false for
// control characters, quotes and backslashes (written with other escapes)
// and for U+FFFD (which invalid UTF-8 in a key decodes to).
func plainName(name string) bool {
	if name == "" || !utf8.ValidString(name) {
		return false
	}
	for _, c := range name {
		if c < ' ' || c == '"' || c == '\\' || c == utf8.RuneError {
			return false
		}
	}
	return true
}

// cannotMatch reports whether no rule can select anything in data and
// decoding it would return it unchanged without an error.
func (r *Rules) cannotMatch(data []byte) bool {
	if !r.noName(data) {
		return false
	}
	return !mayBeTooDeep(data)
}

// cannotMatchEvent is cannotMatch for a raw SSE event, before its data is
// cut out. Body checks the data again, so an event that holds many opening
// brackets is left to it.
func (r *Rules) cannotMatchEvent(event []byte) bool {
	return r.noName(event) && countOpens(event) <= jsonvalue.MaxDepth
}

// noName reports whether data holds no rule's name, and no escape that could
// spell one. It is false when the prefilter cannot be used.
func (r *Rules) noName(data []byte) bool {
	if r.literals == nil || mayEscape(data, r.literals) {
		return false
	}
	for _, lit := range r.literals {
		if bytes.Contains(data, lit) {
			return false
		}
	}
	return true
}

// mayEscape reports whether data holds a \u escape, which can spell any
// character, or a \/ escape when a name holds a slash. The other escapes
// spell only control characters, quotes and backslashes, which plainName
// excludes from names.
func mayEscape(data []byte, literals [][]byte) bool {
	if bytes.Contains(data, []byte(`\u`)) {
		return true
	}
	if !bytes.Contains(data, []byte(`\/`)) {
		return false
	}
	for _, lit := range literals {
		if bytes.IndexByte(lit, '/') >= 0 {
			return true
		}
	}
	return false
}

func countOpens(data []byte) int {
	return bytes.Count(data, []byte("[")) + bytes.Count(data, []byte("{"))
}

// endOfString returns the index of the quote that closes the string whose
// content starts at from, or len(data) if it is not closed. It jumps from
// quote to quote, so string content, most of a body, is not looped over.
func endOfString(data []byte, from int) int {
	for i := from; ; {
		j := bytes.IndexByte(data[i:], '"')
		if j < 0 {
			return len(data)
		}
		i += j
		slashes := 0
		for k := i - 1; k >= from && data[k] == '\\'; k-- {
			slashes++
		}
		if slashes%2 == 0 {
			return i
		}
		i++
	}
}

// mayBeTooDeep reports whether Decode could return ErrTooDeep for data. That
// needs more than MaxDepth opening brackets and a valid prefix that nests
// that deep. On a valid prefix this count is exact, and on anything else it
// only errs towards "yes" or is irrelevant, as the decode fails first.
func mayBeTooDeep(data []byte) bool {
	if countOpens(data) <= jsonvalue.MaxDepth {
		return false
	}
	depth := 0
	for i := 0; i < len(data); i++ {
		c := data[i]
		switch c {
		case '"':
			i = endOfString(data, i+1)
		case '[', '{':
			depth++
			if depth > jsonvalue.MaxDepth {
				return true
			}
		case ']', '}':
			depth--
		}
	}
	return false
}
