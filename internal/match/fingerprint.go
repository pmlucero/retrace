package match

import (
	"crypto/sha256"
	"encoding/hex"
)

// Fingerprint returns the matching fingerprint of a request's canonical form:
// its SHA-256 in lower-case hex (ADR 0007 §1).
func Fingerprint(canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}
