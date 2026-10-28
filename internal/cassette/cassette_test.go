package cassette

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"testing/fstest"
	"time"
)

// at returns a fixed UTC time with microsecond precision, the precision of the
// cassette's time columns (spec §7.1).
func at(sec int) time.Time {
	return time.Date(2026, 10, 23, 12, 0, sec, 123456000, time.UTC)
}

func ptr[T any](v T) *T { return &v }

func testSession() Session {
	return Session{
		ID:             "20261023-120000-ab12",
		Mode:           "record",
		Status:         "recording",
		CreatedAt:      at(0),
		Command:        []string{"python", "agent.py"},
		Cwd:            "/work",
		OnMiss:         "strict",
		MCPServers:     json.RawMessage(`[{"name":"orders","url":"http://localhost:9001/mcp"}]`),
		Redaction:      json.RawMessage(`{"headers":["authorization"]}`),
		RetraceVersion: "dev",
		DeriverVersion: 1,
		PricingVersion: "2026-10",
		Labels:         map[string]string{"case": "checkout"},
	}
}

func newCassette(t *testing.T) (*Cassette, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.cassette")
	c, err := Create(context.Background(), path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, path
}

func write(t *testing.T, c *Cassette, fn func(*Tx) error) {
	t.Helper()
	if err := c.Write(context.Background(), fn); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

func TestRoundTripSurvivesCloseAndReopen(t *testing.T) {
	ctx := context.Background()
	c, path := newCassette(t)

	wantSession := testSession()
	var reqKey string
	wantInteraction := Interaction{
		ID: 1, Channel: "llm", Upstream: "anthropic", HTTPMethod: "POST", Path: "/v1/messages",
		RequestHeaders: json.RawMessage(`{"content-type":["application/json"]}`),
		Fingerprint:    ptr("fp-1"), StartedAt: at(1), Provenance: "recorded",
	}
	completion := Completion{
		StatusCode: ptr(200), ResponseHeaders: json.RawMessage(`{"content-type":["text/event-stream"]}`),
		Streamed: true, TTFBMillis: ptr[int64](40), DurationMillis: 900,
	}
	wantChunks := []Chunk{
		{InteractionID: 1, Index: 0, OffsetMillis: 40, Data: []byte("event: message_start\ndata: {}\n\n")},
		{InteractionID: 1, Index: 1, OffsetMillis: 900, Data: []byte("event: message_stop\ndata: {}\n\n")},
	}
	wantEvents := []Event{
		{Seq: 1, InteractionID: ptr[int64](1), Kind: "model_response", Source: "model", Target: "agent",
			StartedAt: at(1), DurationMillis: ptr[int64](900), Model: ptr("claude-x"), RequestedModel: ptr("claude-x"),
			Result: json.RawMessage(`{"stop_reason":"tool_use"}`), TokensIn: ptr[int64](10), TokensOut: ptr[int64](5),
			CostUSD: ptr(0.0012), Status: "ok", Provenance: "recorded", DeriverVersion: 1},
		{Seq: 2, ParentSeq: ptr[int64](1), InteractionID: ptr[int64](1), Kind: "tool_use", Source: "model",
			Target: "agent", StartedAt: at(2), Tool: ptr("search"), Arguments: json.RawMessage(`{"q":"x"}`),
			Status: "ok", Provenance: "recorded", DeriverVersion: 1},
	}

	write(t, c, func(tx *Tx) error {
		if err := tx.InsertSession(ctx, wantSession); err != nil {
			return err
		}
		var err error
		if reqKey, err = tx.PutBlob(ctx, []byte(`{"model":"claude-x"}`)); err != nil {
			return err
		}
		wantInteraction.RequestBlob = &reqKey
		return tx.BeginInteraction(ctx, wantInteraction)
	})
	write(t, c, func(tx *Tx) error {
		for _, ch := range wantChunks {
			if err := tx.AppendChunk(ctx, ch); err != nil {
				return err
			}
		}
		if err := tx.CompleteInteraction(ctx, 1, completion); err != nil {
			return err
		}
		for _, e := range wantEvents {
			if err := tx.AppendEvent(ctx, e); err != nil {
				return err
			}
		}
		return tx.EndSession(ctx, SessionEnd{Status: "completed", EndedAt: at(3), AgentExitCode: ptr(0), DroppedEvents: 2})
	})
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// ADR 0005: after Close the cassette is a single self-contained file.
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists after Close (err = %v)", path+suffix, err)
		}
	}

	r, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()

	gotSession, err := r.Session(ctx)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	wantSession.Status, wantSession.EndedAt, wantSession.AgentExitCode = "completed", ptr(at(3)), ptr(0)
	wantSession.DroppedEvents = 2
	if !reflect.DeepEqual(gotSession, wantSession) {
		t.Errorf("session:\n got %+v\nwant %+v", gotSession, wantSession)
	}

	gotInteractions, err := r.Interactions(ctx)
	if err != nil {
		t.Fatalf("Interactions: %v", err)
	}
	wantInteraction.StatusCode, wantInteraction.ResponseHeaders = completion.StatusCode, completion.ResponseHeaders
	wantInteraction.Streamed, wantInteraction.TTFBMillis = true, completion.TTFBMillis
	wantInteraction.DurationMillis = ptr(completion.DurationMillis)
	if !reflect.DeepEqual(gotInteractions, []Interaction{wantInteraction}) {
		t.Errorf("interactions:\n got %+v\nwant %+v", gotInteractions, []Interaction{wantInteraction})
	}

	gotChunks, err := r.Chunks(ctx, 1)
	if err != nil {
		t.Fatalf("Chunks: %v", err)
	}
	if !reflect.DeepEqual(gotChunks, wantChunks) {
		t.Errorf("chunks:\n got %+v\nwant %+v", gotChunks, wantChunks)
	}

	gotEvents, err := r.Events(ctx)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if !reflect.DeepEqual(gotEvents, wantEvents) {
		t.Errorf("events:\n got %+v\nwant %+v", gotEvents, wantEvents)
	}

	blob, err := r.Blob(ctx, reqKey)
	if err != nil || string(blob) != `{"model":"claude-x"}` {
		t.Errorf("Blob(%s) = %q, %v", reqKey, blob, err)
	}
}

