package cassette

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// ErrUnsafeSchema is returned by Open for a file whose schema holds objects
// that would run SQL stored in the file when it is read (spec §7.6).
var ErrUnsafeSchema = errors.New("cassette schema holds objects retrace does not allow")

// expectedTables are the tables retrace reads, which must be ordinary tables
// (spec §7.6).
var expectedTables = []string{"session", "interactions", "events", "blobs", "chunks"}

// checkSchema rejects a cassette whose schema objects would run SQL stored in
// the file when it is read (spec §7.6): a view, a virtual table, a trigger the
// migrations do not create, a generated column in an expected table, or a
// missing expected table. It only reads sqlite_schema and table_xinfo, which
// parse the stored SQL but never evaluate it.
func checkSchema(ctx context.Context, db *sql.DB) error {
	allowed, err := migrationTriggers()
	if err != nil {
		return err
	}
	rows, err := db.QueryContext(ctx, "SELECT name, sql FROM sqlite_schema")
	if err != nil {
		return fmt.Errorf("reading schema: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var stmt sql.NullString
		if err := rows.Scan(&name, &stmt); err != nil {
			return fmt.Errorf("reading schema: %w", err)
		}
		if err := checkObject(name, stmt, allowed); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading schema: %w", err)
	}
	// With no views or virtual tables left, what table_xinfo finds under an
	// expected name is an ordinary table.
	for _, table := range expectedTables {
		if err := checkColumns(ctx, db, table); err != nil {
			return err
		}
	}
	return nil
}

// checkObject rejects a schema object that §7.6 does not allow. It is an
// allow-list, so an object kind it does not know is rejected too. Objects are
// classified by their SQL, not by sqlite_schema.type: SQLite builds each
// object from its SQL, and a crafted file can set type to anything.
func checkObject(name string, stmt sql.NullString, allowed map[string]map[string]bool) error {
	if !stmt.Valid {
		return nil // an index SQLite creates for a UNIQUE or PRIMARY KEY constraint
	}
	switch createKeyword(stmt.String) {
	case "TABLE", "INDEX", "UNIQUE":
		return nil
	case "TRIGGER":
		if allowed[name][stmt.String] {
			return nil
		}
		return fmt.Errorf("%w: trigger %q is not one retrace creates", ErrUnsafeSchema, name)
	case "VIEW":
		return fmt.Errorf("%w: view %q", ErrUnsafeSchema, name)
	case "VIRTUAL":
		return fmt.Errorf("%w: virtual table %q", ErrUnsafeSchema, name)
	default:
		return fmt.Errorf("%w: object %q of an unknown kind", ErrUnsafeSchema, name)
	}
}

// checkColumns rejects table when it is missing or has a generated column,
// whose expression runs when the column is read.
func checkColumns(ctx context.Context, db *sql.DB, table string) error {
	rows, err := db.QueryContext(ctx, "SELECT name, hidden FROM pragma_table_xinfo(?)", table)
	if err != nil {
		return fmt.Errorf("reading the columns of %s: %w", table, err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var col string
		var hidden int
		if err := rows.Scan(&col, &hidden); err != nil {
			return fmt.Errorf("reading the columns of %s: %w", table, err)
		}
		// hidden is 2 for a virtual and 3 for a stored generated column.
		if hidden != 0 {
			return fmt.Errorf("%w: generated column %q in %q", ErrUnsafeSchema, col, table)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading the columns of %s: %w", table, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: table %q is missing", ErrUnsafeSchema, table)
	}
	return nil
}

// migrationTriggers returns the triggers the migrations create, by name, with
// every SQL text SQLite stores for them at any schema version. A cassette at
// an older version may hold a trigger a later migration drops or rewrites,
// and it must still open so that the migrations can bring it forward. It runs
// the migrations once on an in-memory database.
var migrationTriggers = sync.OnceValues(func() (map[string]map[string]bool, error) {
	return triggersOf(context.Background(), migrations)
})

// triggersOf runs migs on an in-memory database and returns, by name, every
// SQL text a trigger has after any of them.
func triggersOf(ctx context.Context, migs []string) (map[string]map[string]bool, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, fmt.Errorf("listing the migrations' triggers: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1) // each connection has its own in-memory database
	out := map[string]map[string]bool{}
	for _, m := range migs {
		if _, err := db.ExecContext(ctx, m); err != nil {
			return nil, fmt.Errorf("listing the migrations' triggers: %w", err)
		}
		if err := addTriggers(ctx, db, out); err != nil {
			return nil, fmt.Errorf("listing the migrations' triggers: %w", err)
		}
	}
	return out, nil
}

// addTriggers adds the triggers in db to set.
func addTriggers(ctx context.Context, db *sql.DB, set map[string]map[string]bool) error {
	rows, err := db.QueryContext(ctx, "SELECT name, sql FROM sqlite_schema WHERE type = 'trigger'")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name, stmt string
		if err := rows.Scan(&name, &stmt); err != nil {
			return err
		}
		if set[name] == nil {
			set[name] = map[string]bool{}
		}
		set[name][stmt] = true
	}
	return rows.Err()
}

// createKeyword returns the upper-cased keyword after CREATE (and after TEMP
// or TEMPORARY) in a schema statement, skipping whitespace and comments: TABLE,
// VIRTUAL, VIEW, TRIGGER, INDEX or UNIQUE. It returns "" for anything else.
func createKeyword(stmt string) string {
	rest := stmt
	word := func() string {
		for {
			rest = strings.TrimLeft(rest, " \t\n\r\f\v")
			switch {
			case strings.HasPrefix(rest, "--"):
				i := strings.IndexByte(rest, '\n')
				if i < 0 {
					return ""
				}
				rest = rest[i+1:]
			case strings.HasPrefix(rest, "/*"):
				i := strings.Index(rest[2:], "*/")
				if i < 0 {
					return ""
				}
				rest = rest[2+i+2:]
			default:
				i := strings.IndexFunc(rest, func(r rune) bool {
					return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z')
				})
				if i < 0 {
					i = len(rest)
				}
				w := strings.ToUpper(rest[:i])
				rest = rest[i:]
				return w
			}
		}
	}
	if word() != "CREATE" {
		return ""
	}
	w := word()
	if w == "TEMP" || w == "TEMPORARY" {
		w = word()
	}
	return w
}
