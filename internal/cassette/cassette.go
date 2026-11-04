// Package cassette stores one retrace session per SQLite file (spec §7,
// ADR 0005), as an append-only log with a raw and a semantic layer (ADR 0009).
//
// Writes go through Write, which runs a function in a single transaction, so
// callers can batch many appends. Reads are methods on Cassette.
package cassette

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	// ErrReadOnly is returned when writing to a cassette opened with
	// OpenForRead.
	ErrReadOnly = errors.New("cassette was opened for reading")
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
	readOnly bool // opened with OpenForRead
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

	// The file is new and ours: a failure from here on removes it, so the
	// path stays free and no half-made cassette is left behind.
	c, err := open(ctx, path)
	if err != nil {
		return nil, errors.Join(err, Remove(path))
	}
	if err := c.enableWAL(ctx); err != nil {
		_ = c.db.Close()
		return nil, errors.Join(err, Remove(path))
	}
	if err := c.migrate(ctx, migrations); err != nil {
		_ = c.db.Close()
		return nil, errors.Join(err, Remove(path))
	}
	return c, nil
}

// Replace creates a cassette at path like Create, replacing the cassette
// there, if any. The old cassette stays untouched until the new one is
// complete (#111): Replace creates the new one under a temporary name in the
// same directory, runs init on it in one transaction, closes it into a single
// file and only then renames it over path. On failure up to that point the
// temporary file is removed and the old cassette is kept. After the rename,
// the old cassette's -wal, -shm and -journal files are removed, because
// SQLite would apply a stale log to the new file. The new cassette is then
// reopened in WAL mode, as Create leaves it. A failure after the rename
// removes the new cassette as well.
//
// Two crash windows are accepted: a process killed between the rename and
// the removal of the old logs leaves them next to the new file, and one
// killed before the rename leaves the hidden temporary file behind, which no
// command reads.
//
// The old cassette must not be open. On Windows the rename fails while
// another process has it open, and the old cassette is kept.
func Replace(ctx context.Context, path string, init func(*Tx) error) (*Cassette, error) {
	tmp := tempPath(path)
	c, err := Create(ctx, tmp)
	if errors.Is(err, ErrExists) {
		// Not the caller's ErrExists: that one is about path.
		return nil, fmt.Errorf("creating %s: temporary file already exists", tmp)
	}
	if err != nil {
		return nil, err
	}
	if err := c.Write(ctx, init); err != nil {
		return nil, errors.Join(err, c.Close(), Remove(tmp))
	}
	if err := c.Close(); err != nil {
		return nil, errors.Join(err, Remove(tmp))
	}
	// os.Rename replaces path atomically on POSIX, and through MoveFileEx
	// with MOVEFILE_REPLACE_EXISTING on Windows.
	if err := os.Rename(tmp, path); err != nil {
		return nil, errors.Join(fmt.Errorf("replacing %s: %w", path, err), Remove(tmp))
	}
	// The old cassette is gone from here on. A failure removes the new one
	// too: a file next to a stale log, or a session that never started, is
	// worse than a free id.
	if err := removeFiles(sidecars(path)); err != nil {
		return nil, errors.Join(err, Remove(path))
	}
	if c, err = Open(ctx, path); err != nil {
		return nil, errors.Join(err, Remove(path))
	}
	if err := c.enableWAL(ctx); err != nil {
		return nil, errors.Join(err, c.db.Close(), Remove(path))
	}
	return c, nil
}

// sidecars returns the files SQLite keeps next to the database at path. It
// treats a -wal or a hot -journal there as the database's own, and applies
// it on the next open.
func sidecars(path string) []string {
	return []string{path + "-wal", path + "-shm", path + "-journal"}
}

// tempPath returns a new name next to path for a cassette being built. It
// starts with a dot and does not end in .cassette, so commands that list
// cassettes ignore it, and its random part keeps it from colliding.
func tempPath(path string) string {
	dir, base := filepath.Split(path)
	return filepath.Join(dir, "."+base+"."+rand.Text()+".tmp")
}

// enableWAL switches the open file to WAL journaling, which Close undoes.
func (c *Cassette) enableWAL(ctx context.Context) error {
	if _, err := c.db.ExecContext(ctx, "PRAGMA journal_mode = WAL"); err != nil {
		return fmt.Errorf("enabling WAL on %s: %w", c.path, err)
	}
	c.walMode = true
	return nil
}

// Remove deletes the cassette file at path and its -wal, -shm and -journal
// files. A file that does not exist is not an error. The cassette must be
// closed.
func Remove(path string) error {
	return removeFiles(append([]string{path}, sidecars(path)...))
}

// removeFiles deletes the files at paths. A file that does not exist is not
// an error.
func removeFiles(paths []string) error {
	var errs []error
	for _, p := range paths {
		// The caller chooses the path, as in Create: session ids are validated
		// where they enter from the user.
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) { //nolint:gosec // G703: see above
			errs = append(errs, fmt.Errorf("removing %s: %w", p, err))
		}
	}
	return errors.Join(errs...)
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

// OpenForRead opens an existing cassette like Open, for commands that only
// read it, such as ls and show. Writes to it fail, and Close leaves the
// journal as it is: the cassette may be a session still being recorded, and
// switching it out of WAL mode would checkpoint the whole log, which blocks
// the recorder's writes until it is done (ADR 0009: a writer that falls
// behind drops records). A crashed recording keeps its -wal file until a
// command that writes, such as reindex or run, opens it.
func OpenForRead(ctx context.Context, path string) (*Cassette, error) {
	c, err := Open(ctx, path)
	if err != nil {
		return nil, err
	}
	c.walMode, c.readOnly = false, true
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
	if c.readOnly {
		return fmt.Errorf("writing %s: %w", c.path, ErrReadOnly)
	}
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