func TestCreateRefusesExistingFile(t *testing.T) {
	_, path := newCassette(t)
	if _, err := Create(context.Background(), path); !errors.Is(err, ErrExists) {
		t.Errorf("Create on existing file: err = %v, want ErrExists", err)
	}
}

func TestPutBlobDeduplicatesContent(t *testing.T) {
	ctx := context.Background()
	c, _ := newCassette(t)
	var k1, k2 string
	write(t, c, func(tx *Tx) error {
		var err error
		if k1, err = tx.PutBlob(ctx, []byte("same")); err != nil {
			return err
		}
		k2, err = tx.PutBlob(ctx, []byte("same"))
		return err
	})

	if k1 != k2 {
		t.Errorf("keys differ for identical content: %s vs %s", k1, k2)
	}
	var n int
	if err := c.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM blobs").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("blobs rows = %d, want 1", n)
	}
}

// An empty body, which callers often hold as a nil slice, is stored as an
// empty blob, not rejected by the NOT NULL data column.
func TestPutBlobStoresAnEmptyBody(t *testing.T) {
	ctx := context.Background()
	c, _ := newCassette(t)
	var kNil, kEmpty string
	write(t, c, func(tx *Tx) error {
		var err error
		if kNil, err = tx.PutBlob(ctx, nil); err != nil {
			return err
		}
		kEmpty, err = tx.PutBlob(ctx, []byte{})
		return err
	})

	if kNil != BlobKey(nil) || kEmpty != kNil {
		t.Errorf("keys %s and %s, want both %s", kNil, kEmpty, BlobKey(nil))
	}
	got, err := c.Blob(ctx, kNil)
	if err != nil || len(got) != 0 {
		t.Errorf("Blob = %q, %v; want an empty body", got, err)
	}
}

