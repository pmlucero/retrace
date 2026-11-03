package cli

import (
	"bufio"
	"context"
	"encoding/json"
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

// seedShowSession writes a replay with metadata, a long argument, and gaps
// in its seqs: one before the first event, one in the middle, and one at
// the end that left no seq behind.
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
		response(2, "claude-haiku-9", 1_200, 80, cost(0.002)),
		toolCall(3, "search_orders", long),
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
		sc := bufio.NewScanner(strings.NewReader(r.stdout.String()))
		for sc.Scan() {
			var e showEvent
			if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
				t.Fatalf("line %q: %v", sc.Text(), err)
			}
			seqs = append(seqs, e.Seq)
		}
		if want := []int64{2, 3, 4, 6}; !slices.Equal(seqs, want) {
			t.Errorf("seqs = %v, want %v", seqs, want)
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
