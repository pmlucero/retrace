package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/diff"
)

func toolPoint(step int, kind, tool string) *diff.Point {
	p := &diff.Point{Step: step, Kind: kind}
	if tool != "" {
		p.B = &cassette.Event{Tool: &tool}
	}
	return p
}

func replaysOf(outcome string, p *diff.Point, n int) []replayOutcome {
	out := make([]replayOutcome, n)
	for i := range out {
		out[i] = replayOutcome{outcome: outcome, point: p}
	}
	return out
}

// AC-40: a session's line counts its outcomes and groups its divergence
// points by step, kind and tool; the summary groups them by kind and tool,
// counting sessions.
func TestCheckReportGroupsDivergencePoints(t *testing.T) {
	cat := func(parts ...[]replayOutcome) (all []replayOutcome) {
		for _, p := range parts {
			all = append(all, p...)
		}
		return all
	}
	r := checkReport{mode: "exact", repeat: 5, out: "out", sessions: []checkSession{
		{id: "a", replays: cat(replaysOf(outcomeIdentical, nil, 5))},
		{id: "b", replays: cat(replaysOf(outcomeDiverged, toolPoint(3, "tool_use", "search_orders"), 3), replaysOf(outcomeIdentical, nil, 2))},
		{id: "c", replays: cat(
			replaysOf(outcomeDiverged, toolPoint(2, "tool_call", "lookup"), 1),
			replaysOf(outcomeDiverged, toolPoint(1, "tool_use", "get_invoice"), 4))},
		// Different steps, same kind and tool: one group in the summary.
		{id: "d", replays: cat(replaysOf(outcomeDiverged, toolPoint(7, "tool_use", "search_orders"), 1))},
		// No tool: a cassette miss and a stop marker.
		{id: "e", replays: cat(replaysOf(outcomeDiverged, toolPoint(0, "divergence", ""), 1),
			replaysOf(outcomeDiverged, &diff.Point{Step: 4, Kind: diff.KindStop}, 1),
			replaysOf(outcomeError, nil, 1), replaysOf(outcomeSkipped, nil, 1), replaysOf(outcomeIdentical, nil, 1))},
		// A hostile tool name cannot drive the terminal.
		{id: "f", replays: cat(replaysOf(outcomeDiverged, toolPoint(1, "tool_use", "x\x1b[2J"), 1))},
	}}
	var out strings.Builder
	if err := r.writeText(&out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"  a   5 identical\n",
		"  b   3 diverged, 2 identical  step 3 tool_use search_orders (3)\n",
		"  c   5 diverged  step 1 tool_use get_invoice (4), step 2 tool_call lookup (1)\n",
		"  e   2 diverged, 1 error, 1 skipped, 1 identical  step 0 divergence (1), step 4 stop (1)\n",
		`step 1 tool_use x\x1b[2J (1)`,
		"diverged at  search_orders tool_use (2 sessions), divergence (1 session), stop (1 session), lookup tool_call (1 session), get_invoice tool_use (1 session), ",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\x1b") {
		t.Errorf("output holds a raw escape:\n%q", got)
	}
}

// AC-40: without --text, text that differs is said so as diff does, with the
// steps of all the replays of a session; with --text, it is a divergence.
func TestCheckText(t *testing.T) {
	dir := t.TempDir()
	recordHelperSession(t, dir, "a", fakeAnthropic(t).URL)
	up := swapChangingUpstream(t)
	flags := []string{"--model", "claude-haiku-4-5", "--anthropic-upstream", up.URL}

	r := runCheckCmd(t, dir, afterDash(append([]string{"--out", filepath.Join(t.TempDir(), "o1")}, flags...), agentCommand())...)
	if !strings.Contains(r.stdout.String(), "text differs in step 1 (not compared; use --text)") {
		t.Errorf("without --text, stdout lacks the note:\n%s%s", r.stdout, r.stderr)
	}

	r = runCheckCmd(t, dir, afterDash(append([]string{"--text", "--out", filepath.Join(t.TempDir(), "o2")}, flags...), agentCommand())...)
	if r.code != ExitDiverged || strings.Contains(r.stdout.String(), "not compared") ||
		!strings.Contains(r.stdout.String(), "step 1 model_response (1)") {
		t.Errorf("with --text: exit %d, want %d and a model_response divergence\n%s%s", r.code, ExitDiverged, r.stdout, r.stderr)
	}
}