func TestCompleteInteractionExactlyOnce(t *testing.T) {
	ctx := context.Background()
	c, _ := newCassette(t)
	write(t, c, func(tx *Tx) error {
		return tx.BeginInteraction(ctx, Interaction{ID: 7, Channel: "mcp", Upstream: "mcp:orders",
			HTTPMethod: "POST", Path: "/", StartedAt: at(0), Provenance: "recorded"})
	})
	complete := func(id int64) error {
		return c.Write(ctx, func(tx *Tx) error {
			return tx.CompleteInteraction(ctx, id, Completion{DurationMillis: 5})
		})
	}

	if err := complete(7); err != nil {
		t.Fatalf("first completion: %v", err)
	}
	if err := complete(7); !errors.Is(err, ErrAlreadyCompleted) {
		t.Errorf("second completion: err = %v, want ErrAlreadyCompleted", err)
	}
	if err := complete(8); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown interaction: err = %v, want ErrNotFound", err)
	}
}

func TestWriteRollsBackOnError(t *testing.T) {
	ctx := context.Background()
	c, _ := newCassette(t)
	boom := errors.New("boom")

	err := c.Write(ctx, func(tx *Tx) error {
		if err := tx.InsertSession(ctx, testSession()); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Write: err = %v, want boom", err)
	}
	if _, err := c.Session(ctx); !errors.Is(err, ErrNotFound) {
		t.Errorf("Session after rollback: err = %v, want ErrNotFound", err)
	}
}

func TestSchemaRejectsInvalidValuesAndDanglingReferences(t *testing.T) {
	ctx := context.Background()
	c, _ := newCassette(t)

	bad := testSession()
	bad.Mode = "rewind"
	if err := c.Write(ctx, func(tx *Tx) error { return tx.InsertSession(ctx, bad) }); err == nil {
		t.Error("InsertSession with invalid mode: want error")
	}
	chunk := Chunk{InteractionID: 99, Index: 0, Data: []byte("x")}
	if err := c.Write(ctx, func(tx *Tx) error { return tx.AppendChunk(ctx, chunk) }); err == nil {
		t.Error("AppendChunk for a missing interaction: want foreign key error")
	}
}

func TestOpenNewerSchemaIsReadOnly(t *testing.T) {
	ctx := context.Background()
	c, path := newCassette(t)
	write(t, c, func(tx *Tx) error { return tx.InsertSession(ctx, testSession()) })
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	setUserVersion(t, path, len(migrations)+1)

	r, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()

	if _, err := r.Session(ctx); err != nil {
		t.Errorf("reading a newer cassette: %v", err)
	}
	err = r.Write(ctx, func(tx *Tx) error {
		_, err := tx.PutBlob(ctx, []byte("x"))
		return err
	})
	if !errors.Is(err, ErrNewerSchema) {
		t.Errorf("writing a newer cassette: err = %v, want ErrNewerSchema", err)
	}
}

func TestMigrateAppliesOnlyPendingMigrations(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "m.cassette")
	all := []string{
		"CREATE TABLE a (x INTEGER)",
		"CREATE TABLE b (y INTEGER)",
	}

	c, err := open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.migrate(ctx, all[:1]); err != nil {
		t.Fatalf("migrate to v1: %v", err)
	}
	// Re-running v1 would fail with "table a already exists": only v2 may run.
	if err := c.migrate(ctx, all); err != nil {
		t.Fatalf("migrate v1 -> v2: %v", err)
	}
	if c.SchemaVersion() != 2 {
		t.Errorf("schema = %d, want 2", c.SchemaVersion())
	}
	var version int
	if err := c.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != 2 {
		t.Errorf("user_version = %d (err %v), want 2", version, err)
	}
}

