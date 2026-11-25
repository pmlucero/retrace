package cassette

import (
	"context"
	"errors"
	"testing"
)

func foreignKeysOn(t *testing.T, c *Cassette) bool {
	t.Helper()
	var on int
	if err := c.db.QueryRowContext(context.Background(), `PRAGMA foreign_keys`).Scan(&on); err != nil {
		t.Fatal(err)
	}
	return on == 1
}

func replaceWith(ctx context.Context, events []Event) func(*Tx) error {
	return func(tx *Tx) error {
		if err := tx.DeleteEvents(ctx); err != nil {
			return err
		}
		for _, e := range events {
			if err := tx.AppendEvent(ctx, e); err != nil {
				return err
			}
		}
		return nil
	}
}

func TestReplaceEventsRestoresForeignKeysAfterSuccess(t *testing.T) {
	ctx := context.Background()
	c, _ := newCassette(t)
	write(t, c, func(tx *Tx) error { return tx.InsertSession(ctx, testSession()) })
	if err := c.ReplaceEvents(ctx, replaceWith(ctx, chainEvents(50))); err != nil {
		t.Fatal(err)
	}
	if !foreignKeysOn(t, c) {
		t.Fatal("foreign keys are off after a successful ReplaceEvents")
	}
	got, err := c.Events(ctx)
	if err != nil || len(got) != 50 {
		t.Fatalf("Events = %d, %v; want 50", len(got), err)
	}
}

func TestReplaceEventsRestoresForeignKeysAfterFnError(t *testing.T) {
	ctx := context.Background()
	c, _ := newCassette(t)
	write(t, c, func(tx *Tx) error { return tx.InsertSession(ctx, testSession()) })
	boom := errors.New("boom")
	if err := c.ReplaceEvents(ctx, func(*Tx) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if !foreignKeysOn(t, c) {
		t.Fatal("foreign keys are off after a failed ReplaceEvents")
	}
}

func TestReplaceEventsRejectsDanglingParentAndKeepsEvents(t *testing.T) {
	ctx := context.Background()
	c, _ := newCassette(t)
	write(t, c, func(tx *Tx) error { return tx.InsertSession(ctx, testSession()) })
	if err := c.ReplaceEvents(ctx, replaceWith(ctx, chainEvents(5))); err != nil {
		t.Fatal(err)
	}

	bad := chainEvents(3)
	missing := int64(99)
	bad[2].ParentSeq = &missing
	err := c.ReplaceEvents(ctx, replaceWith(ctx, bad))
	if !errors.Is(err, ErrForeignKey) {
		t.Fatalf("err = %v, want ErrForeignKey", err)
	}
	if !foreignKeysOn(t, c) {
		t.Fatal("foreign keys are off after a rejected ReplaceEvents")
	}
	got, err := c.Events(ctx)
	if err != nil || len(got) != 5 {
		t.Fatalf("Events = %d, %v; want the original 5", len(got), err)
	}
}

func TestReplaceEventsRejectsDanglingInteraction(t *testing.T) {
	ctx := context.Background()
	c, _ := newCassette(t)
	write(t, c, func(tx *Tx) error { return tx.InsertSession(ctx, testSession()) })
	bad := chainEvents(1)
	missing := int64(7)
	bad[0].InteractionID = &missing
	if err := c.ReplaceEvents(ctx, replaceWith(ctx, bad)); !errors.Is(err, ErrForeignKey) {
		t.Fatalf("err = %v, want ErrForeignKey", err)
	}
}

// A failed restore must not leave a connection with foreign keys off in the pool.
func TestRestoreForeignKeysFailureDiscardsConnection(t *testing.T) {
	ctx := context.Background()
	c, _ := newCassette(t)
	conn, err := c.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := restoreForeignKeys(canceled, conn); err == nil {
		t.Fatal("restoreForeignKeys succeeded with a canceled context")
	}
	if err := closeConn(conn); err != nil {
		t.Fatal(err)
	}
	if !foreignKeysOn(t, c) {
		t.Fatal("the pool kept a connection with foreign keys off")
	}
}

func TestReplaceEventsRefusesReadOnly(t *testing.T) {
	ctx := context.Background()
	c, path := newCassette(t)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := OpenForRead(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	if err := r.ReplaceEvents(ctx, func(*Tx) error { return nil }); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("err = %v, want ErrReadOnly", err)
	}
}
