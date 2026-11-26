package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pmlucero/retrace/internal/cassette"
)

func TestNextReplayID(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a[1]") // not a pattern
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if id, err := NextID(dir, "s", "exact"); err != nil || id != "s.exact.1" {
		t.Errorf("first replay id %q, %v; want s.exact.1", id, err)
	}
	for _, f := range []string{"s.exact.1", "s.exact.7", "s.exact.x", "s.exact.+9", "s.exact.09", "s.exact.9223372036854775807", "s.exact.99999999999999999999", "s.swap-model.9", "12"} {
		if err := os.WriteFile(cassette.Path(dir, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if id, err := NextID(dir, "s", "exact"); err != nil || id != "s.exact.8" {
		t.Errorf("replay id %q, %v; want s.exact.8", id, err)
	}
	long := strings.Repeat("a", 125)
	if _, err := NextID(dir, long, "exact"); err == nil || !strings.Contains(err.Error(), "--as") {
		t.Errorf("replay id of a 125-character id: %v, want an error asking for --as", err)
	}
	// spec §9.3: a default n may not reach 2^63 - 1.
	if err := os.WriteFile(cassette.Path(dir, "b.exact.9223372036854775806"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NextID(dir, "b", "exact"); err == nil || !strings.Contains(err.Error(), "--as") {
		t.Errorf("replay id after n = 2^63 - 2: %v, want an error asking for --as", err)
	}
}