// Schema v2 adds session.dropped_events (spec §7.7, ADR 0010). A v1 cassette
// migrates to v2 when opened, with dropped_events 0, and can then be ended
// with a count.
func TestOpenMigratesV1CassetteWithNoDroppedEvents(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v1.cassette")
	v1, err := open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := v1.migrate(ctx, migrations[:1]); err != nil {
		t.Fatalf("migrate to v1: %v", err)
	}
	write(t, v1, func(tx *Tx) error { return tx.InsertSession(ctx, testSession()) })
	if err := v1.Close(); err != nil {
		t.Fatal(err)
	}

	c, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer c.Close()
	if c.SchemaVersion() != len(migrations) {
		t.Errorf("schema = %d, want %d", c.SchemaVersion(), len(migrations))
	}
	s, err := c.Session(ctx)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if !reflect.DeepEqual(s, testSession()) {
		t.Errorf("migrated session:\n got %+v\nwant %+v", s, testSession())
	}

	write(t, c, func(tx *Tx) error {
		return tx.EndSession(ctx, SessionEnd{Status: "completed", EndedAt: at(3), DroppedEvents: 4})
	})
	if s, err = c.Session(ctx); err != nil || s.DroppedEvents != 4 {
		t.Errorf("dropped events after EndSession = %d (err %v), want 4", s.DroppedEvents, err)
	}
}

func TestCreateWritesCurrentSchema(t *testing.T) {
	ctx := context.Background()
	c, _ := newCassette(t)
	if c.SchemaVersion() != len(migrations) {
		t.Errorf("schema = %d, want %d", c.SchemaVersion(), len(migrations))
	}
	var (
		notNull int
		dflt    sql.NullString
	)
	err := c.db.QueryRowContext(ctx,
		`SELECT "notnull", dflt_value FROM pragma_table_info('session') WHERE name = 'dropped_events' AND type = 'INTEGER'`).
		Scan(&notNull, &dflt)
	if err != nil || notNull != 1 || dflt.String != "0" {
		t.Errorf("session.dropped_events: notnull = %d, default = %v (err %v), want INTEGER NOT NULL DEFAULT 0", notNull, dflt, err)
	}
}

func TestSessionDroppedEventsAreSetWhenTheSessionEnds(t *testing.T) {
	ctx := context.Background()
	c, _ := newCassette(t)

	s := testSession()
	s.DroppedEvents = 1
	if err := c.Write(ctx, func(tx *Tx) error { return tx.InsertSession(ctx, s) }); err == nil {
		t.Error("InsertSession with dropped events: want error")
	}
	write(t, c, func(tx *Tx) error { return tx.InsertSession(ctx, testSession()) })
	if got, err := c.Session(ctx); err != nil || got.DroppedEvents != 0 {
		t.Errorf("dropped events while recording = %d (err %v), want 0", got.DroppedEvents, err)
	}
	end := SessionEnd{Status: "completed", EndedAt: at(3), DroppedEvents: -1}
	if err := c.Write(ctx, func(tx *Tx) error { return tx.EndSession(ctx, end) }); err == nil {
		t.Error("EndSession with negative dropped events: want error")
	}
}

func setUserVersion(t *testing.T, path string, v int) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(context.Background(), "PRAGMA user_version = "+strconv.Itoa(v)); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRejectsFilesThatAreNotCassettes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	foreign := filepath.Join(dir, "app.db")
	db, err := sql.Open("sqlite", foreign)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE users (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	empty := filepath.Join(dir, "empty.cassette") // e.g. left by a Create that crashed
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{foreign, empty} {
		if _, err := Open(ctx, path); !errors.Is(err, ErrNotCassette) {
			t.Errorf("Open(%s): err = %v, want ErrNotCassette", filepath.Base(path), err)
		}
	}
	if got := tableNames(t, foreign); !reflect.DeepEqual(got, []string{"users"}) {
		t.Errorf("foreign database was modified: tables = %v, want [users]", got)
	}
}