// AC-47: the --mcp overrides reach only the sessions that have the server,
// and a name no session has is a usage error.
func TestCheckMCPOverrides(t *testing.T) {
	has := func(names ...string) cassette.Session {
		var servers []mcpServer
		for _, n := range names {
			servers = append(servers, mcpServer{Name: n, URL: "http://x"})
		}
		b, _ := json.Marshal(servers)
		return cassette.Session{MCPServers: b}
	}
	overrides := []mcpServer{{Name: "orders", URL: "http://o"}, {Name: "weather", URL: "http://w"}}

	got := sessionOptions(runOptions{mcp: overrides}, has("weather", "billing"))
	if len(got.mcp) != 1 || got.mcp[0].Name != "weather" {
		t.Errorf("overrides for a session with weather and billing = %+v, want weather only", got.mcp)
	}
	if got := sessionOptions(runOptions{mcp: overrides}, has()); len(got.mcp) != 0 {
		t.Errorf("overrides for a session without servers = %+v, want none", got.mcp)
	}
	corpus := []cassette.Session{has("billing"), has("weather")}
	if err := checkMCPNames(corpus, overrides[1:]); err != nil {
		t.Errorf("weather is in the corpus: %v", err)
	}
	if err := checkMCPNames(corpus, overrides[:1]); err == nil || !strings.Contains(err.Error(), `"orders"`) {
		t.Errorf("orders is in no session: error %v", err)
	}
}

// AC-47, AC-49: --tools live with --mcp end to end, and its usage errors,
// which create nothing. The agent's stdout goes to stderr.
func TestCheckLiveTools(t *testing.T) {
	t.Setenv(mcpFollowUpEnv, "1")
	t.Setenv(agentIgnoreTermEnv, "1")
	dir := t.TempDir()
	recordMCPForReplay(t, dir) // its servers are closed now
	good := t.TempDir()
	mcpFlags := liveServers(t, lookupTool())

	out := filepath.Join(good, "out")
	r := runCheckCmd(t, dir, append([]string{"--tools", "live", "--recorded-command", "--out", out}, mcpFlags...)...)
	if r.code != ExitOK || !strings.Contains(r.stdout.String(), "s   1 identical") {
		t.Fatalf("exit %d, want %d and an identical session\n%s%s", r.code, ExitOK, r.stdout, r.stderr)
	}
	// AC-49: stdout holds only the report; the agent's JSON is on stderr.
	if !strings.HasPrefix(r.stdout.String(), "retrace check  live-tools") || strings.Contains(r.stdout.String(), `"Method"`) ||
		strings.Contains(r.stdout.String(), "tools/call") || !strings.Contains(r.stderr.String(), "tools/call") {
		t.Errorf("agent output mixed into stdout:\n%s", r.stdout)
	}

	c := openCassette(t, cassette.Path(dir, "s"))
	redacted := json.RawMessage(`[{"name":"orders","url":"https://REDACTED@orders.example.com/mcp\u001b[2J"}]`)
	if err := c.Write(context.Background(), func(tx *cassette.Tx) error {
		return tx.SetMCPServers(context.Background(), redacted)
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"unknown server", append([]string{"--tools", "live"}, "--mcp", "billing=http://127.0.0.1:1"), `no session in the corpus has an MCP server named "billing"`},
		{"redacted URL", []string{"--tools", "live"}, `session "s": MCP server orders: the recorded URL`},
		{"frozen", []string{"--mcp", "orders=http://127.0.0.1:1"}, "--mcp is only used with --tools live"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "out")
			r := runCheckCmd(t, dir, append([]string{"--recorded-command", "--out", out}, tc.args...)...)
			if r.code != ExitUsage || !strings.Contains(r.stderr.String(), tc.want) || strings.Contains(r.stderr.String(), "\x1b") {
				t.Errorf("exit %d, stderr %q; want %d and %q, escaped", r.code, r.stderr, ExitUsage, tc.want)
			}
			assertNotCreated(t, out)
		})
	}
}

// AC-49: an explicit id replays a failed session and a replay, which the
// default corpus leaves out.
func TestCheckExplicitIDs(t *testing.T) {
	dir := t.TempDir()
	up := fakeAnthropic(t)
	recordHelperSession(t, dir, "f", up.URL)
	recordHelperSession(t, dir, "p", up.URL)
	if r := runRunCmd(t, dir, afterDash([]string{"p", "--as", "p.old", "--listen", "127.0.0.1:0"}, agentCommand())...); r.code != ExitOK {
		t.Fatalf("run: exit %d\n%s", r.code, r.stderr)
	}
	c := openCassette(t, cassette.Path(dir, "f"))
	if err := c.Write(context.Background(), func(tx *cassette.Tx) error {
		return tx.EndSession(context.Background(), cassette.SessionEnd{Status: "failed", EndedAt: time.Now()})
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "out")
	r := runCheckCmd(t, dir, afterDash([]string{"--out", out, "f", "p.old"}, agentCommand())...)
	if r.code != ExitOK {
		t.Fatalf("exit %d\n%s", r.code, r.stderr)
	}
	for _, id := range []string{"f.exact.1", "p.old.exact.1"} {
		if !strings.Contains(r.stderr.String(), id+"  identical") {
			t.Errorf("stderr lacks the replay %s:\n%s", id, r.stderr)
		}
	}
	if !strings.Contains(r.stdout.String(), "  f       1 identical") || !strings.Contains(r.stdout.String(), "  p.old   1 identical") {
		t.Errorf("stdout lacks the explicit sessions:\n%s", r.stdout)
	}
}
