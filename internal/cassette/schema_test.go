package cassette

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
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
		{"DROP TABLE t", ""},
		{"", ""},
	} {
		if got := createKeyword(tc.sql); got != tc.want {
			t.Errorf("createKeyword(%q) = %q, want %q", tc.sql, got, tc.want)
		}
	}
}
