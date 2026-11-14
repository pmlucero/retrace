package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/pmlucero/retrace/internal/cassette"
)

// cwdAgentCommand is an agent that prints its working directory and exits
// (TestHelperCwdAgent): its output shows whether it ran, and where.
func cwdAgentCommand() []string {
	return []string{os.Args[0], "-test.run=^TestHelperCwdAgent$"}
}

// writeSource writes a completed session id with the given command and cwd,
// and no interactions, as a cassette from someone else could hold.
func writeSource(t *testing.T, dir, id string, command []string, cwd string) {
	t.Helper()
	ctx := context.Background()
	c, err := cassette.Create(ctx, cassette.Path(dir, id))
	if err != nil {
		t.Fatal(err)
	}
	err = c.Write(ctx, func(tx *cassette.Tx) error {
		return tx.InsertSession(ctx, cassette.Session{ID: id, Mode: "record", Status: "completed", OnMiss: "strict",
			Command: command, Cwd: cwd, Redaction: json.RawMessage(`{"headers":[],"body":[]}`)})
	})
	if cerr := c.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
}

// AC-33: without -- <cmd> or --recorded-command, run prints the recorded
// command, escaped, and runs nothing.
func TestRunNeedsAnExplicitCommand(t *testing.T) {
	t.Setenv(cwdAgentEnv, "1")
	// A hostile cassette: escape sequences and a bidi override that would
	// clear the screen and hide part of the command if printed raw.
	command := append(cwdAgentCommand(), "\x1b[2J", "safe\u202egnp.exe", "\u009b31m")
	cwd := "/tmp/x\x07"

	tests := []struct {
		name string
		args []string
	}{
		{"plain", []string{"s"}},
		// These checks come after the command one (spec §9.3).
		{"existing --as", []string{"s", "--as", "taken"}},
		{"unknown --mcp name", []string{"s", "--tools", "live", "--mcp", "nope=http://127.0.0.1:1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeSource(t, dir, "s", command, cwd)
			writeSource(t, dir, "taken", nil, "")

			r := runRunCmd(t, dir, append(tt.args, "--listen", "127.0.0.1:0")...)
			if r.code != ExitUsage {
				t.Fatalf("exit %d, want %d\n%s", r.code, ExitUsage, r.stderr)
			}
			stderr := r.stderr.String()
			for _, want := range []string{
				"--recorded-command", "'--'",
				strconv.Quote(os.Args[0]), `"\x1b[2J"`, `"safe\u202egnp.exe"`, `"\u009b31m"`, `"/tmp/x\a"`,
			} {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr lacks %q:\n%s", want, stderr)
				}
			}
			if strings.ContainsFunc(stderr, func(r rune) bool { return r != '\n' && unsafeRune(r) }) {
				t.Errorf("stderr holds an unescaped control character: %q", stderr)
			}
			if out := r.stdout.String(); out != "" {
				t.Errorf("the recorded command ran: %q", out)
			}
			assertDirHolds(t, dir, "s.cassette", "taken.cassette")
		})
	}
}

// AC-30, AC-33: a source without a recorded command asks for -- <cmd>, and
// --recorded-command cannot help it. No cassette is created.
func TestRunRefusesASourceWithoutACommand(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"no choice", []string{"s"}},                      // AC-33
		{"recorded", []string{"s", "--recorded-command"}}, // AC-30
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeSource(t, dir, "s", nil, "")

			r := runRunCmd(t, dir, append(tt.args, "--listen", "127.0.0.1:0")...)
			stderr := r.stderr.String()
			if r.code != ExitUsage || !strings.Contains(stderr, "no recorded command") || !strings.Contains(stderr, "'--'") {
				t.Errorf("exit %d, stderr %q; want a usage error that asks for '--'", r.code, stderr)
			}
			// In neither case: the flag was not given, or it was and cannot help.
			if strings.Contains(stderr, "--recorded-command") {
				t.Errorf("stderr suggests --recorded-command, which cannot help:\n%s", stderr)
			}
			assertDirHolds(t, dir, "s.cassette")
		})
	}
}

// AC-33: --recorded-command runs the recorded command in the recorded cwd.
func TestRunRecordedCommandRunsInTheRecordedCwd(t *testing.T) {
	dir := t.TempDir()
	cwd := t.TempDir()
	t.Setenv(cwdAgentEnv, "1")
	writeSource(t, dir, "s", cwdAgentCommand(), cwd)

	r := runRunCmd(t, dir, "s", "--recorded-command", "--listen", "127.0.0.1:0")
	if r.code != ExitOK {
		t.Fatalf("exit %d\n%s", r.code, r.stderr)
	}
	got := r.stdout.String()
	want, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if g, err := filepath.EvalSymlinks(got); err != nil || g != want {
		t.Errorf("agent ran in %q, want %q", got, want)
	}
	if s, _ := sessionOf(t, dir, "s.exact.1"); s.Cwd != cwd {
		t.Errorf("replay cwd %q, want %q", s.Cwd, cwd)
	}
}
