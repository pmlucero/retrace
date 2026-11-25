package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func runCheckCmd(t *testing.T, dir string, args ...string) recordRun {
	t.Helper()
	r := recordRun{stdout: &syncBuffer{}, stderr: &syncBuffer{}}
	all := append([]string{"--dir", dir, "check"}, args...)
	r.code = run(context.Background(), all, nil, r.stdout, r.stderr, fakeEnv(nil), commands)
	return r
}

// recordHelperSession records session id with TestHelperAgent against up,
// which answers with a tool_use.
func recordHelperSession(t *testing.T, dir, id, upstream string) {
	t.Helper()
	t.Setenv(agentModeEnv, "run")
	r := runRecordCmd(t, dir, append([]string{"--session", id, "--listen", "127.0.0.1:0",
		"--anthropic-upstream", upstream, "--"}, agentCommand()...)...)
	if r.code != ExitOK {
		t.Fatalf("record %s: exit %d\n%s", id, r.code, r.stderr)
	}
}

// dirState returns the name and content of every file in dir.
func dirState(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]string{}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // G304: a test reads its own temp dir
		if err != nil {
			t.Fatal(err)
		}
		state[e.Name()] = string(b)
	}
	return state
}

func assertNotCreated(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Errorf("%s exists, want it not created", path)
	}
}

// AC-39: with no ids, check replays every completed recording and no replay;
// an exact check of an unchanged agent is identical and exits 0, writes to
// --out and leaves --dir unchanged.
func TestCheckExactCorpusIsIdentical(t *testing.T) {
	dir, out := t.TempDir(), filepath.Join(t.TempDir(), "out")
	up := fakeAnthropic(t)
	recordHelperSession(t, dir, "b", up.URL)
	recordHelperSession(t, dir, "a", up.URL)
	if r := runRunCmd(t, dir, afterDash([]string{"a", "--as", "a.old-replay", "--listen", "127.0.0.1:0"}, agentCommand())...); r.code != ExitOK {
		t.Fatalf("run: exit %d\n%s", r.code, r.stderr)
	}
	before := dirState(t, dir)

	r := runCheckCmd(t, dir, afterDash([]string{"--out", out, "--grace", "1s"}, agentCommand())...)
	if r.code != ExitOK {
		t.Fatalf("exit %d\n%s", r.code, r.stderr)
	}
	stdout := r.stdout.String()
	for _, want := range []string{"retrace check  exact  repeat 1  2 sessions", "a   1 identical", "b   1 identical",
		"sessions     2 identical", "replays      2 identical", "replays in   " + out} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "old-replay") {
		t.Errorf("a replay is in the default corpus:\n%s", stdout)
	}
	for _, id := range []string{"a.exact.1", "b.exact.1"} {
		if !strings.Contains(r.stderr.String(), id+"  identical") {
			t.Errorf("stderr lacks the progress line of %s:\n%s", id, r.stderr)
		}
		if _, err := os.Stat(filepath.Join(out, id+".cassette")); err != nil {
			t.Errorf("replay %s: %v", id, err)
		}
	}
	after := dirState(t, dir)
	if len(after) != len(before) {
		t.Errorf("--dir holds %d files, was %d", len(after), len(before))
	}
	for name, content := range before {
		if after[name] != content {
			t.Errorf("%s in --dir changed", name)
		}
	}
}

// AC-40, AC-44: an agent whose requests differ diverges, with the first
// divergence point, and exits 3.
func TestCheckDivergedAgent(t *testing.T) {
	dir, out := t.TempDir(), filepath.Join(t.TempDir(), "out")
	recordForReplay(t, dir, "s", 1)
	t.Setenv(replayPromptEnv, "Weather in Rome?")
	t.Setenv(agentIgnoreTermEnv, "1") // so it exits by itself after the miss

	r := runCheckCmd(t, dir, afterDash([]string{"--out", out, "--grace", "30s"}, replayAgentCommand())...)
	if r.code != ExitDiverged {
		t.Fatalf("exit %d, want %d\n%s", r.code, ExitDiverged, r.stderr)
	}
	for _, want := range []string{"s   1 diverged  step 0 divergence (1)", "sessions     1 diverged", "replays      1 diverged"} {
		if !strings.Contains(r.stdout.String(), want) {
			t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
		}
	}
	if !strings.Contains(r.stderr.String(), "s.exact.1  diverged") {
		t.Errorf("stderr lacks the progress line:\n%s", r.stderr)
	}
}

