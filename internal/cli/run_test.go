package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pmlucero/retrace/internal/cassette"
)

// The replay agent sends RETRACE_TEST_REPLAY_N identical requests asking
// RETRACE_TEST_REPLAY_PROMPT, streamed, and prints each status and body: its
// stdout depends only on the answers it gets.
const (
	replayPromptEnv = "RETRACE_TEST_REPLAY_PROMPT"
	replayCountEnv  = "RETRACE_TEST_REPLAY_N"
)

func replayAgentCommand() []string {
	return []string{os.Args[0], "-test.run=^TestHelperReplayAgent$"}
}

// TestHelperReplayAgent is the fake agent of the run tests. It only runs as
// their subprocess.
func TestHelperReplayAgent(t *testing.T) {
	prompt := os.Getenv(replayPromptEnv)
	if prompt == "" {
		t.Skip("subprocess of the run tests")
	}
	n, _ := strconv.Atoi(os.Getenv(replayCountEnv))
	body := fmt.Sprintf(`{"model":"claude-sonnet-5-5","max_tokens":16,"stream":true,"metadata":{"user_id":"run-%s"},`+
		`"messages":[{"role":"user","content":%q}]}`, os.Getenv("RETRACE_MODE"), prompt)
	for range max(n, 1) {
		// The URL is the proxy that retrace injected: calling it is the point.
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, //nolint:gosec // G704
			os.Getenv("ANTHROPIC_BASE_URL")+"/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: see above
		if err != nil {
			fmt.Fprintln(os.Stderr, "agent request failed:", err)
			os.Exit(99)
		}
		data, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		fmt.Printf("%d %s\n", resp.StatusCode, data)
	}
	os.Exit(0)
}

// fakeStreamingAnthropic streams a different answer to each request, so a
// replay that serves them out of order shows in the agent's output.
func fakeStreamingAnthropic(t *testing.T) *httptest.Server {
	t.Helper()
	var calls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-sonnet-5-5\",\"usage\":{\"input_tokens\":10}}}\n\n")
		w.(http.Flusher).Flush()
		fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"answer %d\"}}\n\n", n)
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	t.Cleanup(up.Close)
	return up
}

// recordForReplay records session id with the replay agent sending n
// requests, then shuts the upstream down. It returns the agent's stdout.
func recordForReplay(t *testing.T, dir, id string, n int) string {
	t.Helper()
	t.Setenv(replayPromptEnv, "Weather in Paris?")
	t.Setenv(replayCountEnv, strconv.Itoa(n))
	up := fakeStreamingAnthropic(t)
	r := runRecordCmd(t, dir, append([]string{"--session", id, "--listen", "127.0.0.1:0",
		"--anthropic-upstream", up.URL, "--redact", "$.max_tokens", "--"}, replayAgentCommand()...)...)
	if r.code != ExitOK {
		t.Fatalf("record: exit %d\n%s", r.code, r.stderr)
	}
	up.Close() // AC-08: nothing upstream is reachable during the replay
	return r.stdout.String()
}

func runRunCmd(t *testing.T, dir string, args ...string) recordRun {
	t.Helper()
	r := recordRun{stdout: &syncBuffer{}, stderr: &syncBuffer{}}
	all := append([]string{"--dir", dir, "run"}, args...)
	r.code = run(context.Background(), all, nil, r.stdout, r.stderr, fakeEnv(nil), commands)
	return r
}

