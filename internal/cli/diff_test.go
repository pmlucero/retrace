package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/diff"
	"github.com/pmlucero/retrace/internal/divergence"
)

func runDiffCmd(dir string, args ...string) recordRun {
	r := recordRun{stdout: &syncBuffer{}, stderr: &syncBuffer{}}
	all := append([]string{"--dir", dir, "diff"}, args...)
	r.code = run(context.Background(), all, nil, r.stdout, r.stderr, fakeEnv(nil), commands)
	return r
}

// modelResponse builds a model_response event at seq with a stop reason and
// a text.
func modelResponse(seq int64, model, stop, text string) cassette.Event {
	e := response(seq, model, 1_000, 100, cost(0.01))
	e.Result, _ = json.Marshal(map[string]string{"stop_reason": stop, "text": text})
	return e
}

// toolUse builds a tool_use event at seq, emitted by the model_response at
// parent.
func toolUse(seq, parent int64, tool, args string) cassette.Event {
	return cassette.Event{
		Seq: seq, ParentSeq: &parent, Kind: "tool_use", Source: "model", Target: "agent",
		StartedAt: lsT0.Add(time.Duration(seq) * time.Second), Tool: &tool,
		Arguments: json.RawMessage(args), Status: "ok", Provenance: "recorded",
	}
}

// missEvent builds the divergence event of a cassette miss on upstream.
func missEvent(seq int64, upstream string) cassette.Event {
	msg := "retrace: cassette miss: no recorded request matches"
	return cassette.Event{
		Seq: seq, Kind: "divergence", Source: "retrace", Target: "agent",
		StartedAt: lsT0.Add(time.Duration(seq) * time.Second),
		Result:    json.RawMessage(`{"channel":"mcp","upstream":"` + upstream + `","received_fingerprint":null,"diff":[]}`),
		Status:    "divergence", Error: &msg, Provenance: "replayed",
	}
}

// checkoutEvents are the events of a recording: two steps that call
// search_orders, and a last one that ends the turn.
func checkoutEvents() []cassette.Event {
	return []cassette.Event{
		modelResponse(1, "claude-sonnet-4-5", "tool_use", "Looking."),
		toolUse(2, 1, "search_orders", `{"customer_id": "c_812"}`),
		toolCall(3, "search_orders", `{"customer_id":"c_812"}`),
		toolResult(4, 3, "search_orders", `{"orders":[]}`),
		modelResponse(5, "claude-sonnet-4-5", "tool_use", "Found none."),
		toolUse(6, 5, "search_orders", `{"customer_id":"c_812","status":"open"}`),
		toolCall(7, "search_orders", `{"customer_id":"c_812","status":"open"}`),
		toolResult(8, 7, "search_orders", `{"orders":[{"id":"o_1"}]}`),
		modelResponse(9, "claude-sonnet-4-5", "end_turn", "Done."),
	}
}

func seedCheckout(t *testing.T, dir string, dropped int) {
	t.Helper()
	seedSession(t, dir, cassette.Session{ID: "checkout", Mode: "record", Status: "recording", CreatedAt: lsT0},
		&cassette.SessionEnd{Status: "completed", EndedAt: lsT0.Add(38200 * time.Millisecond), DroppedEvents: dropped},
		checkoutEvents()...)
}

// seedSwap writes session id, a swap-model replay of checkout whose model phrases step 1
// differently and drops an argument in step 2, which then misses the
// cassette.
func seedSwap(t *testing.T, dir, id string) {
	t.Helper()
	parent := "checkout"
	seedSession(t, dir,
		cassette.Session{ID: id, Mode: "swap-model", ParentSessionID: &parent, Status: "recording", CreatedAt: lsT0},
		&cassette.SessionEnd{Status: "diverged", EndedAt: lsT0.Add(11900 * time.Millisecond)},
		modelResponse(1, "claude-haiku-4-5", "tool_use", "Let me look."),
		toolUse(2, 1, "search_orders", `{"customer_id":"c_812"}`),
		toolCall(3, "search_orders", `{"customer_id":"c_812"}`),
		toolResult(4, 3, "search_orders", `{"orders":[]}`),
		modelResponse(5, "claude-haiku-4-5", "tool_use", "Found none."),
		toolUse(6, 5, "search_orders", `{"customer_id":"c_812"}`),
		toolCall(7, "search_orders", `{"customer_id":"c_812"}`),
		missEvent(8, "mcp:shop"),
	)
}

