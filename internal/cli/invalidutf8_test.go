package cli

import (
	"bytes"
	"encoding/json"
	"testing"
	"unicode/utf8"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/diff"
	"github.com/pmlucero/retrace/internal/divergence"
)

// rawFFFD is U+FFFD as UTF-8. encoding/json writes an invalid byte as the
// escape \ufffd up to Go 1.26 and as this raw rune from Go 1.27 (issue #214),
// so these tests pin the one form retrace writes on both: the raw rune.
var rawFFFD = []byte{0xEF, 0xBF, 0xBD}

// assertReplaced fails unless out is valid UTF-8, holds a raw U+FFFD and
// holds no \ufffd escape.
func assertReplaced(t *testing.T, out []byte) {
	t.Helper()
	if !utf8.Valid(out) {
		t.Errorf("output is not valid UTF-8:\n%q", out)
	}
	if !bytes.Contains(out, rawFFFD) {
		t.Errorf("output has no raw U+FFFD:\n%s", out)
	}
	if bytes.Contains(out, []byte(`\ufffd`)) {
		t.Errorf("output has the escape \\ufffd:\n%s", out)
	}
}

// AC-22, spec §9.6: invalid UTF-8 in show --json is replaced by U+FFFD, as a
// raw rune, whatever the Go version.
func TestShowJSONInvalidUTF8(t *testing.T) {
	bad := "a\xffb"
	e := cassette.Event{
		Seq: 1, Kind: "tool_call" + bad, Source: "s" + bad, Target: "t" + bad,
		Model: &bad, RequestedModel: &bad, Tool: &bad, Error: &bad,
		Status: "ok" + bad, Provenance: "recorded" + bad,
		Arguments: json.RawMessage(`{"q":"` + bad + `"}`),
		Result:    json.RawMessage("not json " + bad),
	}
	var out bytes.Buffer
	if err := writeShowJSON(&out, []cassette.Event{e}); err != nil {
		t.Fatal(err)
	}
	assertReplaced(t, out.Bytes())
	if got := bytes.Count(out.Bytes(), rawFFFD); got != 11 {
		t.Errorf("raw U+FFFD count = %d, want 11 (one per field):\n%s", got, out.Bytes())
	}
	var back showEvent
	if err := json.Unmarshal(out.Bytes(), &back); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out.Bytes())
	}
	if want := "a\uFFFDb"; *back.Tool != want || *back.Error != want {
		t.Errorf("tool = %q, error = %q, want %q", *back.Tool, *back.Error, want)
	}
}

// AC-21, spec §8.4: invalid UTF-8 in diff --format json is replaced by
// U+FFFD, as a raw rune, whatever the Go version.
func TestDiffJSONInvalidUTF8(t *testing.T) {
	bad := "a\xffb"
	ev := func(seq int64) *cassette.Event {
		return &cassette.Event{
			Seq: seq, Kind: "tool_call" + bad, Source: "s" + bad, Target: "t" + bad, Tool: &bad,
			Status:    "ok" + bad,
			Arguments: json.RawMessage(`{"q":"` + bad + `"}`),
			Result:    json.RawMessage("not json " + bad),
		}
	}
	diverged := &cassette.Event{Seq: 9, Result: json.RawMessage(`{"channel":"` + bad + `","upstream":"` + bad + `"}`)}
	session := cassette.Session{ID: "id" + bad, Mode: cassette.Mode("swap" + bad), Status: "ok" + bad, ParentSessionID: &bad}
	side := diffSide{session: session, diverged: diverged, divergedStep: 1}
	side.totals.Models = []string{bad}
	rep := diffReport{a: side, b: side, result: diff.Result{Point: &diff.Point{
		Step: 1, Kind: "tool_call" + bad, Field: "arguments" + bad, A: ev(1), B: ev(2),
		StopA: &diff.Stop{StopReason: bad}, StopB: &diff.Stop{Failed: true, Error: bad},
		Diff: divergence.Diff{Entries: []divergence.Entry{{
			Path: "$" + bad, Expected: json.RawMessage(`"x` + bad + `"`), Received: json.RawMessage("text " + bad),
		}}},
	}}}

	var out bytes.Buffer
	if err := rep.writeJSON(&out); err != nil {
		t.Fatal(err)
	}
	assertReplaced(t, out.Bytes())
	if !json.Valid(out.Bytes()) {
		t.Errorf("output is not JSON:\n%s", out.Bytes())
	}
}

// Sanitizing the JSON must not write through to the session it was built from.
func TestDiffJSONSanitizeKeepsInput(t *testing.T) {
	bad := "a\xff"
	side := diffSide{session: cassette.Session{ID: "ok", ParentSessionID: &bad}}
	side.totals.Models = []string{bad}
	_ = sessionJSON(side)
	if *side.session.ParentSessionID != bad || side.totals.Models[0] != bad {
		t.Error("sessionJSON changed its input")
	}
}
