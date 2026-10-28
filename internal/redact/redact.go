// Package redact removes secrets before anything is written to a cassette
// (ADR 0008, spec §3.5).
package redact

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// DefaultHeaders are always redacted, compared case-insensitively. Header
// names ending in any of DefaultSuffixes are redacted too (ADR 0008).
var (
	DefaultHeaders  = []string{"authorization", "x-api-key", "proxy-authorization", "cookie", "set-cookie"}
	DefaultSuffixes = []string{"-token", "-secret", "-key"}
)

// queryNames are query parameter names that carry credentials although no
// header has that name (spec §3.5).
var queryNames = map[string]bool{
	"key": true, "token": true, "secret": true, "password": true,
	"apikey": true, "auth": true, "sig": true, "signature": true,
}

// Rules decides what is redacted.
type Rules struct {
	headers map[string]bool // lower-case
	body    []Path
}

// NewRules returns the default header rules plus extra header names
// (--redact-header) and body rules (--redact).
func NewRules(extraHeaders []string, body []Path) *Rules {
	r := &Rules{headers: map[string]bool{}, body: body}
	for _, n := range slices.Concat(DefaultHeaders, extraHeaders) {
		r.headers[strings.ToLower(n)] = true
	}
	return r
}

// HeaderNames returns the redacted header names and suffix patterns, sorted,
// for the cassette's redaction metadata (spec §7.2).
func (r *Rules) HeaderNames() []string {
	out := make([]string, 0, len(r.headers)+len(DefaultSuffixes))
	for n := range r.headers {
		out = append(out, n)
	}
	for _, s := range DefaultSuffixes {
		out = append(out, "*"+s)
	}
	slices.Sort(out)
	return out
}

// metadata is the session's redaction metadata (spec §7.2). Body comes
// first, as it did when record wrote a map.
type metadata struct {
	Body    []string `json:"body"`
	Headers []string `json:"headers"`
}

// Metadata returns the rules as stored in the session's redaction metadata
// (spec §7.2). RulesFromMetadata reads them back.
func (r *Rules) Metadata() json.RawMessage {
	m, _ := json.Marshal(metadata{Body: r.BodyRules(), Headers: r.HeaderNames()}) // slices of strings always marshal
	return m
}

// RulesFromMetadata rebuilds the rules that Metadata stored, so a replay
// redacts requests as the recording did before fingerprinting them
// (spec §3.5, §5). stored comes from a cassette, which is untrusted input.
func RulesFromMetadata(stored json.RawMessage) (*Rules, error) {
	var m metadata
	if err := json.Unmarshal(stored, &m); err != nil {
		return nil, fmt.Errorf("reading redaction rules: %w", err)
	}
	var headers []string
	for _, h := range m.Headers {
		// The suffix patterns are always applied.
		if suffix, ok := strings.CutPrefix(h, "*"); !ok || !slices.Contains(DefaultSuffixes, suffix) {
			headers = append(headers, h)
		}
	}
	body := make([]Path, len(m.Body))
	for i, raw := range m.Body {
		p, err := ParsePath(raw)
		if err != nil {
			return nil, fmt.Errorf("reading redaction rules: %w", err)
		}
		body[i] = p
	}
	return NewRules(headers, body), nil
}

// RedactsHeader reports whether the header called name is redacted.
func (r *Rules) RedactsHeader(name string) bool {
	name = strings.ToLower(name)
	if r.headers[name] {
		return true
	}
	for _, s := range DefaultSuffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

// Headers returns a copy of hdr with every redacted value replaced by
// [REDACTED:<first 8 hex digits of its SHA-256>]. The hash lets two sessions
// be compared for using the same key without revealing it. hdr is unchanged:
// the upstream still needs the real values.
func (r *Rules) Headers(hdr http.Header) http.Header {
	out := make(http.Header, len(hdr))
	for name, values := range hdr {
		if !r.RedactsHeader(name) {
			out[name] = slices.Clone(values)
			continue
		}
		red := make([]string, len(values))
		for i, v := range values {
			red[i] = mask(v)
		}
		out[name] = red
	}
	return out
}

// Query returns rawQuery with the values of credential parameters masked,
// keeping everything else byte for byte.
func (r *Rules) Query(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	parts := strings.Split(rawQuery, "&")
	for i, part := range parts {
		name, value, hasValue := strings.Cut(part, "=")
		key, err := url.QueryUnescape(name)
		if err != nil {
			key = name
		}
		if hasValue && r.redactsQuery(key) {
			parts[i] = name + "=" + mask(value)
		}
	}
	return strings.Join(parts, "&")
}

func (r *Rules) redactsQuery(name string) bool {
	name = strings.ReplaceAll(strings.ToLower(name), "_", "-")
	return queryNames[name] || r.RedactsHeader(name)
}

// URL returns u as a string with its userinfo replaced by REDACTED and its
// credential query parameters masked.
func (r *Rules) URL(u *url.URL) string {
	c := *u
	if c.User != nil {
		c.User = url.User("REDACTED")
	}
	c.RawQuery = r.Query(c.RawQuery)
	return c.String()
}

func mask(v string) string {
	sum := sha256.Sum256([]byte(v))
	return "[REDACTED:" + hex.EncodeToString(sum[:4]) + "]"
}