func sessionOf(t *testing.T, dir, id string) (cassette.Session, []cassette.Interaction) {
	t.Helper()
	c := openCassette(t, cassette.Path(dir, id))
	s, err := c.Session(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ins, err := c.Interactions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s, ins
}

// AC-08, AC-12
func TestRunReplaysARecordingOffline(t *testing.T) {
	dir := t.TempDir()
	want := recordForReplay(t, dir, "checkout", 2)
	if !strings.Contains(want, "answer 1") || !strings.Contains(want, "answer 2") {
		t.Fatalf("recorded agent output %q, want two different answers", want)
	}

	r := runRunCmd(t, dir, "checkout", "--listen", "127.0.0.1:0")
	if r.code != ExitOK {
		t.Fatalf("run: exit %d\n%s", r.code, r.stderr)
	}
	// AC-12: the identical requests got the recorded answers in order.
	if got := r.stdout.String(); got != want {
		t.Errorf("replayed agent output:\n%s\nwant the recorded one:\n%s", got, want)
	}
	if strings.Contains(r.stderr.String(), "never replayed") {
		t.Errorf("unexpected unconsumed warning:\n%s", r.stderr)
	}

	s, ins := sessionOf(t, dir, "checkout.exact.1")
	src, _ := sessionOf(t, dir, "checkout")
	if s.Mode != "exact" || s.Status != "completed" || s.ParentSessionID == nil || *s.ParentSessionID != "checkout" ||
		!slices.Equal(s.Command, src.Command) || s.Cwd != src.Cwd || string(s.Redaction) != string(src.Redaction) {
		t.Errorf("replay session = %+v, want exact, completed, from checkout with its command, cwd and redaction", s)
	}
	if len(ins) != 2 || ins[0].Provenance != "replayed" || ins[0].SourceInteractionID == nil || *ins[0].SourceInteractionID != 1 {
		t.Errorf("replayed interactions = %+v, want two, replayed from the source's", ins)
	}

	// The next replay gets the next id.
	if r := runRunCmd(t, dir, "checkout", "--listen", "127.0.0.1:0"); r.code != ExitOK {
		t.Fatalf("second run: exit %d\n%s", r.code, r.stderr)
	}
	if _, err := os.Stat(cassette.Path(dir, "checkout.exact.2")); err != nil {
		t.Errorf("second replay: %v", err)
	}
}

// AC-28
func TestRunEndsDivergedOnACassetteMiss(t *testing.T) {
	dir := t.TempDir()
	recordForReplay(t, dir, "s", 1)

	t.Setenv(replayPromptEnv, "Weather in Rome?")
	r := runRunCmd(t, dir, "s", "--as", "rome", "--listen", "127.0.0.1:0")
	if r.code != ExitDiverged {
		t.Fatalf("exit %d, want %d\n%s", r.code, ExitDiverged, r.stderr)
	}
	if out := r.stdout.String(); !strings.HasPrefix(out, "422 ") || !strings.Contains(out, "retrace_cassette_miss") {
		t.Errorf("agent got %q, want a 422 cassette miss", out)
	}
	s, _ := sessionOf(t, dir, "rome")
	if s.Status != "diverged" || s.AgentExitCode == nil || *s.AgentExitCode != 0 {
		t.Errorf("session status %q, agent exit code %v; want diverged and 0", s.Status, s.AgentExitCode)
	}
	events, err := openCassette(t, cassette.Path(dir, "rome")).Events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != "divergence" {
		t.Errorf("events = %+v, want one divergence", events)
	}
	// The recorded interaction was never served.
	if !strings.Contains(r.stderr.String(), "never replayed") || !strings.Contains(r.stderr.String(), "count=1") {
		t.Errorf("stderr lacks the unconsumed warning:\n%s", r.stderr)
	}
}

// spec §4.4: unconsumed interactions are a warning, not an error.
func TestRunWarnsAboutUnconsumedInteractions(t *testing.T) {
	dir := t.TempDir()
	recordForReplay(t, dir, "s", 3)

	t.Setenv(replayCountEnv, "1")
	r := runRunCmd(t, dir, "s", "--listen", "127.0.0.1:0")
	if r.code != ExitOK {
		t.Fatalf("exit %d\n%s", r.code, r.stderr)
	}
	if e := r.stderr.String(); !strings.Contains(e, "never replayed") || !strings.Contains(e, "count=2") || !strings.Contains(e, "first=\"[2 3]\"") {
		t.Errorf("stderr lacks the unconsumed warning with count and ids:\n%s", e)
	}
}

func TestRunOverridesTheCommand(t *testing.T) {
	dir := t.TempDir()
	recordForReplay(t, dir, "s", 1)

	cmd := append(replayAgentCommand(), "-test.count=1")
	r := runRunCmd(t, dir, append([]string{"s", "--listen", "127.0.0.1:0", "--as", "mine", "--realtime", "--"}, cmd...)...)
	if r.code != ExitOK {
		t.Fatalf("exit %d\n%s", r.code, r.stderr)
	}
	wd, _ := os.Getwd()
	if s, _ := sessionOf(t, dir, "mine"); !slices.Equal(s.Command, cmd) || s.Cwd != wd {
		t.Errorf("session command %q in %q, want %q in %q", s.Command, s.Cwd, cmd, wd)
	}
	// AC-30: an existing --as id is not replaced.
	r = runRunCmd(t, dir, "s", "--listen", "127.0.0.1:0", "--as", "mine")
	if r.code != ExitError || !strings.Contains(r.stderr.String(), "already exists") {
		t.Errorf("run --as an existing id: exit %d\n%s", r.code, r.stderr)
	}
}

// AC-30: a source without a command is refused before any cassette exists.
func TestRunRefusesASourceWithoutACommand(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c, err := cassette.Create(ctx, cassette.Path(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	err = c.Write(ctx, func(tx *cassette.Tx) error {
		return tx.InsertSession(ctx, cassette.Session{ID: "s", Mode: "record", Status: "completed", OnMiss: "strict",
			Redaction: json.RawMessage(`{"headers":[],"body":[]}`)})
	})
	if cerr := c.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}

	r := runRunCmd(t, dir, "s", "--listen", "127.0.0.1:0")
	if r.code != ExitUsage || !strings.Contains(r.stderr.String(), "no recorded command") {
		t.Errorf("exit %d, stderr %q; want a usage error", r.code, r.stderr)
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "*.cassette")); len(files) != 1 {
		t.Errorf("cassettes = %v, want only the source", files)
	}
}

// A busy port fails the run before the replay's cassette is created.
func TestRunBusyPortLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	recordForReplay(t, dir, "s", 1)
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	r := runRunCmd(t, dir, "s", "--listen", ln.Addr().String())
	if r.code != ExitError || !strings.Contains(r.stderr.String(), "cannot start the proxy") {
		t.Errorf("exit %d, stderr %q; want %d", r.code, r.stderr, ExitError)
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "*.cassette")); len(files) != 1 {
		t.Errorf("cassettes = %v, want only the source", files)
	}
}