// diffCase is a diff of seeded sessions, whose output in each format is
// checked against a golden file: golden plus .txt or .json.
type diffCase struct {
	name   string
	seed   func(t *testing.T, dir string)
	args   []string
	code   int
	golden string
}

// diffCases are the cases of TestDiffText and TestDiffJSON.
func diffCases() []diffCase {
	return []diffCase{
		{
			name: "argument diff",
			seed: func(t *testing.T, dir string) { seedCheckout(t, dir, 0); seedSwap(t, dir, "checkout.swap-model.1") },
			args: []string{"checkout", "checkout.swap-model.1"}, code: ExitDiffFound, golden: "diverged",
		},
		{
			// AC-27: the warning comes first, and the exit code is unchanged.
			name: "dropped events",
			seed: func(t *testing.T, dir string) { seedCheckout(t, dir, 3); seedSwap(t, dir, "checkout.swap-model.1") },
			args: []string{"checkout", "checkout.swap-model.1"}, code: ExitDiffFound, golden: "gaps",
		},
		{
			name: "identical, one never ended",
			seed: func(t *testing.T, dir string) {
				seedCheckout(t, dir, 0)
				seedSession(t, dir, cassette.Session{ID: "live", Mode: "exact", Status: "recording", CreatedAt: lsT0},
					nil, checkoutEvents()...)
			},
			args: []string{"checkout", "live"}, code: ExitOK, golden: "identical",
		},
		{
			// Spec §8.3 step 4: two replays that stopped at the same miss
			// are identical, and both stops are reported.
			name: "both stopped",
			seed: func(t *testing.T, dir string) { seedSwap(t, dir, "a"); seedSwap(t, dir, "b") },
			args: []string{"a", "b"}, code: ExitOK, golden: "both-stopped",
		},
		{
			// A replay that missed before any model response: no models,
			// and its events are in step 0.
			name: "no model response",
			seed: func(t *testing.T, dir string) {
				seedCheckout(t, dir, 0)
				seedSession(t, dir, cassette.Session{ID: "early", Mode: "exact", Status: "recording", CreatedAt: lsT0},
					&cassette.SessionEnd{Status: "diverged", EndedAt: lsT0.Add(2 * time.Second)},
					toolCall(1, "search_orders", `{"customer_id":"c_812"}`), missEvent(2, "anthropic"))
			},
			args: []string{"checkout", "early"}, code: ExitDiffFound, golden: "no-model",
		},
		{
			name: "text compared",
			seed: func(t *testing.T, dir string) { seedCheckout(t, dir, 0); seedSwap(t, dir, "checkout.swap-model.1") },
			args: []string{"--text", "checkout", "checkout.swap-model.1"}, code: ExitDiffFound, golden: "text",
		},
		{
			name: "fewer steps",
			seed: func(t *testing.T, dir string) {
				seedCheckout(t, dir, 0)
				seedSession(t, dir, cassette.Session{ID: "short", Mode: "exact", Status: "recording", CreatedAt: lsT0},
					&cassette.SessionEnd{Status: "completed", EndedAt: lsT0.Add(time.Second)}, checkoutEvents()[:4]...)
			},
			args: []string{"checkout", "short"}, code: ExitDiffFound, golden: "fewer-steps",
		},
		{
			name: "stop markers",
			seed: func(t *testing.T, dir string) {
				seedCheckout(t, dir, 0)
				events := checkoutEvents()
				failed := "overloaded_error: Overloaded"
				events[8].Status, events[8].Error = "error", &failed
				seedSession(t, dir, cassette.Session{ID: "failed", Mode: "exact", Status: "recording", CreatedAt: lsT0},
					&cassette.SessionEnd{Status: "completed", EndedAt: lsT0.Add(time.Minute)}, events...)
			},
			args: []string{"checkout", "failed"}, code: ExitDiffFound, golden: "stop",
		},
	}
}

// AC-20, AC-27: the text output of diff (spec §8.4), from seeded sessions so
// that every line is checked against a golden file.
func TestDiffText(t *testing.T) {
	for _, tc := range diffCases() {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.seed(t, dir)
			r := runDiffCmd(dir, tc.args...)
			if r.code != tc.code {
				t.Fatalf("exit %d, want %d\n%s", r.code, tc.code, r.stderr)
			}
			checkGolden(t, "diff/"+tc.golden+".txt", r.stdout.String())
		})
	}
}

