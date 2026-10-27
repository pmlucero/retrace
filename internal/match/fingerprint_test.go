package match

import "testing"

// ADR 0007 §1: a fingerprint is the hex SHA-256 of the canonical form.
func TestFingerprint(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{"abc", "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
	}
	for _, tt := range tests {
		if got := Fingerprint([]byte(tt.in)); got != tt.want {
			t.Errorf("Fingerprint(%q) = %s, want %s", tt.in, got, tt.want)
		}
	}
}
