package session

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/pmlucero/retrace/internal/cassette"
)

// errInsertFailed is what failingInsert returns.
var errInsertFailed = errors.New("disk full (injected)")

// failingInsert makes the session insert fail, as a full disk or a locked
// database would.
func failingInsert(context.Context, *cassette.Tx, cassette.Session) error { return errInsertFailed }

// assertDirHolds fails the test unless dir holds exactly the named files: no
// other cassette and no -wal or -shm file.
func assertDirHolds(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if !slices.Equal(got, want) {
		t.Errorf("%s holds %v, want %v", dir, got, want)
	}
}

// #56: a cassette whose session row cannot be written is removed.
func TestCreateCassetteRemovesTheFileWhenTheInsertFails(t *testing.T) {
	dir := t.TempDir()
	path := cassette.Path(dir, "s")
	_, err := CreateCassette(context.Background(), path, cassette.Session{ID: "s", Mode: "record", Status: "recording", OnMiss: "strict"}, failingInsert)
	if !errors.Is(err, errInsertFailed) {
		t.Fatalf("err = %v, want the insert's error", err)
	}
	assertDirHolds(t, dir)
}

// The cassette of another session at the same path is never removed.
func TestCreateCassetteKeepsAnExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := cassette.Path(dir, "s")
	if err := os.WriteFile(path, []byte("another session"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateCassette(context.Background(), path, cassette.Session{ID: "s"}, failingInsert); !errors.Is(err, cassette.ErrExists) {
		t.Fatalf("err = %v, want ErrExists", err)
	}
	data, _ := os.ReadFile(path) //nolint:gosec // G304: the file this test created
	if string(data) != "another session" {
		t.Errorf("existing file now holds %q", data)
	}
}

// #111: a replacement whose session row cannot be written keeps the old
// cassette.
func TestReplaceCassetteKeepsTheOldOneWhenTheInsertFails(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := cassette.Path(dir, "s")
	c, err := CreateCassette(ctx, path, cassette.Session{ID: "s", Mode: "record", Status: "recording", OnMiss: "strict"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path) //nolint:gosec // G304: the test's own cassette
	if _, err := ReplaceCassette(ctx, path, cassette.Session{ID: "s"}, failingInsert); !errors.Is(err, errInsertFailed) {
		t.Fatalf("err = %v, want the insert's error", err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) { //nolint:gosec // G304: as above
		t.Error("the old cassette changed")
	}
	assertDirHolds(t, dir, "s.cassette")
}

// spec §9.3: a retry that would leave the id format or reach 2^63 - 1 asks
// for --as.
func TestCreateReplayCassetteGivesUp(t *testing.T) {
	ctx := context.Background()
	for _, id := range []string{"s.exact.9223372036854775806", strings.Repeat("a", 120) + ".exact.9"} {
		dir := t.TempDir()
		if err := os.WriteFile(cassette.Path(dir, id), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, err := createReplayCassette(ctx, dir, id, false, cassette.Session{Mode: "exact", Status: "recording", OnMiss: "strict"}, nil)
		if err == nil || !strings.Contains(err.Error(), "--as") || !strings.Contains(err.Error(), "after "+id+";") {
			t.Errorf("%s: %v, want an error asking for --as", id, err)
		}
		if files, _ := os.ReadDir(dir); len(files) != 1 {
			t.Errorf("%s: dir holds %d files, want only the existing one", id, len(files))
		}
	}
}

func TestCreateReplayCassetteTakesTheNextFreeDefaultID(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(cassette.Path(dir, "s.exact.1"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	c, id, err := createReplayCassette(ctx, dir, "s.exact.1", false, cassette.Session{Mode: "exact", Status: "recording", OnMiss: "strict"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if id != "s.exact.2" {
		t.Errorf("id %q, want s.exact.2", id)
	}
}
