package cassette

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// AC-24 (validation part; the CLI test checks the exit code and that no file is created)
func TestValidateSessionID(t *testing.T) {
	tests := []struct {
		id    string
		valid bool
	}{
		{"checkout", true},
		{"20261023-120000-ab12", true},
		{"checkout.swap-model.1", true},
		{"A_b-9", true},
		{strings.Repeat("a", 128), true},
		{"console", true}, // only exact device names are reserved
		{"", false},
		{"../x", false},
		{"..", false},
		{".hidden", false},
		{"a/b", false},
		{`a\b`, false},
		{"-flag", false},
		{"with space", false},
		{"ñ", false},
		{strings.Repeat("a", 129), false},
		{"NUL", false},
		{"nul.swap-model.1", false},
		{"Com1", false},
		{"lpt9.x", false},
	}
	for _, tt := range tests {
		err := ValidateSessionID(tt.id)
		if (err == nil) != tt.valid {
			t.Errorf("ValidateSessionID(%q) = %v, want valid=%v", tt.id, err, tt.valid)
		}
		if err != nil && !errors.Is(err, ErrInvalidSessionID) {
			t.Errorf("ValidateSessionID(%q) = %v, want ErrInvalidSessionID", tt.id, err)
		}
	}
}

func TestNewSessionIDFormatAndCollision(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 23, 12, 34, 56, 0, time.Local)

	id, err := NewSessionID(dir, now, bytes.NewReader([]byte{0xab, 0x12}))
	if err != nil || id != "20261023-123456-ab12" {
		t.Fatalf("NewSessionID = %q, %v; want 20261023-123456-ab12", id, err)
	}
	if err := ValidateSessionID(id); err != nil {
		t.Errorf("generated id is invalid: %v", err)
	}

	// The first candidate exists, so a new suffix is drawn (spec §9.2).
	if err := os.WriteFile(Path(dir, id), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	id, err = NewSessionID(dir, now, bytes.NewReader([]byte{0xab, 0x12, 0xcd, 0x34}))
	if err != nil || id != "20261023-123456-cd34" {
		t.Errorf("NewSessionID after a collision = %q, %v; want 20261023-123456-cd34", id, err)
	}
}