// swapChangingUpstream answers every even request with another tool input.
func swapChangingUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	var calls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		city := "Paris"
		if calls.Add(1)%2 == 0 {
			city = "Rome"
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"type":"message","role":"assistant","model":"claude-haiku-4-5",`+
			`"content":[{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":%q}}],`+
			`"stop_reason":"tool_use","usage":{"input_tokens":100,"output_tokens":10}}`, city)
	}))
	t.Cleanup(up.Close)
	return up
}

// AC-41: --model --repeat 3 against an API that answers differently runs 3
// replays per session and reports per-session counts.
func TestCheckSwapModelRepeat(t *testing.T) {
	dir, out := t.TempDir(), filepath.Join(t.TempDir(), "out")
	recordHelperSession(t, dir, "a", fakeAnthropic(t).URL)
	recordHelperSession(t, dir, "b", fakeAnthropic(t).URL)
	up := swapChangingUpstream(t) // calls 1-3 go to a, 4-6 to b

	r := runCheckCmd(t, dir, afterDash([]string{"--out", out, "--model", "claude-haiku-4-5", "--repeat", "3",
		"--anthropic-upstream", up.URL}, agentCommand())...)
	if r.code != ExitDiverged {
		t.Fatalf("exit %d, want %d\n%s", r.code, ExitDiverged, r.stderr)
	}
	for _, want := range []string{"retrace check  swap-model (claude-haiku-4-5)  repeat 3  2 sessions",
		"a   1 diverged, 2 identical  step 1 tool_use get_weather (1)", "b   2 diverged, 1 identical  step 1 tool_use get_weather (2)",
		"sessions     2 diverged", "replays      3 diverged, 3 identical", "cost (est.)  <$0.01"} {
		if !strings.Contains(r.stdout.String(), want) {
			t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
		}
	}
	for _, id := range []string{"a.swap-model.3", "b.swap-model.1"} {
		if _, err := os.Stat(filepath.Join(out, id+".cassette")); err != nil {
			t.Error(err)
		}
	}
}

// AC-44: identical trajectories whose agent exits differently are an error,
// which exits 1; an empty corpus exits 1.
func TestCheckErrorsExitOne(t *testing.T) {
	dir := t.TempDir()
	recordHelperSession(t, dir, "s", fakeAnthropic(t).URL)
	t.Setenv(agentExitEnv, "3")
	r := runCheckCmd(t, dir, afterDash([]string{"--out", filepath.Join(t.TempDir(), "out")}, agentCommand())...)
	if r.code != ExitError || !strings.Contains(r.stdout.String(), "s   1 error") {
		t.Errorf("exit %d, want %d with 1 error\n%s%s", r.code, ExitError, r.stdout, r.stderr)
	}

	empty := runCheckCmd(t, t.TempDir(), "--", "true")
	if empty.code != ExitError || !strings.Contains(empty.stderr.String(), "corpus is empty") {
		t.Errorf("empty corpus: exit %d\n%s", empty.code, empty.stderr)
	}
}

// AC-44: a divergence takes precedence over an error (spec §9.9.8).
func TestCheckExitCode(t *testing.T) {
	session := func(outcomes ...string) checkSession {
		var s checkSession
		for _, o := range outcomes {
			s.replays = append(s.replays, replayOutcome{outcome: o})
		}
		return s
	}
	for _, tt := range []struct {
		name     string
		sessions []checkSession
		want     int
	}{
		{"identical", []checkSession{session(outcomeIdentical)}, ExitOK},
		{"error", []checkSession{session(outcomeIdentical), session(outcomeError)}, ExitError},
		{"diverged and error", []checkSession{session(outcomeError), session(outcomeIdentical, outcomeDiverged)}, ExitDiverged},
	} {
		if got := (checkReport{sessions: tt.sessions}).exitCode(); got != tt.want {
			t.Errorf("%s: exit %d, want %d", tt.name, got, tt.want)
		}
	}
}

