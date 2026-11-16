package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
)

// AC-35: a cassette whose events is a recursive view, which would hang a
// reader, is rejected before it is read. ls skips it with a warning, show and
// run fail, and none of them modifies the file.
func TestUnsafeCassetteIsRejected(t *testing.T) {
	dir := t.TempDir()
	seedSession(t, dir, cassette.Session{ID: "ok", Mode: "record", Status: "recording", CreatedAt: lsT0}, nil)
	seedSession(t, dir, cassette.Session{ID: "evil", Mode: "record", Status: "recording", CreatedAt: lsT0, Command: []string{"true"}}, nil)
	path := cassette.Path(dir, "evil")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		"DROP TABLE events",
		"CREATE VIEW events AS WITH RECURSIVE r(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM r) SELECT n AS seq FROM r",
	} {
		if _, err := db.ExecContext(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path) //nolint:gosec // G304: a file of this test
	if err != nil {
		t.Fatal(err)
	}

	r := within(t, "ls", func() recordRun { return runLsCmd(dir, "--json") })
	if r.code != ExitOK {
		t.Fatalf("ls: exit code %d, stderr:\n%s", r.code, r.stderr)
	}
	var listed []lsEntry
	if err := json.Unmarshal([]byte(r.stdout.String()), &listed); err != nil || len(listed) != 1 || listed[0].ID != "ok" {
		t.Errorf("ls listed %+v (%v), want only ok", listed, err)
	}
	if !strings.Contains(r.stderr.String(), "evil.cassette") {
		t.Errorf("ls does not warn about evil.cassette:\n%s", r.stderr)
	}

	// The cause, so that an unrelated failure does not pass.
	const cause = "safety checks"
	if r := within(t, "show", func() recordRun { return runShowCmd(dir, "evil") }); r.code != ExitError || !strings.Contains(r.stderr.String(), cause) {
		t.Errorf("show: exit code %d, want %d and %q; stderr:\n%s", r.code, ExitError, cause, r.stderr)
	}
	r = within(t, "run", func() recordRun { return runRunCmd(t, dir, "evil", "--listen", "127.0.0.1:0") })
	if r.code != ExitError || !strings.Contains(r.stderr.String(), cause) {
		t.Errorf("run: exit code %d, want %d and %q; stderr:\n%s", r.code, ExitError, cause, r.stderr)
	}

	after, err := os.ReadFile(path) //nolint:gosec // G304: a file of this test
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("a command modified the rejected cassette")
	}
}

// within runs cmd and fails the test if it does not return in time, so a
// command that reads the recursive view fails instead of hanging the suite.
func within(t *testing.T, name string, cmd func() recordRun) recordRun {
	t.Helper()
	done := make(chan recordRun, 1)
	go func() { done <- cmd() }()
	select {
	case r := <-done:
		return r
	case <-time.After(30 * time.Second):
		// The hung goroutine is left running until the test binary exits.
		// That is accepted: the test has already failed.
		t.Fatalf("%s hung on the recursive view", name)
		return recordRun{}
	}
}
