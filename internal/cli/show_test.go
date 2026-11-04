package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
)

func runShowCmd(dir string, args ...string) recordRun {
	r := recordRun{stdout: &syncBuffer{}, stderr: &syncBuffer{}}
	all := append([]string{"--dir", dir, "show"}, args...)
	r.code = run(context.Background(), all, nil, r.stdout, r.stderr, fakeEnv(nil), commands)
	return r
}

// toolResult builds a tool_result event at seq, answering the tool_call at
// parent.
func toolResult(seq, parent int64, tool, result string) cassette.Event {
	return cassette.Event{
		Seq: seq, ParentSeq: &parent, Kind: "tool_result", Source: "mcp:shop", Target: "agent",
		StartedAt: lsT0.Add(time.Duration(seq) * time.Second), Tool: &tool,
		Result: json.RawMessage(result), Status: "ok", Provenance: "recorded",
	}
}

// later returns e with its started_at moved forward by d, to give it a
// sub-second time.
func later(e cassette.Event, d time.Duration) cassette.Event {
	e.StartedAt = e.StartedAt.Add(d)
	return e
}

// seedShowSession writes a replay with metadata, a long argument, gaps in
// its seqs (one before the first event, one in the middle, and one at the
// end that left no seq behind) and two events with a sub-second started_at.
func seedShowSession(t *testing.T, dir string, dropped int) {
	t.Helper()
	parent, model, exit := "checkout", "claude-haiku-9", 0
	long := `{"query": "` + strings.Repeat("é", 70) + `"}`
	seedSession(t, dir,
		cassette.Session{
			ID: "checkout.swap-model.1", Mode: "swap-model", ParentSessionID: &parent, Status: "recording",
			CreatedAt: lsT0, Command: []string{"python", "agent.py", "--fast"}, Cwd: "/work",
			ModelOverride: &model, MCPServers: json.RawMessage(`[{"name":"shop","url":"http://x"}]`),
			Labels: map[string]string{"team": "core", "env": "ci"}, RetraceVersion: "v0.1.0-dev",
		},
		&cassette.SessionEnd{Status: "completed", EndedAt: lsT0.Add(75 * time.Second), AgentExitCode: &exit, DroppedEvents: dropped},
		later(response(2, "claude-haiku-9", 1_200, 80, cost(0.002)), 500*time.Millisecond),
		later(toolCall(3, "search_orders", long), 123456*time.Microsecond),
		toolResult(4, 3, "search_orders", `{"orders":[]}`),
		response(6, "claude-haiku-9", 1_500, 90, nil),
	)
}

// AC-22, AC-27: show prints the session summary, a warning with the count of
// dropped events, and a timeline that marks each gap in seq.
func TestShowText(t *testing.T) {
	utcDisplay(t)
	dir := t.TempDir()
	seedShowSession(t, dir, 4)

	r := runShowCmd(dir, "checkout.swap-model.1")
	if r.code != ExitOK {
		t.Fatalf("exit code %d, stderr:\n%s", r.code, r.stderr)
	}
	checkGolden(t, "show/gaps.txt", r.stdout.String())
}

// AC-22: a session without gaps has no warning and no marker.
func TestShowTextWithoutGaps(t *testing.T) {
	utcDisplay(t)
	dir := t.TempDir()
	seedSession(t, dir,
		cassette.Session{ID: "s", Mode: "record", Status: "recording", CreatedAt: lsT0, RetraceVersion: "v0.1.0-dev"},
		nil,
		response(1, "claude-sonnet-4-5", 10, 5, cost(0.01)),
		toolCall(2, "t", `{"a": 1}`),
	)
	r := runShowCmd(dir, "s")
	if r.code != ExitOK {
		t.Fatalf("exit code %d, stderr:\n%s", r.code, r.stderr)
	}
	checkGolden(t, "show/recording.txt", r.stdout.String())
}

