package cassette

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// craftCassette writes a cassette with a session at schema version v, closes
// it and then runs stmts on it through a plain connection, the way an
// attacker would edit the file.
func craftCassette(t *testing.T, v int, stmts ...string) string {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "crafted.cassette")
	c, err := open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.migrate(ctx, migrations[:min(v, len(migrations))]); err != nil {
		t.Fatal(err)
	}
	write(t, c, func(tx *Tx) error { return tx.InsertSession(ctx, testSession()) })
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if v > len(migrations) {
		stmts = append(stmts, "PRAGMA user_version = "+strconv.Itoa(v))
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("crafting %q: %v", s, err)
		}
	}
	return path
}

const recursiveEvents = `CREATE VIEW events AS
WITH RECURSIVE r(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM r)
SELECT n AS seq FROM r`

// AC-35: a cassette whose schema objects would run SQL when it is read is
// rejected before anything is read or migrated, and the file is left as it was.
func TestOpenRejectsUnsafeSchemas(t *testing.T) {
	current := len(migrations)
	for _, tc := range []struct {
		name    string
		version int
		stmts   []string
		want    string
	}{
		{"events is a view", current, []string{"DROP TABLE events", "CREATE VIEW events AS SELECT 1 AS seq"}, `view "events"`},
		{"events is a recursive view", current, []string{"DROP TABLE events", recursiveEvents}, `view "events"`},
		{"another view", current, []string{"CREATE VIEW v AS SELECT 1"}, `view "v"`},
		{"virtual table", current, []string{"CREATE VIRTUAL TABLE notes USING fts5(body)"}, `virtual table "notes"`},
		{"events is a virtual table", current, []string{"DROP TABLE events", "CREATE VIRTUAL TABLE events USING fts5(seq)"}, `virtual table "events"`},
		{"extra trigger", current, []string{"CREATE TRIGGER evil AFTER INSERT ON blobs BEGIN SELECT 1; END"}, `trigger "evil"`},
		{"migration trigger with other SQL", current, []string{
			"DROP TRIGGER session_single_row",
			"CREATE TRIGGER session_single_row BEFORE INSERT ON session BEGIN SELECT 1; END",
		}, `trigger "session_single_row"`},
		{"generated column", current, []string{
			"ALTER TABLE events ADD COLUMN g TEXT GENERATED ALWAYS AS (printf('%s', kind)) VIRTUAL",
		}, `generated column "g" in "events"`},
		{"missing table", current, []string{"DROP TABLE chunks"}, `table "chunks" is missing`},
		{"older schema with a view", 1, []string{"CREATE VIEW v AS SELECT 1"}, `view "v"`},
		{"newer schema with a view", current + 1, []string{"CREATE VIEW v AS SELECT 1"}, `view "v"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := craftCassette(t, tc.version, tc.stmts...)
			before, err := os.ReadFile(path) //nolint:gosec // G304: a file of this test
			if err != nil {
				t.Fatal(err)
			}
			// The check must not query the recursive view: the timeout turns a
			// hang into a failure instead of a stuck test.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			for _, opener := range []func(context.Context, string) (*Cassette, error){Open, OpenForRead} {
				c, err := opener(ctx, path)
				if err == nil {
					_ = c.Close()
					t.Fatal("opened a cassette with an unsafe schema")
				}
				if !errors.Is(err, ErrUnsafeSchema) || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("err = %v, want ErrUnsafeSchema mentioning %s", err, tc.want)
				}
				newer := tc.version > len(migrations)
				if errors.Is(err, ErrNewerSchema) != newer || strings.Contains(err.Error(), "newer") != newer {
					t.Errorf("err = %v, want it to say the schema is newer: %v", err, newer)
				}
			}
			after, err := os.ReadFile(path) //nolint:gosec // G304: a file of this test
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Error("opening the rejected cassette modified it")
			}
			for _, side := range sidecars(path) {
				if _, err := os.Stat(side); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("opening the rejected cassette left %s (err %v)", filepath.Base(side), err)
				}
			}
		})
	}
}

// Other tables and indexes run no SQL when the cassette is read (spec §7.6).
func TestOpenAllowsExtraTablesAndIndexes(t *testing.T) {
	path := craftCassette(t, len(migrations),
		"CREATE TABLE notes (body TEXT)",
		"CREATE INDEX notes_body ON notes (lower(body))",
		"CREATE UNIQUE INDEX events_kind ON events (kind, seq)",
	)
	c, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer c.Close()
	if _, err := c.Session(context.Background()); err != nil {
		t.Errorf("Session: %v", err)
	}
}

func TestCreateKeyword(t *testing.T) {
	for _, tc := range []struct{ sql, want string }{
		{"CREATE TABLE t (a)", "TABLE"},
		{"create view v as select 1", "VIEW"},
		{"CREATE /* x */ VIRTUAL TABLE t USING fts5(a)", "VIRTUAL"},
		{"CREATE\n-- TABLE\n\tTRIGGER t", "TRIGGER"},
		{"CREATE TEMP VIEW v AS SELECT 1", "VIEW"},
		{"CREATE TEMPORARY TRIGGER t", "TRIGGER"},
		{"CREATE UNIQUE INDEX i ON t (a)", "UNIQUE"},
		{"CREATE /* unterminated", ""},
		{"CREATE -- unterminated", ""},
		{"DROP TABLE t", ""},
		{"", ""},
	} {
		if got := createKeyword(tc.sql); got != tc.want {
			t.Errorf("createKeyword(%q) = %q, want %q", tc.sql, got, tc.want)
		}
	}
}

// A trigger a later migration rewrites keeps its old text allowed, so a
// cassette at the older version still opens and migrates (spec §7.6).
func TestTriggersOfKeepsEveryVersion(t *testing.T) {
	v1 := "CREATE TABLE t (a);\nCREATE TRIGGER t_ins BEFORE INSERT ON t BEGIN SELECT 1; END;"
	v2 := "DROP TRIGGER t_ins;\nCREATE TRIGGER t_ins BEFORE INSERT ON t BEGIN SELECT 2; END;"
	got, err := triggersOf(context.Background(), []string{v1, v2})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]map[string]bool{"t_ins": {
		"CREATE TRIGGER t_ins BEFORE INSERT ON t BEGIN SELECT 1; END": true,
		"CREATE TRIGGER t_ins BEFORE INSERT ON t BEGIN SELECT 2; END": true,
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("triggersOf = %v, want %v", got, want)
	}
}

// AC-35: checkObject allows only what spec §7.6 allows, and rejects an
// object kind it does not know.
func TestCheckObject(t *testing.T) {
	allowed := map[string]map[string]bool{"tr": {"CREATE TRIGGER tr AFTER INSERT ON t BEGIN SELECT 1; END": true}}
	for _, tc := range []struct {
		name, sql string
		null      bool
		want      string // "" when allowed
	}{
		{"autoindex", "", true, ""},
		{"t", "CREATE TABLE t (a)", false, ""},
		{"i", "CREATE INDEX i ON t (a)", false, ""},
		{"u", "CREATE UNIQUE INDEX u ON t (a)", false, ""},
		{"tr", "CREATE TRIGGER tr AFTER INSERT ON t BEGIN SELECT 1; END", false, ""},
		{"tr", "CREATE TRIGGER tr AFTER INSERT ON t BEGIN SELECT 2; END", false, `trigger "tr"`},
		{"other", "CREATE TRIGGER other AFTER INSERT ON t BEGIN SELECT 1; END", false, `trigger "other"`},
		{"v", "CREATE VIEW v AS SELECT 1", false, `view "v"`},
		{"x", "CREATE VIRTUAL TABLE x USING fts5(a)", false, `virtual table "x"`},
		{"w", "CREATE WIDGET w", false, `object "w" of an unknown kind`},
		{"e", "", false, `object "e" of an unknown kind`},
	} {
		err := checkObject(tc.name, sql.NullString{String: tc.sql, Valid: !tc.null}, allowed)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("checkObject(%q, %q) = %v, want allowed", tc.name, tc.sql, err)
		case tc.want != "" && (!errors.Is(err, ErrUnsafeSchema) || !strings.Contains(fmt.Sprint(err), tc.want)):
			t.Errorf("checkObject(%q, %q) = %v, want ErrUnsafeSchema mentioning %s", tc.name, tc.sql, err, tc.want)
		}
	}
}

// AC-35 for a cassette shipped with a -wal that holds the unsafe object, as a
// crashed recording leaves it. The rejection writes nothing: no migration and
// no journal mode switch. SQLite's own checkpoint when the connection closes
// may fold the -wal into the main file, which changes its bytes but not its
// content, so this checks the content.
func TestOpenRejectsAnUnsafeWALCassetteWithoutMigrating(t *testing.T) {
	ctx := context.Background()
	src := craftCassette(t, 1)
	db, err := sql.Open("sqlite", src)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, s := range []string{"PRAGMA journal_mode = WAL", "PRAGMA wal_autocheckpoint = 0", "CREATE VIEW v AS SELECT 1"} {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	// Copy the files while the connection keeps the view in the -wal.
	path := filepath.Join(t.TempDir(), "wal.cassette")
	for _, suffix := range []string{"", "-wal"} {
		b, err := os.ReadFile(src + suffix) //nolint:gosec // G304: a file of this test
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+suffix, b, 0o600); err != nil { //nolint:gosec // G703: a file of this test
			t.Fatal(err)
		}
	}

	if c, err := OpenForRead(ctx, path); !errors.Is(err, ErrUnsafeSchema) {
		if err == nil {
			_ = c.Close()
		}
		t.Fatalf("OpenForRead: err = %v, want ErrUnsafeSchema", err)
	}
	if c, err := Open(ctx, path); !errors.Is(err, ErrUnsafeSchema) {
		if err == nil {
			_ = c.Close()
		}
		t.Fatalf("Open: err = %v, want ErrUnsafeSchema", err)
	}

	after, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer after.Close()
	var version, views, sessions int
	if err := after.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := after.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE type = 'view'").Scan(&views); err != nil {
		t.Fatal(err)
	}
	if err := after.QueryRowContext(ctx, "SELECT count(*) FROM session").Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if version != 1 || views != 1 || sessions != 1 {
		t.Errorf("after the rejection: user_version %d, views %d, sessions %d; want 1, 1, 1 (unchanged)", version, views, sessions)
	}
}

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// The stored text of every trigger the migrations create is pinned: Open
// accepts a trigger only with that exact text (spec §7.6), so changing it in a
// shipped migration, even its whitespace, would make every existing cassette
// unreadable. This golden file may only grow, with a new migration.
func TestMigrationTriggersArePinned(t *testing.T) {
	triggers, err := migrationTriggers()
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, name := range slices.Sorted(maps.Keys(triggers)) {
		for _, text := range slices.Sorted(maps.Keys(triggers[name])) {
			fmt.Fprintf(&b, "-- trigger %s\n%s\n\n", name, text)
		}
	}
	path := filepath.Join("testdata", "migration_triggers.golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (rerun with -update to create it)", err)
	}
	if b.String() != string(want) {
		t.Errorf("migration triggers differ from %s: a shipped migration's trigger text changed, which makes existing cassettes unreadable\ngot:\n%s\nwant:\n%s", path, b.String(), want)
	}
}
