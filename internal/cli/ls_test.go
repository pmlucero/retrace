package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
)

// lsT0 is the creation time of the seeded sessions.
var lsT0 = time.Date(2026, 10, 23, 9, 30, 0, 0, time.UTC)

// utcDisplay shows text times in UTC for the duration of the test, so golden
// files do not depend on the machine's time zone.
func utcDisplay(t *testing.T) {
	t.Helper()
	prev := displayLocation
	displayLocation = time.UTC
	t.Cleanup(func() { displayLocation = prev })
}

// seedSession writes a cassette with session s, its events, and, when end is
// not nil, the end of the session.
func seedSession(t *testing.T, dir string, s cassette.Session, end *cassette.SessionEnd, events ...cassette.Event) {
	t.Helper()
	ctx := context.Background()
	c, err := cassette.Create(ctx, cassette.Path(dir, s.ID))
	if err != nil {
		t.Fatal(err)
	}
	if s.Command == nil {
		s.Command = []string{"agent"}
	}
	if s.OnMiss == "" {
		s.OnMiss = "strict"
	}
	err = c.Write(ctx, func(tx *cassette.Tx) error {
		if err := tx.InsertSession(ctx, s); err != nil {
			return err
		}
		for _, e := range events {
			if err := tx.AppendEvent(ctx, e); err != nil {
				return err
			}
		}
		if end != nil {
			return tx.EndSession(ctx, *end)
		}
		return nil
	})
	if cerr := c.Close(); err != nil || cerr != nil {
		t.Fatal(err, cerr)
	}
}

// response builds a model_response event at seq.
func response(seq int64, model string, in, out int64, cost *float64) cassette.Event {
	return cassette.Event{
		Seq: seq, Kind: "model_response", Source: "model", Target: "agent",
		StartedAt: lsT0.Add(time.Duration(seq) * time.Second), Model: &model,
		TokensIn: &in, TokensOut: &out, CostUSD: cost, Status: "ok", Provenance: "recorded",
		Result: json.RawMessage(`{"stop_reason":"end_turn","text":""}`),
	}
}

// toolCall builds a tool_call event at seq.
func toolCall(seq int64, tool, args string) cassette.Event {
	return cassette.Event{
		Seq: seq, Kind: "tool_call", Source: "agent", Target: "mcp:shop",
		StartedAt: lsT0.Add(time.Duration(seq) * time.Second), Tool: &tool,
		Arguments: json.RawMessage(args), Status: "ok", Provenance: "recorded",
	}
}

func cost(usd float64) *float64 { return &usd }

// seedLsSessions writes the sessions that the ls tests list: a recording, an
// exact replay of it with dropped events, an unpriced swap-model replay and a
// session still recording.
func seedLsSessions(t *testing.T, dir string) {
	t.Helper()
	parent := "checkout"
	seedSession(t, dir,
		cassette.Session{ID: "checkout", Mode: "record", Status: "recording", CreatedAt: lsT0},
		&cassette.SessionEnd{Status: "completed", EndedAt: lsT0.Add(38200 * time.Millisecond)},
		response(1, "claude-sonnet-4-5", 20_000, 1_500, cost(0.1)),
		toolCall(2, "search_orders", `{"customer_id":"c_812"}`),
		response(3, "claude-sonnet-4-5", 21_230, 2_402, cost(0.08)),
	)
	seedSession(t, dir,
		cassette.Session{ID: "checkout.exact.1", Mode: "exact", ParentSessionID: &parent, Status: "recording", CreatedAt: lsT0.Add(time.Hour)},
		&cassette.SessionEnd{Status: "completed", EndedAt: lsT0.Add(time.Hour + 2*time.Second), DroppedEvents: 2},
		response(1, "claude-sonnet-4-5", 20_000, 1_500, cost(0.1)),
	)
	seedSession(t, dir,
		cassette.Session{ID: "checkout.swap-model.1", Mode: "swap-model", ParentSessionID: &parent, Status: "recording", CreatedAt: lsT0.Add(2 * time.Hour)},
		&cassette.SessionEnd{Status: "diverged", EndedAt: lsT0.Add(2*time.Hour + 61*time.Second)},
		response(1, "claude-haiku-9", 999, 42, nil),
	)
	seedSession(t, dir,
		cassette.Session{ID: "live", Mode: "record", Status: "recording", CreatedAt: lsT0.Add(3 * time.Hour)},
		nil,
	)
}

func runLsCmd(dir string, args ...string) recordRun {
	r := recordRun{stdout: &syncBuffer{}, stderr: &syncBuffer{}}
	all := append([]string{"--dir", dir, "ls"}, args...)
	r.code = run(context.Background(), all, nil, r.stdout, r.stderr, fakeEnv(nil), commands)
	return r
}

// checkGolden compares got with testdata/<name>, rewriting it under -update.
func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path) //nolint:gosec // G304: a golden file of this test
	if err != nil {
		t.Fatalf("%v (rerun with -update to create it)", err)
	}
	if got != string(want) {
		t.Errorf("output differs from %s (rerun with -update if the change is intended)\ngot:\n%s\nwant:\n%s", path, got, want)
	}
}

// AC-22, AC-27: ls lists every session, newest first, with the columns of
// spec §9.5, and +gaps after the status of a session with dropped events.
func TestLsTable(t *testing.T) {
	utcDisplay(t)
	dir := t.TempDir()
	seedLsSessions(t, dir)

	r := runLsCmd(dir)
	if r.code != ExitOK {
		t.Fatalf("exit code %d, stderr:\n%s", r.code, r.stderr)
	}
	checkGolden(t, "ls/table.txt", r.stdout.String())
}

