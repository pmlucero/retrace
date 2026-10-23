// Package cassette stores one retrace session per SQLite file (spec §7,
// ADR 0005), as an append-only log with a raw and a semantic layer (ADR 0009).
//
// Writes go through Write, which runs a function in a single transaction, so
// callers can batch many appends. Reads are methods on Cassette.
package cassette

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	_ "modernc.org/sqlite" // registers the "sqlite" driver (pure Go, ADR 0001)
)

var (
	// ErrExists is returned by Create when the file already exists.
	ErrExists = errors.New("cassette already exists")
	// ErrNewerSchema is returned when writing to a cassette whose schema is
	// newer than this build supports (spec §7.6).
	ErrNewerSchema = errors.New("cassette schema is newer than this version of retrace supports")
	// ErrAlreadyCompleted is returned when completing an interaction twice (ADR 0009).
	ErrAlreadyCompleted = errors.New("interaction already completed")
	// ErrNotFound is returned when a referenced row does not exist.
	ErrNotFound = errors.New("not found")
	// ErrNotCassette is returned by Open for a file that was never a cassette:
	// schema version 0, such as a foreign SQLite database or an empty file.
	ErrNotCassette = errors.New("not a retrace cassette")
)

// Cassette is an open cassette file.
type Cassette struct {
	db       *sql.DB
	path     string
	schema   int  // schema version of the file
	walMode  bool // the file is in WAL mode: Close switches it back to a single file
	writable bool // false when the file's schema is newer than this build's
}

// Create creates a new cassette at path with the current schema. The file must
// not exist. The cassette uses WAL journaling while open, and Close turns it
// back into a single self-contained file (ADR 0005).
func Create(ctx context.Context, path string) (*Cassette, error) {
	// The caller chooses the path. Session ids are validated where they enter from
	// the user (record --session, M2), which is what prevents path traversal.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: see above
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("creating %s: %w", path, ErrExists)
		}
		return nil, fmt.Errorf("creating %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("creating %s: %w", path, err)
	}

	c, err := open(ctx, path)
	if err != nil {
		return nil, err
	}
	if _, err := c.db.ExecContext(ctx, "PRAGMA journal_mode = WAL"); err != nil {
		_ = c.db.Close()
		return nil, fmt.Errorf("enabling WAL on %s: %w", path, err)
	}
	c.walMode = true
	if err := c.migrate(ctx, migrations); err != nil {
		_ = c.db.Close()
		return nil, err
	}
	return c, nil
}

// Open opens an existing cassette. Older schemas are migrated forward (spec
// §7.6). A cassette with a newer schema can be read, but writes to it fail
// with ErrNewerSchema. A file with schema version 0 is not a cassette (v1 is
// the initial schema) and is rejected with ErrNotCassette, untouched.
func Open(ctx context.Context, path string) (*Cassette, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	c, err := open(ctx, path)
	if err != nil {
		return nil, err
	}
	if c.schema == 0 {
		_ = c.db.Close()
		return nil, fmt.Errorf("opening %s: %w", path, ErrNotCassette)
	}
	if c.schema > len(migrations) {
		c.writable = false
		return c, nil
	}
	// A recording that crashed leaves the file in WAL mode, which persists.
	// Let Close return it to a single file (ADR 0005).
	var journal string
	if err := c.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
		_ = c.db.Close()
		return nil, fmt.Errorf("reading journal mode of %s: %w", path, err)
	}
	c.walMode = strings.EqualFold(journal, "wal")
	if err := c.migrate(ctx, migrations); err != nil {
		_ = c.db.Close()
		return nil, err
	}
	return c, nil
}

func open(ctx context.Context, path string) (*Cassette, error) {
	if strings.ContainsRune(path, '?') {
		// The driver reads connection options after '?'.
		return nil, fmt.Errorf("opening %s: path must not contain '?'", path)
	}
	// trusted_schema(0): cassettes are untrusted input (SECURITY.md, Scope), so
	// views, triggers and defaults in the file may not call application functions.
	dsn := path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=trusted_schema(0)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	// One connection: a cassette has a single writer, and SQLite serializes
	// writes anyway. It also keeps per-connection pragmas in effect.
	db.SetMaxOpenConns(1)

	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("reading schema version of %s: %w", path, err)
	}
	return &Cassette{db: db, path: path, schema: version, writable: true}, nil
}

// SchemaVersion returns the schema version of the open file.
func (c *Cassette) SchemaVersion() int { return c.schema }

// Path returns the file path of the cassette.
func (c *Cassette) Path() string { return c.path }

// Close closes the cassette. A cassette in WAL mode (created by Create, or
// left by a crashed recording) is checkpointed and switched back to a single
// file, so no -wal or -shm files remain next to it.
//
// It takes no context: closing always runs to the end, so the file is left
// valid even when the caller is shutting down.
//
// nolint: contextcheck
func (c *Cassette) Close() error {
	var errs []error
	if c.walMode {
		ctx := context.Background()
		if _, err := c.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			errs = append(errs, fmt.Errorf("checkpointing %s: %w", c.path, err))
		}
		if _, err := c.db.ExecContext(ctx, "PRAGMA journal_mode = DELETE"); err != nil {
			errs = append(errs, fmt.Errorf("leaving WAL mode on %s: %w", c.path, err))
		}
	}
	if err := c.db.Close(); err != nil {
		errs = append(errs, fmt.Errorf("closing %s: %w", c.path, err))
	}
	return errors.Join(errs...)
}

// Write runs fn in a single transaction. If fn returns an error, nothing it
// wrote is kept.
func (c *Cassette) Write(ctx context.Context, fn func(*Tx) error) error {
	if !c.writable {
		return fmt.Errorf("writing %s (schema v%d, supported v%d): %w", c.path, c.schema, len(migrations), ErrNewerSchema)
	}
	sqlTx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction on %s: %w", c.path, err)
	}
	if err := fn(&Tx{tx: sqlTx}); err != nil {
		return errors.Join(err, sqlTx.Rollback())
	}
	if err := sqlTx.Commit(); err != nil {
		return fmt.Errorf("committing to %s: %w", c.path, err)
	}
	return nil
}
