package cli

import (
	"encoding/json"
	"io"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/diff"
)

// diffFormatVersion is the format_version of `diff --format json`. A change
// that is not backward compatible increments it and adds a schema file next
// to docs/schema/diff.v1.json (spec §8.4).
const diffFormatVersion = 1

// diffDocument is the document of `diff --format json`. Its schema is
// docs/schema/diff.v1.json, so a field added here goes there too.
type diffDocument struct {
	FormatVersion   int             `json:"format_version"`
	A               diffSessionJSON `json:"a"`
	B               diffSessionJSON `json:"b"`
	Identical       bool            `json:"identical"`
	FirstDivergence *diffPointJSON  `json:"first_divergence"`
	IdenticalSteps  *diffSpanJSON   `json:"identical_steps"`
	TextCompared    bool            `json:"text_compared"`
	TextDifferences []int           `json:"text_differences"`
}

// diffSessionJSON is a session as `ls --json` describes it, plus its first
// divergence event, the strict stop of spec §8.3 step 4.
type diffSessionJSON struct {
	lsEntry
	Divergence *diffStopJSON `json:"divergence"`
}

// diffStopJSON is where a side stopped on a cassette miss. Channel and
// Upstream come from the divergence event's result (spec §7.4), and are
// null when an untrusted cassette's result does not hold them.
type diffStopJSON struct {
	Step     int     `json:"step"`
	Seq      int64   `json:"seq"`
	Channel  *string `json:"channel"`
	Upstream *string `json:"upstream"`
}

// diffPointJSON is the divergence point (spec §8.3 step 3). Its diff values
// are the JSON values themselves, not the 80-character text form.
type diffPointJSON struct {
	Step          int             `json:"step"`
	Kind          string          `json:"kind"`
	Field         *string         `json:"field"`
	A             *diffEventJSON  `json:"a"`
	B             *diffEventJSON  `json:"b"`
	StopA         *diffMarkerJSON `json:"stop_a"`
	StopB         *diffMarkerJSON `json:"stop_b"`
	Diff          []diffEntryJSON `json:"diff"`
	DiffTruncated bool            `json:"diff_truncated"`
	MorePaths     bool            `json:"more_paths"`
}

// diffEventJSON is the part of an event that action identity compares
// (spec §8.3 step 1), with its seq to find it in `show`.
type diffEventJSON struct {
	Seq       int64           `json:"seq"`
	Kind      string          `json:"kind"`
	Source    string          `json:"source"`
	Target    string          `json:"target"`
	Tool      *string         `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	Result    json.RawMessage `json:"result"`
	Status    string          `json:"status"`
}

// diffMarkerJSON is a stop marker (spec §8.2). Failed is set when the last
// response's status is error, and then StopReason is null. StopReason and
// Error are also null when an untrusted cassette does not hold them.
type diffMarkerJSON struct {
	Failed     bool    `json:"failed"`
	StopReason *string `json:"stop_reason"`
	Error      *string `json:"error"`
}

// diffEntryJSON is one path of the argument-level diff. A side without the
// path has no member, as in the divergence report (spec §6.4).
type diffEntryJSON struct {
	Path     string          `json:"path"`
	Expected json.RawMessage `json:"expected,omitempty"`
	Received json.RawMessage `json:"received,omitempty"`
}

type diffSpanJSON struct {
	First     int `json:"first"`
	Last      int `json:"last"`
	ToolCalls int `json:"tool_calls"`
}

// writeJSON writes the JSON output (spec §8.4): the same information as
// writeText, unabbreviated.
func (r diffReport) writeJSON(w io.Writer) error {
	res := r.result
	doc := diffDocument{
		FormatVersion:   diffFormatVersion,
		A:               sessionJSON(r.a),
		B:               sessionJSON(r.b),
		Identical:       res.Point == nil,
		FirstDivergence: pointJSON(res.Point),
		TextCompared:    r.textCompared,
		TextDifferences: res.TextDifferences,
	}
	if doc.TextDifferences == nil {
		doc.TextDifferences = []int{}
	}
	if span := res.Identical; span.Last >= span.First {
		doc.IdenticalSteps = &diffSpanJSON{First: span.First, Last: span.Last, ToolCalls: span.ToolCalls}
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

func sessionJSON(s diffSide) diffSessionJSON {
	out := diffSessionJSON{lsEntry: newLsEntry(s.session, s.totals)}
	if e := s.diverged; e != nil {
		var res struct{ Channel, Upstream *string }
		_ = json.Unmarshal(e.Result, &res) // unreadable: both stay null
		out.Divergence = &diffStopJSON{Step: s.divergedStep, Seq: e.Seq, Channel: res.Channel, Upstream: res.Upstream}
	}
	return out
}

func pointJSON(p *diff.Point) *diffPointJSON {
	if p == nil {
		return nil
	}
	out := &diffPointJSON{
		Step: p.Step, Kind: p.Kind, A: eventJSON(p.A), B: eventJSON(p.B),
		StopA: markerJSON(p.StopA), StopB: markerJSON(p.StopB),
		Diff:          make([]diffEntryJSON, 0, len(p.Diff.Entries)),
		DiffTruncated: p.Diff.Truncated, MorePaths: p.Diff.MorePaths,
	}
	if p.Field != "" {
		out.Field = &p.Field
	}
	for _, e := range p.Diff.Entries {
		out.Diff = append(out.Diff, diffEntryJSON{Path: e.Path, Expected: jsonOrString(e.Expected), Received: jsonOrString(e.Received)})
	}
	return out
}

func eventJSON(e *cassette.Event) *diffEventJSON {
	if e == nil {
		return nil
	}
	return &diffEventJSON{
		Seq: e.Seq, Kind: e.Kind, Source: e.Source, Target: e.Target, Tool: e.Tool,
		Arguments: jsonOrString(e.Arguments), Result: jsonOrString(e.Result), Status: e.Status,
	}
}

func markerJSON(s *diff.Stop) *diffMarkerJSON {
	if s == nil {
		return nil
	}
	out := &diffMarkerJSON{Failed: s.Failed}
	if s.StopReason != "" {
		out.StopReason = &s.StopReason
	}
	if s.Error != "" {
		out.Error = &s.Error
	}
	return out
}