// AC-22, AC-27: ls --json carries the same sessions, unabbreviated, with
// dropped_events.
func TestLsJSON(t *testing.T) {
	dir := t.TempDir()
	seedLsSessions(t, dir)

	r := runLsCmd(dir, "--json")
	if r.code != ExitOK {
		t.Fatalf("exit code %d, stderr:\n%s", r.code, r.stderr)
	}
	checkGolden(t, "ls/sessions.json", r.stdout.String())

	var got []lsEntry
	if err := json.Unmarshal([]byte(r.stdout.String()), &got); err != nil {
		t.Fatalf("not one JSON array: %v", err)
	}
	var ids []string
	for _, e := range got {
		ids = append(ids, e.ID)
	}
	if want := []string{"live", "checkout.swap-model.1", "checkout.exact.1", "checkout"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("ids = %v, want newest first %v", ids, want)
	}
}

// Spec §9.5: a cassette that cannot be read is skipped with a warning, and
// ls still exits 0.
func TestLsSkipsUnreadableCassettes(t *testing.T) {
	dir := t.TempDir()
	seedSession(t, dir, cassette.Session{ID: "ok", Mode: "record", Status: "recording", CreatedAt: lsT0}, nil)
	if err := os.WriteFile(filepath.Join(dir, "garbage.cassette"), []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A cassette without a session row.
	c, err := cassette.Create(context.Background(), cassette.Path(dir, "empty"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	// Neither a directory nor another extension is a cassette.
	if err := os.Mkdir(filepath.Join(dir, "sub.cassette"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	r := runLsCmd(dir, "--json")
	if r.code != ExitOK {
		t.Fatalf("exit code %d, stderr:\n%s", r.code, r.stderr)
	}
	var got []lsEntry
	if err := json.Unmarshal([]byte(r.stdout.String()), &got); err != nil || len(got) != 1 || got[0].ID != "ok" {
		t.Errorf("listed %+v (%v), want only ok", got, err)
	}
	stderr := r.stderr.String()
	for _, f := range []string{"garbage.cassette", "empty.cassette"} {
		if !strings.Contains(stderr, f) {
			t.Errorf("stderr does not warn about %s:\n%s", f, stderr)
		}
	}
	if strings.Contains(stderr, "sub.cassette") || strings.Contains(stderr, "notes.txt") {
		t.Errorf("stderr warns about a file that is not a cassette:\n%s", stderr)
	}
}

// A session that is still being recorded is listed while its recorder holds
// the cassette open, and ls leaves its WAL alone: checkpointing it would block
// the recorder's writes (cassette.OpenForRead). This checks what ls does to
// the file, not the recorder's latency under contention.
func TestLsWhileRecording(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	path := cassette.Path(dir, "live")
	c, err := cassette.Create(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Write(ctx, func(tx *cassette.Tx) error {
		return tx.InsertSession(ctx, cassette.Session{ID: "live", Mode: "record", Status: "recording",
			CreatedAt: lsT0, Command: []string{"agent"}, OnMiss: "strict"})
	}); err != nil {
		t.Fatal(err)
	}
	walSize := func() int64 {
		t.Helper()
		fi, err := os.Stat(path + "-wal")
		if err != nil {
			t.Fatal(err)
		}
		return fi.Size()
	}
	before := walSize()

	r := runLsCmd(dir, "--json")
	if r.code != ExitOK || !strings.Contains(r.stdout.String(), `"id": "live"`) {
		t.Fatalf("exit code %d, stdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
	}
	if after := walSize(); after != before {
		t.Errorf("the WAL went from %d to %d bytes: ls checkpointed the live recording", before, after)
	}
	if err := c.Write(ctx, func(tx *cassette.Tx) error { return tx.AppendEvent(ctx, toolCall(1, "t", `{}`)) }); err != nil {
		t.Errorf("the recorder cannot write after ls: %v", err)
	}
}

func TestLsEmpty(t *testing.T) {
	for _, tc := range []struct {
		name, dir string
		args      []string
		want      string
	}{
		{"missing directory, text", "missing", nil, "ID  CREATED  MODE  PARENT  STATUS  STEPS  TOKENS  COST (est.)  DURATION\n"},
		{"missing directory, json", "missing", []string{"--json"}, "[]\n"},
		{"empty directory, json", "", []string{"--json"}, "[]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), tc.dir)
			r := runLsCmd(dir, tc.args...)
			if r.code != ExitOK || r.stdout.String() != tc.want {
				t.Errorf("exit code %d, stdout %q; want 0 and %q", r.code, r.stdout, tc.want)
			}
		})
	}
}

func TestLsErrors(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, dir string
		args      []string
		code      int
		stderr    string
	}{
		{"an argument", dir, []string{"x"}, ExitUsage, "ls takes no arguments"},
		{"unknown flag", dir, []string{"--nope"}, ExitUsage, "flag provided but not defined"},
		{"global flag after the command", dir, []string{"--dir", dir}, ExitUsage, "global flag"},
		{"dir is a file", file, nil, ExitError, "cannot list the sessions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runLsCmd(tc.dir, tc.args...)
			if r.code != tc.code || !strings.Contains(r.stderr.String(), tc.stderr) {
				t.Errorf("exit code %d, stderr %q; want %d and %q", r.code, r.stderr, tc.code, tc.stderr)
			}
		})
	}
}
