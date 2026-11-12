// Package divergence describes how a request that missed the cassette
// differs from the recorded request it was expected to repeat (spec §5
// step 4).
package divergence

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/pmlucero/retrace/internal/jsonvalue"
)

const (
	// MaxEntries caps the entries of a Diff (spec §5 step 4).
	MaxEntries = 50
	// MaxValueBytes caps the compact JSON of one value in an Entry. A longer
	// value is elided, so one divergence event stays small even when a
	// message with a large tool result or image differs.
	MaxValueBytes = 1024
)

// Entry is one path where the recorded and received requests differ. A nil
// Expected or Received means the path does not exist on that side, and the
// member is omitted from its JSON.
type Entry struct {
	Path     string          `json:"path"`
	Expected json.RawMessage `json:"expected,omitempty"`
	Received json.RawMessage `json:"received,omitempty"`
}

// Diff is the structural diff of a cassette miss. Truncated is set when
// more than MaxEntries paths differ or a value was elided; MorePaths only in
// the first case.
type Diff struct {
	Entries   []Entry
	Truncated bool
	MorePaths bool
}

// Compare returns the paths where received differs from expected. Both are
// canonical forms (spec §5 step 1), already redacted, in the shape the diff
// paths are relative to. Objects are compared member by member and arrays
// by index, and an entry is emitted at the deepest path that differs. When
// either side is not JSON, the two are compared as whole strings at "$".
func Compare(expected, received []byte) Diff {
	c := &collector{}
	ev, errE := jsonvalue.Decode(expected)
	rv, errR := jsonvalue.Decode(received)
	if errE != nil || errR != nil {
		if !bytes.Equal(expected, received) {
			c.add("$", text(expected), text(received))
		}
		return c.diff()
	}
	c.walk("$", ev, rv)
	return c.diff()
}

// text is a body that is not JSON, as the JSON string the diff shows.
func text(b []byte) jsonvalue.Value { return strings.ToValidUTF8(string(b), "�") }

type collector struct {
	entries []Entry
	full    bool // a further differing path was found past MaxEntries
	elided  bool
}

func (c *collector) diff() Diff {
	entries := c.entries
	if entries == nil {
		entries = []Entry{}
	}
	return Diff{Entries: entries, Truncated: c.full || c.elided, MorePaths: c.full}
}

// missing marks a side where a path does not exist.
type missing struct{}

func (c *collector) walk(path string, e, r jsonvalue.Value) {
	if c.full {
		return
	}
	switch ev := e.(type) {
	case *jsonvalue.Object:
		if rv, ok := r.(*jsonvalue.Object); ok {
			c.walkObjects(path, ev, rv)
			return
		}
	case jsonvalue.Array:
		if rv, ok := r.(jsonvalue.Array); ok {
			c.walkArrays(path, ev, rv)
			return
		}
	default:
		if equalScalars(e, r) {
			return
		}
	}
	c.add(path, e, r)
}

func (c *collector) walkObjects(path string, e, r *jsonvalue.Object) {
	inExpected := make(map[string]bool, len(e.Members))
	for _, m := range e.Members {
		if inExpected[m.Key] {
			continue // a duplicate: the first member is the one compared
		}
		inExpected[m.Key] = true
		if rv, ok := member(r, m.Key); ok {
			c.walk(path+name(m.Key), m.Value, rv)
		} else {
			c.add(path+name(m.Key), m.Value, missing{})
		}
	}
	seen := map[string]bool{}
	for _, m := range r.Members {
		if inExpected[m.Key] || seen[m.Key] {
			continue
		}
		seen[m.Key] = true
		c.add(path+name(m.Key), missing{}, m.Value)
	}
}

func (c *collector) walkArrays(path string, e, r jsonvalue.Array) {
	for i := range max(len(e), len(r)) {
		p := path + "[" + strconv.Itoa(i) + "]"
		switch {
		case i >= len(r):
			c.add(p, e[i], missing{})
		case i >= len(e):
			c.add(p, missing{}, r[i])
		default:
			c.walk(p, e[i], r[i])
		}
	}
}

func member(o *jsonvalue.Object, key string) (jsonvalue.Value, bool) {
	for _, m := range o.Members {
		if m.Key == key {
			return m.Value, true
		}
	}
	return nil, false
}

// equalScalars reports whether two values that are not both objects or both
// arrays are the same. Canonical forms keep number text, so numbers compare
// by text, as their fingerprints do.
func equalScalars(e, r jsonvalue.Value) bool {
	switch ev := e.(type) {
	case string:
		rv, ok := r.(string)
		return ok && ev == rv
	case json.Number:
		rv, ok := r.(json.Number)
		return ok && ev == rv
	case bool:
		rv, ok := r.(bool)
		return ok && ev == rv
	case nil:
		return r == nil
	}
	return false
}

func (c *collector) add(path string, e, r jsonvalue.Value) {
	if len(c.entries) == MaxEntries {
		c.full = true
		return
	}
	c.entries = append(c.entries, Entry{Path: path, Expected: c.value(e), Received: c.value(r)})
}

func (c *collector) value(v jsonvalue.Value) json.RawMessage {
	if _, ok := v.(missing); ok {
		return nil
	}
	b := jsonvalue.Encode(v)
	if len(b) > MaxValueBytes {
		c.elided = true
		return jsonvalue.Encode(fmt.Sprintf("[retrace: %d bytes elided]", len(b)))
	}
	return b
}

// name is the path step for member key, in the JSONPath subset of spec §3.5:
// .key for a name of letters, digits, '_' and '-', ['key'] otherwise. The
// key is kept as received, control characters included: the path is stored
// in the divergence event, and the text outputs escape it when they print it.
func name(key string) string {
	if key != "" && strings.IndexFunc(key, func(r rune) bool { return !isNameRune(r) }) < 0 {
		return "." + key
	}
	return "['" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(key) + "']"
}

func isNameRune(r rune) bool {
	return r == '_' || r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
}
