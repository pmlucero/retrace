package cassette

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
)

// ErrForeignKey is returned when a ReplaceEvents write leaves a row that
// references a missing row.
var ErrForeignKey = errors.New("foreign key violation")

// ReplaceEvents runs fn in a single transaction like Write, with foreign key
// enforcement off for the duration, and checks every foreign key before it
// commits. Only `retrace reindex` uses it, to delete and re-insert all events.
//
// Why: events.parent_seq references events(seq) and has no index. With
// enforcement on, every deleted event scans the whole table for children, so
// replacing n events costs O(n²). Adding the index would change the schema
// (a migration and a user_version bump), which older retraces refuse to write
// (spec §7.6). Instead, the check runs once, in linear time, at the end: the
// result is the same as enforcing per statement, because a violation anywhere
// fails the whole write and nothing is kept.
//
// PRAGMA foreign_keys is a no-op inside a transaction, so it is set on the
// cassette's only connection before the transaction begins, and restored
// afterwards on every path. If it cannot be restored, the connection is
// discarded so the pool opens a fresh one with enforcement on (the DSN sets
// it), and an error is returned.
func (c *Cassette) ReplaceEvents(ctx context.Context, fn func(*Tx) error) (err error) {
	if c.readOnly {
		return fmt.Errorf("writing %s: %w", c.path, ErrReadOnly)
	}
	if !c.writable {
		return fmt.Errorf("writing %s (schema v%d, supported v%d): %w", c.path, c.schema, len(migrations), ErrNewerSchema)
	}
	conn, err := c.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("opening connection to %s: %w", c.path, err)
	}
	defer func() {
		err = errors.Join(err, closeConn(conn))
	}()

	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("disabling foreign keys on %s: %w", c.path, err)
	}
	defer func() {
		// The write may have failed because ctx was canceled: restore anyway.
		if rerr := restoreForeignKeys(context.WithoutCancel(ctx), conn); rerr != nil {
			err = errors.Join(err, fmt.Errorf("restoring foreign keys on %s: %w", c.path, rerr))
		}
	}()

	sqlTx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction on %s: %w", c.path, err)
	}
	if err := fn(&Tx{tx: sqlTx}); err != nil {
		return errors.Join(err, sqlTx.Rollback())
	}
	if err := checkNoDanglingRows(ctx, sqlTx); err != nil {
		return errors.Join(fmt.Errorf("checking %s: %w", c.path, err), sqlTx.Rollback())
	}
	if err := sqlTx.Commit(); err != nil {
		return fmt.Errorf("committing to %s: %w", c.path, err)
	}
	return nil
}

// checkNoDanglingRows fails if any row references a missing row.
func checkNoDanglingRows(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("running foreign_key_check: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		var table string
		var rowid sql.NullInt64
		var parent string
		var fkid int
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return fmt.Errorf("reading foreign_key_check: %w", err)
		}
		return fmt.Errorf("%s row %d references a missing %s row: %w", table, rowid.Int64, parent, ErrForeignKey)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("running foreign_key_check: %w", err)
	}
	return nil
}

// restoreForeignKeys turns enforcement back on and verifies it. On failure it
// marks the connection bad so the pool discards it.
func restoreForeignKeys(ctx context.Context, conn *sql.Conn) error {
	var on int
	err := func() error {
		if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
			return err
		}
		if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&on); err != nil {
			return err
		}
		if on != 1 {
			return errors.New("foreign keys still off")
		}
		return nil
	}()
	if err != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	}
	return err
}

// closeConn returns the connection to the pool. Closing a connection that
// Raw already discarded reports sql.ErrConnDone, which is not an error here.
func closeConn(conn *sql.Conn) error {
	if err := conn.Close(); err != nil && !errors.Is(err, sql.ErrConnDone) {
		return fmt.Errorf("releasing connection: %w", err)
	}
	return nil
}