func TestOpenReturnsCrashedRecordingToSingleFile(t *testing.T) {
	ctx := context.Background()
	c, path := newCassette(t)
	write(t, c, func(tx *Tx) error { return tx.InsertSession(ctx, testSession()) })
	// Simulate a crash: the connection goes away without Close's checkpoint
	// and journal mode switch, so the file stays in (persistent) WAL mode.
	if err := c.db.Close(); err != nil {
		t.Fatal(err)
	}
	if got := journalMode(t, path); got != "wal" {
		t.Fatalf("precondition: journal_mode = %q, want wal", got)
	}

	r, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := journalMode(t, path); got != "delete" {
		t.Errorf("journal_mode after Open/Close = %q, want delete (ADR 0005)", got)
	}
}

func TestCassettesAreOpenedWithUntrustedSchema(t *testing.T) {
	c, _ := newCassette(t)
	var trusted int
	if err := c.db.QueryRowContext(context.Background(), "PRAGMA trusted_schema").Scan(&trusted); err != nil {
		t.Fatal(err)
	}
	if trusted != 0 {
		t.Errorf("trusted_schema = %d, want 0 (cassettes are untrusted input)", trusted)
	}
}

func TestSessionHasExactlyOneRow(t *testing.T) {
	ctx := context.Background()
	c, _ := newCassette(t)
	write(t, c, func(tx *Tx) error { return tx.InsertSession(ctx, testSession()) })

	second := testSession()
	second.ID = "another"
	if err := c.Write(ctx, func(tx *Tx) error { return tx.InsertSession(ctx, second) }); err == nil {
		t.Error("second InsertSession: want error, a cassette has exactly one session")
	}
}

func TestSetMCPServersUpdatesSessionMetadata(t *testing.T) {
	ctx := context.Background()
	c, _ := newCassette(t)
	servers := json.RawMessage(`[{"name":"orders","url":"http://localhost:9001/mcp","protocol_version":"2025-06-18"}]`)
	write(t, c, func(tx *Tx) error {
		if err := tx.InsertSession(ctx, testSession()); err != nil {
			return err
		}
		return tx.SetMCPServers(ctx, servers)
	})

	s, err := c.Session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(s.MCPServers) != string(servers) {
		t.Errorf("mcp_servers = %s, want %s", s.MCPServers, servers)
	}
}

func TestEmbeddedMigrationsAreValid(t *testing.T) {
	if _, err := loadMigrations(migrationFiles); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMigrationsRejectsGapsAndDuplicates(t *testing.T) {
	sqlFile := &fstest.MapFile{Data: []byte("SELECT 1")}
	tests := map[string]fstest.MapFS{
		"gap":       {"migrations/0001_a.sql": sqlFile, "migrations/0003_c.sql": sqlFile},
		"duplicate": {"migrations/0001_a.sql": sqlFile, "migrations/0001_b.sql": sqlFile},
		"no zero":   {"migrations/0000_a.sql": sqlFile},
	}
	for name, fsys := range tests {
		if _, err := loadMigrations(fsys); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func tableNames(t *testing.T, path string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.QueryContext(context.Background(), "SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	return names
}

func journalMode(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var mode string
	if err := db.QueryRowContext(context.Background(), "PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	return mode
}

func TestInteractionIncomplete(t *testing.T) {
	duration := int64(5)
	msg := func(s string) *string { return &s }
	tests := []struct {
		name string
		in   Interaction
		want bool
	}{
		{name: "complete", in: Interaction{DurationMillis: &duration}, want: false},
		{name: "transport error", in: Interaction{DurationMillis: &duration, Error: msg("connection reset")}, want: false},
		{name: "records dropped", in: Interaction{DurationMillis: &duration, Error: msg(RecordingIncomplete + ": queue full")}, want: true},
		{name: "in flight when the recording stopped", in: Interaction{}, want: true},
	}
	for _, tt := range tests {
		if got := tt.in.Incomplete(); got != tt.want {
			t.Errorf("%s: Incomplete() = %v, want %v", tt.name, got, tt.want)
		}
	}
}