// AC-42: --recorded-command --cwd runs each recorded argv in the directory;
// without a command, check prints each session's argv and cwd, escaped, and
// starts nothing.
func TestCheckCommandChoice(t *testing.T) {
	t.Setenv(cwdAgentEnv, "1")
	dir, override := t.TempDir(), t.TempDir()
	missing := filepath.Join(t.TempDir(), "gone")
	writeSource(t, dir, "s", append(cwdAgentCommand(), "\x1b[2J"), missing)
	out := filepath.Join(t.TempDir(), "out")

	r := runCheckCmd(t, dir, "--out", out, "--recorded-command", "--cwd", override)
	if r.code != ExitOK {
		t.Fatalf("exit %d\n%s", r.code, r.stderr)
	}
	want, _ := filepath.EvalSymlinks(override)
	if !strings.Contains(r.stderr.String(), want) {
		t.Errorf("agent did not run in %s:\n%s", want, r.stderr)
	}

	// Neither form: a usage error that lists the sessions, escaped.
	out2 := filepath.Join(t.TempDir(), "out2")
	r = runCheckCmd(t, dir, "--out", out2)
	if r.code != ExitUsage || !strings.Contains(r.stderr.String(), `\x1b[2J`) || strings.Contains(r.stderr.String(), "\x1b") ||
		!strings.Contains(r.stderr.String(), strconv.Quote(missing)) {
		t.Errorf("exit %d, want a usage error with the escaped command\n%s", r.code, r.stderr)
	}
	assertNotCreated(t, out2)

	// One session with a recorded cwd that is gone: no replay starts at all.
	writeSource(t, dir, "a", cwdAgentCommand(), t.TempDir())
	out3 := filepath.Join(t.TempDir(), "out3")
	r = runCheckCmd(t, dir, "--out", out3, "--recorded-command")
	if r.code != ExitUsage || !strings.Contains(r.stderr.String(), `session "s"`) {
		t.Errorf("exit %d, want a usage error naming session s\n%s", r.code, r.stderr)
	}
	assertNotCreated(t, out3)

	// More than ten sessions: the usage error lists ten, then how many more.
	many := t.TempDir()
	for i := range 12 {
		writeSource(t, many, fmt.Sprintf("m%02d", i), cwdAgentCommand(), "/")
	}
	if r = runCheckCmd(t, many); r.code != ExitUsage || !strings.Contains(r.stderr.String(), "and 2 more") {
		t.Errorf("exit %d, want a usage error ending in 'and 2 more'\n%s", r.code, r.stderr)
	}
}

// AC-46: corpus and flag errors start no replay and create no --out.
func TestCheckCorpusAndFlagErrors(t *testing.T) {
	dir := t.TempDir()
	writeSource(t, dir, "s", cwdAgentCommand(), "/")
	writeSource(t, dir, strings.Repeat("x", 128), cwdAgentCommand(), "/")
	held := t.TempDir()
	writeSource(t, held, "already", cwdAgentCommand(), "/")

	for _, tt := range []struct {
		name string
		args []string
		want int
	}{
		{"duplicate id", []string{"s", "s"}, ExitUsage},
		{"repeat zero", []string{"s", "--repeat", "0"}, ExitUsage},
		{"missing id", []string{"nope"}, ExitError},
		{"replay id too long", []string{strings.Repeat("x", 128)}, ExitUsage},
		{"out holds a cassette", []string{"s", "--out", held}, ExitUsage},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "out")
			args := append([]string{"--out", out}, tt.args...)
			if tt.name == "out holds a cassette" {
				args = tt.args
			}
			r := runCheckCmd(t, dir, append(args, "--", os.Args[0], "-test.run=^$")...)
			if r.code != tt.want {
				t.Errorf("exit %d, want %d\n%s", r.code, tt.want, r.stderr)
			}
			assertNotCreated(t, out)
			if r.stdout.String() != "" {
				t.Errorf("stdout %q, want nothing: no replay ran", r.stdout)
			}
		})
	}
}

// afterDash is flags, then '--' and the agent command.
func afterDash(flags, cmd []string) []string {
	return append(append(flags, "--"), cmd...)
}

// AC-46: the flag errors of spec 9.9 are usage errors that create nothing.
func TestCheckFlagErrors(t *testing.T) {
	dir := t.TempDir()
	writeSource(t, dir, "s", cwdAgentCommand(), "/")
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		args []string
	}{
		{"tools", []string{"--tools", "both"}},
		{"model with live tools", []string{"--model", "m", "--tools", "live"}},
		{"empty model", []string{"--model", ""}},
		{"upstream without model", []string{"--anthropic-upstream", "http://x"}},
		{"on-miss", []string{"--on-miss", "fallthrough"}},
		{"grace", []string{"--grace", "-1s"}},
		{"cwd is a file", []string{"--cwd", file}},
		{"both command forms", []string{"--recorded-command"}},
		{"redact-header pattern", []string{"--redact-header", "*x"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "out")
			r := runCheckCmd(t, dir, afterDash(append([]string{"--out", out}, tt.args...), []string{"true"})...)
			if r.code != ExitUsage {
				t.Errorf("exit %d, want %d\n%s", r.code, ExitUsage, r.stderr)
			}
			assertNotCreated(t, out)
		})
	}
}