// AC-22, AC-27: show --json emits one event per line, only the events that
// exist, in seq order.
func TestShowJSON(t *testing.T) {
	dir := t.TempDir()
	seedShowSession(t, dir, 4)

	for _, args := range [][]string{{"checkout.swap-model.1", "--json"}, {"--json", "checkout.swap-model.1"}} {
		r := runShowCmd(dir, args...)
		if r.code != ExitOK {
			t.Fatalf("%v: exit code %d, stderr:\n%s", args, r.code, r.stderr)
		}
		checkGolden(t, "show/events.jsonl", r.stdout.String())

		var seqs []int64
		var times []string
		sc := bufio.NewScanner(strings.NewReader(r.stdout.String()))
		for sc.Scan() {
			var e showEvent
			if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
				t.Fatalf("line %q: %v", sc.Text(), err)
			}
			seqs = append(seqs, e.Seq)
			// Decode started_at again as a string: showEvent parses it into a
			// time.Time, which would hide the emitted form.
			var raw struct {
				StartedAt string `json:"started_at"`
			}
			if err := json.Unmarshal(sc.Bytes(), &raw); err != nil {
				t.Fatalf("line %q: %v", sc.Text(), err)
			}
			times = append(times, raw.StartedAt)
		}
		if want := []int64{2, 3, 4, 6}; !slices.Equal(seqs, want) {
			t.Errorf("seqs = %v, want %v", seqs, want)
		}
		// AC-22, spec §9.6: started_at is RFC 3339 in UTC with the trailing
		// zeros of the fraction left out, unlike the stored form of §7.1.
		wantTimes := []string{
			"2026-10-23T09:30:02.5Z",
			"2026-10-23T09:30:03.123456Z",
			"2026-10-23T09:30:04Z",
			"2026-10-23T09:30:06Z",
		}
		if !slices.Equal(times, wantTimes) {
			t.Errorf("started_at = %q, want %q", times, wantTimes)
		}
	}
}

// AC-22, spec §9.6: a stored cost that is not finite is emitted as null, so
// every line is still JSON.
func TestShowJSONNonFiniteCost(t *testing.T) {
	dir := t.TempDir()
	seedInvalidCosts(t, dir, "bad")

	r := runShowCmd(dir, "bad", "--json")
	if r.code != ExitOK {
		t.Fatalf("exit code %d, stderr:\n%s", r.code, r.stderr)
	}
	var costs []*float64
	sc := bufio.NewScanner(strings.NewReader(r.stdout.String()))
	for sc.Scan() {
		var e showEvent
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		costs = append(costs, e.CostUSD)
	}
	// Only the infinite costs become null: show --json emits the events as
	// stored, a negative cost included, and NaN was stored as NULL.
	want := []*float64{nil, nil, cost(-0.5), nil, cost(math.MaxFloat64), cost(math.MaxFloat64)}
	if len(costs) != len(want) {
		t.Fatalf("%d lines, want %d:\n%s", len(costs), len(want), r.stdout)
	}
	for i := range want {
		if (costs[i] == nil) != (want[i] == nil) || costs[i] != nil && *costs[i] != *want[i] {
			t.Errorf("line %d: cost_usd %v, want %v", i+1, costs[i], want[i])
		}
	}
}