// AC-30
func TestRunMissingSource(t *testing.T) {
	dir := t.TempDir()
	r := runRunCmd(t, dir, "nope", "--listen", "127.0.0.1:0", "--", "true")
	if r.code != ExitError || !strings.Contains(r.stderr.String(), "no such session") {
		t.Errorf("exit %d, stderr %q; want %d and no such session", r.code, r.stderr, ExitError)
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "*")); len(files) != 0 {
		t.Errorf("run created %v", files)
	}
}

// AC-16, AC-17 for the flags; swap-model and live-tools come in M5.
func TestRunUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no id", []string{"--realtime"}, "exactly one session id"},
		{"two ids", []string{"a", "b"}, "exactly one session id"},
		{"invalid id", []string{"../x"}, "invalid session id"},
		{"invalid --as", []string{"s", "--as", ".x"}, "invalid session id"},
		{"empty command", []string{"s", "--"}, "no agent command"},
		{"swap-model", []string{"s", "--model", "m"}, "not implemented in this version"},
		{"live-tools", []string{"s", "--tools", "live"}, "not implemented in this version"},
		{"model and live tools", []string{"s", "--model", "m", "--tools", "live"}, "use record instead"},
		{"unknown tools", []string{"s", "--tools", "warm"}, "want frozen or live"},
		{"reserved strategy", []string{"s", "--on-miss", "nearest-match"}, "not implemented in this version"},
		{"unknown strategy", []string{"s", "--on-miss", "lenient"}, "want strict"},
		{"non-loopback listen", []string{"s", "--listen", "0.0.0.0:0"}, "add --listen-unsafe"},
		{"grace is M5", []string{"s", "--grace", "1s"}, "not defined"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := runRunCmd(t, t.TempDir(), tt.args...)
			if r.code != ExitUsage || !strings.Contains(r.stderr.String(), tt.want) {
				t.Errorf("exit %d, stderr %q; want %d and %q", r.code, r.stderr, ExitUsage, tt.want)
			}
		})
	}
}

func TestNextReplayID(t *testing.T) {
	dir := t.TempDir()
	if id, err := nextReplayID(dir, "s", "exact"); err != nil || id != "s.exact.1" {
		t.Errorf("first replay id %q, %v; want s.exact.1", id, err)
	}
	for _, f := range []string{"s.exact.1", "s.exact.7", "s.exact.x", "s.exact.+9", "s.exact.09", "s.exact.9223372036854775807", "s.exact.99999999999999999999", "s.swap-model.9"} {
		if err := os.WriteFile(cassette.Path(dir, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if id, err := nextReplayID(dir, "s", "exact"); err != nil || id != "s.exact.8" {
		t.Errorf("replay id %q, %v; want s.exact.8", id, err)
	}
	long := strings.Repeat("a", 125)
	if _, err := nextReplayID(dir, long, "exact"); err == nil || !strings.Contains(err.Error(), "--as") {
		t.Errorf("replay id of a 125-character id: %v, want an error asking for --as", err)
	}
}

func TestCreateReplayCassetteTakesTheNextFreeDefaultID(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(cassette.Path(dir, "s.exact.1"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	c, id, err := createReplayCassette(ctx, dir, "s.exact.1", false, cassette.Session{Mode: "exact", Status: "recording", OnMiss: "strict"})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if id != "s.exact.2" {
		t.Errorf("id %q, want s.exact.2", id)
	}
}