// Cassettes are untrusted input: what diff prints from them cannot drive the
// terminal.
func TestDiffEscapesUntrustedText(t *testing.T) {
	dir := t.TempDir()
	seedCheckout(t, dir, 0)
	events := checkoutEvents()
	events[5] = toolUse(6, 5, "search\x1b[2J", "{\"customer_id\":\"c_812\",\"status\":\"\u202eopen\"}")
	seedSession(t, dir, cassette.Session{ID: "evil", Mode: "exact", Status: "recording", CreatedAt: lsT0}, nil, events...)
	r := runDiffCmd(dir, "checkout", "evil")
	if r.code != ExitDiffFound {
		t.Fatalf("exit %d, want %d\n%s", r.code, ExitDiffFound, r.stderr)
	}
	out := r.stdout.String()
	if strings.ContainsAny(out, "\x1b\u202e") || !strings.Contains(out, `search\x1b[2J`) || !strings.Contains(out, `"\u202eopen"`) {
		t.Errorf("output is not escaped:\n%s", out)
	}
}

// AC-19 end to end: a recording and its exact replay have identical
// trajectories.
func TestDiffOfAnExactReplay(t *testing.T) {
	dir := t.TempDir()
	recordMCPForReplay(t, dir)
	if r := runRunCmd(t, dir, "s", "--listen", "127.0.0.1:0"); r.code != ExitOK {
		t.Fatalf("run: exit %d\n%s", r.code, r.stderr)
	}
	r := runDiffCmd(dir, "s", "s.exact.1")
	if r.code != ExitOK {
		t.Fatalf("diff: exit %d, want 0\n%s%s", r.code, r.stdout, r.stderr)
	}
	if out := r.stdout.String(); !strings.Contains(out, "✓ trajectories identical (1 step, 4 tool calls)\n") {
		t.Errorf("output does not say the trajectories are identical:\n%s", out)
	}
}

// toolUseUpstream is a fake Anthropic API whose model asks for the weather
// in Rome where the recorded one asked for Paris (fakeAnthropic).
func toolUseUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Model string }
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/v1/messages" || json.Unmarshal(body, &req) != nil {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		resp, _ := json.Marshal(map[string]any{
			"type": "message", "role": "assistant", "model": req.Model,
			"content":     []map[string]any{{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": map[string]string{"city": "Rome"}}},
			"stop_reason": "tool_use", "usage": map[string]int{"input_tokens": 90, "output_tokens": 9},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(resp)
	}))
	t.Cleanup(up.Close)
	return up
}

// AC-20 end to end: a swap-model replay whose model calls the tool with
// other arguments, and whose agent then misses the cassette.
func TestDiffOfADivergingSwapModelReplay(t *testing.T) {
	dir := t.TempDir()
	recordMCPForReplay(t, dir)
	up := toolUseUpstream(t)
	t.Setenv(mcpKeyEnv, "Z9")
	t.Setenv(agentIgnoreTermEnv, "1") // so every run makes the same calls
	r := runRunCmd(t, dir, "s", "--model", "claude-haiku-4-5", "--anthropic-upstream", up.URL, "--listen", "127.0.0.1:0")
	if r.code != ExitDiverged {
		t.Fatalf("run: exit %d, want %d\n%s", r.code, ExitDiverged, r.stderr)
	}

	r = runDiffCmd(dir, "s", "s.swap-model.1")
	if r.code != ExitDiffFound {
		t.Fatalf("diff: exit %d, want %d\n%s%s", r.code, ExitDiffFound, r.stdout, r.stderr)
	}
	out := r.stdout.String()
	for _, want := range []string{
		"retrace diff s  s.swap-model.1\n",
		"A: claude-sonnet-5-5",
		"B: claude-haiku-4-5",
		"(stopped: cassette miss)\n",
		"✗ first divergence at step 1, event tool_use\n",
		`    A  get_weather  {"city":"Paris"}` + "\n",
		`    B  get_weather  {"city":"Rome"}` + "\n",
		`       - $.city: "Paris" → "Rome"` + "\n",
		"  B stopped at step 1: cassette miss on mcp:orders\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// spec §8.4, §9.7: every error of diff exits 2, since 1 means a divergence.
func TestDiffErrors(t *testing.T) {
	dir := t.TempDir()
	seedCheckout(t, dir, 0)
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
		stderr string
	}{
		{"no ids", nil, "exactly two session ids"},
		{"one id", []string{"checkout"}, "exactly two session ids"},
		{"three ids", []string{"checkout", "checkout", "checkout"}, "exactly two session ids"},
		{"invalid id", []string{"checkout", "../x"}, "session id"},
		{"unknown flag", []string{"checkout", "checkout", "--json"}, "flag provided but not defined"},
		{"invalid format", []string{"checkout", "checkout", "--format", "yaml"}, `--format "yaml": want text or json`},
		{"missing session", []string{"checkout", "nope"}, "no such session"},
		{"no session row", []string{"empty", "checkout"}, "cannot read the session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runDiffCmd(dir, tc.args...)
			if r.code != ExitUsage || !strings.Contains(r.stderr.String(), tc.stderr) {
				t.Errorf("exit code %d, stderr %q; want %d and %q", r.code, r.stderr, ExitUsage, tc.stderr)
			}
		})
	}
}