func TestShowArguments(t *testing.T) {
	for _, tc := range []struct{ name, raw, want string }{
		{"absent", "", ""},
		{"compacted", `{ "a" : [1, 2] }`, `{"a":[1,2]}`},
		{"exactly the limit", `"` + strings.Repeat("x", 58) + `"`, `"` + strings.Repeat("x", 58) + `"`},
		{"cut by runes", `"` + strings.Repeat("é", 70) + `"`, `"` + strings.Repeat("é", 59) + "…"},
		{"not JSON", `{"a":`, `{"a":`},
		{"invalid UTF-8", "\"\xff\"", "\"�\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := showArguments(json.RawMessage(tc.raw)); got != tc.want {
				t.Errorf("showArguments(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// SECURITY.md: text from an untrusted cassette cannot drive the terminal or
// break the table.
func TestPrintable(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"search_orders", "search_orders"},
		{"héllo → wörld", "héllo → wörld"},
		{"\x1b]0;title\x07", `\x1b]0;title\a`},
		{"a\tb\r\nc", `a\tb\r\nc`},
		{"\u202egnp.exe", `\u202egnp.exe`},
		{"\xff", "\uFFFD"},
	} {
		if got := printable(tc.in); got != tc.want {
			t.Errorf("printable(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A tool name with control characters is escaped in the timeline, and the
// columns stay one line per event.
func TestShowEscapesUntrustedText(t *testing.T) {
	dir := t.TempDir()
	seedSession(t, dir,
		cassette.Session{ID: "s", Mode: "record", Status: "recording", CreatedAt: lsT0, Cwd: "/w\x1b[2J"},
		nil, toolCall(1, "evil\x1b[31m\ttool\n", `{}`))
	r := runShowCmd(dir, "s")
	if r.code != ExitOK {
		t.Fatalf("exit code %d, stderr:\n%s", r.code, r.stderr)
	}
	out := r.stdout.String()
	if strings.ContainsRune(out, '\x1b') || strings.Contains(out, "\t") {
		t.Errorf("output holds raw control characters:\n%q", out)
	}
	for _, want := range []string{`evil\x1b[31m\ttool\n`, `/w\x1b[2J`} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %s:\n%s", want, out)
		}
	}
}

// AC-27, spec §7.6: after a reindex of a diverged session the seqs are
// contiguous while dropped_events is kept, so the count has no gap to mark.
// show says how many are missing without making up their seqs. While a
// session is recording the count is 0, and a gap in seq is still marked.
func TestShowGapsWithoutSeqs(t *testing.T) {
	utcDisplay(t)
	for _, tc := range []struct {
		name    string
		end     *cassette.SessionEnd
		seqs    []int64
		want    []string
		wantNot []string
	}{
		{
			name:    "reindexed diverged session",
			end:     &cassette.SessionEnd{Status: "diverged", EndedAt: lsT0.Add(time.Minute), DroppedEvents: 2},
			seqs:    []int64{1, 2, 3},
			want:    []string{"2 events were not written", "2 events missing after seq 3"},
			wantNot: []string{"seq 4"},
		},
		{
			name:    "no event written",
			end:     &cassette.SessionEnd{Status: "failed", EndedAt: lsT0.Add(time.Minute), DroppedEvents: 2},
			want:    []string{"2 events missing\n"},
			wantNot: []string{"after seq"},
		},
		{
			name:    "recording with a gap",
			seqs:    []int64{1, 3},
			want:    []string{"1 event missing (seq 2)"},
			wantNot: []string{"warning", "after seq"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			var events []cassette.Event
			for _, seq := range tc.seqs {
				events = append(events, toolCall(seq, "t", `{}`))
			}
			seedSession(t, dir, cassette.Session{ID: "s", Mode: "exact", Status: "recording", CreatedAt: lsT0}, tc.end, events...)
			r := runShowCmd(dir, "s")
			if r.code != ExitOK {
				t.Fatalf("exit code %d, stderr:\n%s", r.code, r.stderr)
			}
			out := r.stdout.String()
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("output lacks %q:\n%s", w, out)
				}
			}
			for _, w := range tc.wantNot {
				if strings.Contains(out, w) {
					t.Errorf("output has %q:\n%s", w, out)
				}
			}
		})
	}
}

func TestJSONOrString(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"", "null"},
		{`{"a":1}`, `{"a":1}`},
		{`{"a":`, `"{\"a\":"`},
	} {
		got, err := json.Marshal(jsonOrString(json.RawMessage(tc.raw)))
		if err != nil || string(got) != tc.want {
			t.Errorf("jsonOrString(%q) = %s, %v; want %s", tc.raw, got, err, tc.want)
		}
	}
}

func TestShowErrors(t *testing.T) {
	dir := t.TempDir()
	seedSession(t, dir, cassette.Session{ID: "s", Mode: "record", Status: "recording", CreatedAt: lsT0}, nil)
	c, err := cassette.Create(context.Background(), cassette.Path(dir, "empty"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		args   []string
		code   int
		stderr string
	}{
		{"no id", nil, ExitUsage, "exactly one session id"},
		{"two ids", []string{"s", "t"}, ExitUsage, "exactly one session id"},
		{"invalid id", []string{"../x"}, ExitUsage, "session id"},
		{"unknown flag", []string{"s", "--nope"}, ExitUsage, "flag provided but not defined"},
		{"missing session", []string{"nope"}, ExitError, "no such session"},
		{"no session row", []string{"empty"}, ExitError, "cannot read the session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runShowCmd(dir, tc.args...)
			if r.code != tc.code || !strings.Contains(r.stderr.String(), tc.stderr) {
				t.Errorf("exit code %d, stderr %q; want %d and %q", r.code, r.stderr, tc.code, tc.stderr)
			}
		})
	}
}
