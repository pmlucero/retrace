package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"

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

	r := runLsCmd(dir, "--json")
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

	if r := runShowCmd(dir, "evil"); r.code != ExitError {
		t.Errorf("show: exit code %d, want %d; stderr:\n%s", r.code, ExitError, r.stderr)
	}
	if r := runRunCmd(t, dir, "evil", "--listen", "127.0.0.1:0"); r.code != ExitError || !strings.Contains(r.stderr.String(), "cannot open the cassette") {
		t.Errorf("run: exit code %d, want %d and cannot open the cassette; stderr:\n%s", r.code, ExitError, r.stderr)
	}

	after, err := os.ReadFile(path) //nolint:gosec // G304: a file of this test
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("a command modified the rejected cassette")
	}
}