func TestDiffHelp(t *testing.T) {
	r := runDiffCmd(t.TempDir(), "--help")
	if r.code != ExitOK || !strings.Contains(r.stdout.String(), "Usage: retrace diff <id-a> <id-b> [--format text|json] [--text]") {
		t.Errorf("exit %d, stdout %q; want 0 and the usage", r.code, r.stdout)
	}
}

// What the divergence point shows for each kind of event, and for a side
// that has none (spec §8.3 step 3).
func TestDiffPointEvent(t *testing.T) {
	call := toolCall(3, "search_orders", `{ "id": 1 }`)
	result := toolResult(4, 3, "list_orders", `{"orders": []}`)
	result.Status = "error"
	noTool := toolUse(2, 1, "x", `not json`)
	noTool.Tool = nil
	model := modelResponse(1, "m", "end_turn", "")
	model.Model = nil
	miss := missEvent(5, "anthropic")
	for _, tc := range []struct {
		name string
		p    diff.Point
		e    *cassette.Event
		want string
	}{
		{"tool_call", diff.Point{Kind: "tool_call"}, &call, `mcp:shop search_orders  {"id":1}`},
		{"tool_result", diff.Point{Kind: "tool_result"}, &result, `mcp:shop list_orders  {"orders":[]}  error`},
		{"no tool, not JSON", diff.Point{Kind: "tool_use"}, &noTool, `(no tool)  not json`},
		{"divergence", diff.Point{Kind: "divergence"}, &miss, "cassette miss on anthropic"},
		{"unreadable divergence", diff.Point{Kind: "divergence"}, &cassette.Event{Kind: "divergence", Result: json.RawMessage(`[]`)}, "cassette miss"},
		{"model unknown", diff.Point{Kind: "model_response"}, &model, "model (unknown)"},
		{"no event", diff.Point{Step: 4, Kind: "tool_call"}, nil, "(none: no matching tool_call in step 4)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pointEvent(tc.p, tc.e, "A"); got != tc.want {
				t.Errorf("pointEvent = %q, want %q", got, tc.want)
			}
		})
	}
}

// spec §6.4, §8.4: a cut diff ends with a line that says more paths differ.
func TestDiffPointSaysMorePathsDiffer(t *testing.T) {
	a := toolUse(2, 1, "search_orders", `{"a":1}`)
	b := toolUse(2, 1, "search_orders", `{"a":2}`)
	var out strings.Builder
	writePoint(&out, diff.Point{
		Step: 1, Kind: "tool_use", A: &a, B: &b, Field: diff.FieldArguments,
		Diff: divergence.Diff{Entries: []divergence.Entry{{Path: "$.a", Expected: []byte("1"), Received: []byte("2")}}, Truncated: true, MorePaths: true},
	})
	if got := out.String(); !strings.HasSuffix(got, "       - $.a: 1 → 2\n       … more paths differ\n") {
		t.Errorf("point:\n%s\nwant it to end with the entry and the more-paths line", got)
	}
}

func TestDiffStopText(t *testing.T) {
	for _, tc := range []struct {
		stop *diff.Stop
		want string
	}{
		{nil, "(none: no model response)"},
		{&diff.Stop{Failed: true}, "stop: error"},
		{&diff.Stop{}, "stop: (no stop_reason)"},
		{&diff.Stop{StopReason: "max_tokens\x1b"}, `stop: max_tokens\x1b`},
	} {
		if got := stopText(tc.stop); got != tc.want {
			t.Errorf("stopText(%+v) = %q, want %q", tc.stop, got, tc.want)
		}
	}
}
