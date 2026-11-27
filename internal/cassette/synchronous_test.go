package cassette

import (
	"context"
	"path/filepath"
	"testing"
)

const (
	syncNormal = 1
	syncFull   = 2
)

func synchronousOf(ctx context.Context, t *testing.T, c *Cassette) int {
	t.Helper()
	var v int
	if err := c.db.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&v); err != nil {
		t.Fatalf("reading synchronous: %v", err)
	}
	return v
}

// Issue #193: NORMAL only while the connection this process put in WAL mode is
// open; every other connection keeps the default FULL.
func TestSynchronousByOpenPath(t *testing.T) {
	ctx := context.Background()
	closed := filepath.Join(t.TempDir(), "closed.cassette")
	c, err := Create(ctx, closed)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		open func() (*Cassette, error)
		want int
	}{
		{"Create", func() (*Cassette, error) {
			return Create(ctx, filepath.Join(t.TempDir(), "a.cassette"))
		}, syncNormal},
		{"Replace", func() (*Cassette, error) {
			return Replace(ctx, filepath.Join(t.TempDir(), "b.cassette"), func(*Tx) error { return nil })
		}, syncNormal},
		{"Open of a closed cassette", func() (*Cassette, error) { return Open(ctx, closed) }, syncFull},
		{"OpenForRead of a closed cassette", func() (*Cassette, error) { return OpenForRead(ctx, closed) }, syncFull},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := tt.open()
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if got := synchronousOf(ctx, t, c); got != tt.want {
				t.Errorf("synchronous = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCloseRestoresFullSynchronousBeforeCheckpoint(t *testing.T) {
	ctx := context.Background()
	c, err := Create(ctx, filepath.Join(t.TempDir(), "c.cassette"))
	if err != nil {
		t.Fatal(err)
	}
	got := -1
	c.beforeCheckpoint = func(ctx context.Context) { got = synchronousOf(ctx, t, c) }
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if got != syncFull {
		t.Errorf("synchronous before checkpoint = %d, want %d", got, syncFull)
	}
}
