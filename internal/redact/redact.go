// Package redact removes secrets before anything is written to a cassette
// (ADR 0008).
package redact

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"slices"
	"strings"
)

// DefaultHeaders are always redacted, compared case-insensitively. Header
// names ending in any of DefaultSuffixes are redacted too (ADR 0008).
var (
	DefaultHeaders  = []string{"authorization", "x-api-key", "proxy-authorization", "cookie", "set-cookie"}
	DefaultSuffixes = []string{"-token", "-secret", "-key"}
)

// Headers redacts header values by name.
type Headers struct {
	names map[string]bool // lower-case
}

// NewHeaders returns a Headers that redacts the defaults plus extra names
// (--redact-header).
func NewHeaders(extra []string) *Headers {
	h := &Headers{names: map[string]bool{}}
	for _, n := range slices.Concat(DefaultHeaders, extra) {
		h.names[strings.ToLower(n)] = true
	}
	return h
}

// Names returns the redacted header names and suffix patterns, sorted, for the
// cassette's redaction metadata (spec §7.2).
func (h *Headers) Names() []string {
	out := make([]string, 0, len(h.names)+len(DefaultSuffixes))
	for n := range h.names {
		out = append(out, n)
	}
	for _, s := range DefaultSuffixes {
		out = append(out, "*"+s)
	}
	slices.Sort(out)
	return out
}

// Redacts reports whether the header called name is redacted.
func (h *Headers) Redacts(name string) bool {
	name = strings.ToLower(name)
	if h.names[name] {
		return true
	}
	for _, s := range DefaultSuffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

// Apply returns a copy of hdr with every redacted value replaced by
// [REDACTED:<first 8 hex digits of its SHA-256>]. The hash lets two sessions
// be compared for using the same key without revealing it. hdr is unchanged:
// the upstream still needs the real values.
func (h *Headers) Apply(hdr http.Header) http.Header {
	out := make(http.Header, len(hdr))
	for name, values := range hdr {
		if !h.Redacts(name) {
			out[name] = slices.Clone(values)
			continue
		}
		red := make([]string, len(values))
		for i, v := range values {
			sum := sha256.Sum256([]byte(v))
			red[i] = "[REDACTED:" + hex.EncodeToString(sum[:4]) + "]"
		}
		out[name] = red
	}
	return out
}
