package derive

import (
	"crypto/sha256"
	"encoding/json"
	"slices"
	"strings"

	"github.com/pmlucero/retrace/internal/jsonvalue"
)

// maxUnlinked caps the tool_use events a later tool_call may still link to.
// A tool_use the agent runs without MCP is never linked, so without a cap
// the list would grow with the session. An agent answers a tool_use within
// a turn or two, so the most recent ones are the only ones worth keeping.
// The value is a generous guess, not a measurement: a turn rarely holds more
// than a few dozen tool_use blocks. Changing it changes the derived events,
// so it needs a Version bump.
const maxUnlinked = 128

// toolUse is a tool_use event that no tool_call has linked to yet.
type toolUse struct {
	seq  int64
	name string
	args [sha256.Size]byte // hash of the canonical arguments: the list holds no payloads
}

// toolLinks links each tool_call to the tool_use it answers (spec §7.4). It
// holds the unlinked tool_use events, oldest first, at most maxUnlinked.
type toolLinks struct {
	unlinked []toolUse
}

// add records a tool_use event that a later tool_call may link to. One
// whose arguments have no canonical form can never be linked, so it is not
// kept.
func (l *toolLinks) add(seq int64, name string, input json.RawMessage) {
	args, ok := canonicalArgs(input)
	if !ok {
		return
	}
	if len(l.unlinked) == maxUnlinked {
		l.unlinked = slices.Delete(l.unlinked, 0, 1)
	}
	l.unlinked = append(l.unlinked, toolUse{seq: seq, name: name, args: args})
}

// link returns the seq of the tool_use that a tools/call of tool with
// arguments answers, and marks it linked, or returns nil when none
// qualifies (spec §7.4): it is the most recent unlinked tool_use whose
// canonical arguments are equal and whose name equals or ends with tool.
// "Ends with" is a plain suffix, as the spec says: agents prefix MCP tool
// names in many ways (weather__get_forecast, mcp__weather__get_forecast,
// weather-get_forecast). A tools/call without a name links to nothing,
// since every name ends with "".
//
// A tool_use derived after the tool_call is never linked to it: events are
// append-only, so a tool_call queued before the end of the streamed response
// whose tool_use it answers stays unlinked (docs/plan/mvp.md, "Where events
// are derived").
func (l *toolLinks) link(tool string, arguments json.RawMessage) *int64 {
	if tool == "" {
		return nil
	}
	args, ok := canonicalArgs(arguments)
	if !ok {
		return nil
	}
	for i := len(l.unlinked) - 1; i >= 0; i-- {
		tu := l.unlinked[i]
		if tu.args == args && strings.HasSuffix(tu.name, tool) {
			l.unlinked = slices.Delete(l.unlinked, i, i+1)
			return &tu.seq
		}
	}
	return nil
}

// canonicalArgs returns the hash of the canonical form of tool arguments,
// as matching encodes JSON (ADR 0007 §1): keys sorted, no insignificant
// whitespace, numbers in their original text. Arguments that are absent are
// {}, as a tool_use input that is missing is (anthropic.ToolUse), because
// MCP makes them optional for a tool that takes none. A tool_use input kept
// as a JSON string, because the streamed input was not valid JSON, is
// compared as that string. It reports false for arguments that are not JSON
// or are nested too deep to decode.
func canonicalArgs(raw json.RawMessage) ([sha256.Size]byte, bool) {
	if raw == nil {
		raw = json.RawMessage(`{}`)
	}
	v, err := jsonvalue.Decode(raw)
	if err != nil {
		return [sha256.Size]byte{}, false
	}
	return sha256.Sum256(jsonvalue.EncodeCanonical(v)), true
}
