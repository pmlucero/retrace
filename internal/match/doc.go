// Package match decides which recorded interaction answers an incoming request
// during replay (ADR 0007, spec §5).
//
// A request is identified by its fingerprint: the SHA-256 of its canonical
// form. Canonicalization is defined once per provider or transport, next to
// the code that understands its requests (for the LLM channel,
// anthropic.Canonicalize). This package only hashes the result and keeps the
// recorded interactions in one FIFO queue per fingerprint, so identical
// requests are answered in recorded order and unrelated requests may arrive
// in any order.
package match
