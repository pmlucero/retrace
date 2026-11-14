// Package redact removes secrets before anything is written to a cassette
// (ADR 0011, spec §3.5).
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

// DefaultHeaders are always redacted, compared case-insensitively. Names
// ending in any of DefaultPatterns (written without their leading "*") are
// redacted too. One list covers header names and query parameter names
// (ADR 0011, spec §3.5).
var (
	DefaultHeaders = []string{
		"authorization", "proxy-authorization", "x-api-key", "cookie", "set-cookie",
		"apikey", "auth", "key", "password", "secret", "sig", "signature", "token",
	}
	DefaultPatterns = []string{
		"*-token", "*-secret", "*-key", "*-auth", "*-password", "*-signature", "*-credential", "*apikey",
	}
)

// urlHeaders carry URLs in their values, which get the URL rule of spec §3.5
// (ADR 0011). Link, which holds URI references inside <…>, is handled apart.
var urlHeaders = map[string]bool{"location": true, "content-location": true, "referer": true}

// fragmentSeparators split a fragment into elements, for Fragment and
// HasMarker alike (spec §3.5).
const fragmentSeparators = "&?"

// Rules decides what is redacted.
type Rules struct {
	headers  map[string]bool // lower-case
	suffixes map[string]bool // the patterns without their "*", lower-case
	lengths  []int           // the distinct lengths of suffixes, ascending
	body     []Path
}

// NewRules returns the default header rules plus extra header names
// (--redact-header) and body rules (--redact).
func NewRules(extraHeaders []string, body []Path) *Rules {
	r := &Rules{headers: map[string]bool{}, suffixes: map[string]bool{}, body: body}
	for _, n := range slices.Concat(DefaultHeaders, extraHeaders) {
		r.headers[strings.ToLower(n)] = true
	}
	for _, p := range DefaultPatterns {
		r.addPattern(p)
	}
	return r
}

// addPattern adds the pattern "*<suffix>".
func (r *Rules) addPattern(p string) {
	suffix := strings.ToLower(strings.TrimPrefix(p, "*"))
	if r.suffixes[suffix] {
		return
	}
	r.suffixes[suffix] = true
	if i, found := slices.BinarySearch(r.lengths, len(suffix)); !found {
		r.lengths = slices.Insert(r.lengths, i, len(suffix))
	}
}

// HeaderNames returns the redacted header names and suffix patterns, sorted,
// for the cassette's redaction metadata (spec §7.2).
func (r *Rules) HeaderNames() []string {
	out := make([]string, 0, len(r.headers)+len(r.suffixes))
	for n := range r.headers {
		out = append(out, n)
	}
	for s := range r.suffixes {
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
// (spec §3.5, §5). The current defaults and extraHeaders (run
// --redact-header) are added to the stored names (spec §4.4). stored comes
// from a cassette, which is untrusted input.
func RulesFromMetadata(stored json.RawMessage, extraHeaders ...string) (*Rules, error) {
	var m metadata
	if err := json.Unmarshal(stored, &m); err != nil {
		return nil, fmt.Errorf("reading redaction rules: %w", err)
	}
	body := make([]Path, len(m.Body))
	for i, raw := range m.Body {
		p, err := ParsePath(raw)
		if err != nil {
			return nil, fmt.Errorf("reading redaction rules: %w", err)
		}
		body[i] = p
	}
	headers := slices.Clone(extraHeaders)
	var patterns []string
	for _, h := range m.Headers {
		if strings.HasPrefix(h, "*") {
			patterns = append(patterns, h)
		} else {
			headers = append(headers, h)
		}
	}
	r := NewRules(headers, body)
	// Stored patterns are kept, including one this binary does not know
	// that a newer one wrote: a replay never stores a value its source
	// redacted (spec §4.4). A bare "*" redacts every header and query
	// parameter, which is the safe reading of what it claims.
	for _, p := range patterns {
		r.addPattern(p)
	}
	return r, nil
}

// RedactsHeader reports whether the header called name is redacted.
func (r *Rules) RedactsHeader(name string) bool {
	name = strings.ToLower(name)
	if r.headers[name] {
		return true
	}
	// One lookup per distinct pattern length that fits in name, so neither
	// many stored patterns nor a long name make it slow.
	for _, n := range r.lengths {
		if n > len(name) {
			break
		}
		if r.suffixes[name[len(name)-n:]] {
			return true
		}
	}
	return false
}

// Headers returns a copy of hdr with every redacted value replaced by
// [REDACTED:<first 8 hex digits of its SHA-256>]. The hash lets two sessions
// be compared for using the same key without revealing it. The values of
// URL-carrying headers get the URL rule instead (ADR 0011). hdr is
// unchanged: the upstream and the agent still need the real values.
func (r *Rules) Headers(hdr http.Header) http.Header {
	out := make(http.Header, len(hdr))
	for name, values := range hdr {
		redactValue := func(v string) string { return v }
		switch lower := strings.ToLower(name); {
		case r.RedactsHeader(lower):
			redactValue = mask
		case lower == "link":
			redactValue = r.linkValue
		case urlHeaders[lower]:
			redactValue = r.urlValue
		}
		red := make([]string, len(values))
		for i, v := range values {
			red[i] = redactValue(v)
		}
		out[name] = red
	}
	return out
}

// linkValue applies urlValue to each URI reference inside <…> of a Link
// header value. An unterminated reference runs to the end of the value. It
// does not parse quoted parameters, so a "<" inside one starts a reference
// too: that redacts more, never less.
func (r *Rules) linkValue(v string) string {
	var b strings.Builder
	for {
		before, rest, ok := strings.Cut(v, "<")
		b.WriteString(before)
		if !ok {
			return b.String()
		}
		ref, after, closed := strings.Cut(rest, ">")
		b.WriteString("<" + r.urlValue(ref))
		if !closed {
			return b.String()
		}
		b.WriteString(">")
		v = after
	}
}

// urlValue redacts the URI reference ref with the URL rule, keeping it byte
// for byte when it holds nothing to redact. A reference that does not parse
// cannot be checked, so it is masked whole.
func (r *Rules) urlValue(ref string) string {
	u, err := url.Parse(ref)
	if err != nil {
		return mask(ref)
	}
	if u.User == nil && r.Query(u.RawQuery) == u.RawQuery && r.Fragment(u.EscapedFragment()) == u.EscapedFragment() {
		return ref
	}
	return r.URL(u)
}

// Query returns rawQuery with the values of credential parameters masked,
// keeping everything else byte for byte.
func (r *Rules) Query(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	parts := strings.Split(rawQuery, "&")
	for i, part := range parts {
		parts[i] = r.element(part)
	}
	return strings.Join(parts, "&")
}

// Fragment returns rawFragment with the values of credential elements
// masked, keeping everything else byte for byte. It is split on "&" and on
// "?", so a route (#/cb?access_token=…) and a value that holds a raw "?" are
// covered: such a value is masked only up to the "?" (spec §3.5).
func (r *Rules) Fragment(rawFragment string) string {
	var b strings.Builder
	for {
		i := strings.IndexAny(rawFragment, fragmentSeparators)
		if i < 0 {
			b.WriteString(r.element(rawFragment))
			return b.String()
		}
		b.WriteString(r.element(rawFragment[:i]))
		b.WriteByte(rawFragment[i])
		rawFragment = rawFragment[i+1:]
	}
}

// element masks the value of one name=value query or fragment element whose
// name is on the deny-list.
func (r *Rules) element(part string) string {
	name, value, hasValue := strings.Cut(part, "=")
	key, err := url.QueryUnescape(name)
	if err != nil {
		key = name
	}
	if hasValue && r.redactsQuery(key) {
		return name + "=" + mask(value)
	}
	return part
}

func (r *Rules) redactsQuery(name string) bool {
	return r.RedactsHeader(strings.ReplaceAll(name, "_", "-"))
}

// URL returns u as a string with its userinfo replaced by REDACTED and the
// values of its credential query parameters and fragment elements masked.
func (r *Rules) URL(u *url.URL) string {
	c := *u
	if c.User != nil {
		c.User = url.User("REDACTED")
	}
	c.RawQuery = r.Query(c.RawQuery)
	if raw := c.EscapedFragment(); raw != "" {
		red := r.Fragment(raw)
		// Fragment only replaces values that EscapedFragment produced with
		// markers, so red is valid escaped text and unescapes.
		if frag, err := url.PathUnescape(red); err == nil {
			c.Fragment, c.RawFragment = frag, red
		} else {
			c.Fragment, c.RawFragment = mask(raw), ""
		}
	}
	return c.String()
}

// HasMarker reports whether u holds what URL writes in place of a
// credential: userinfo equal to REDACTED, or a query or fragment value of
// the form [REDACTED:<8 hex digits>], escaped or not. Such a stored URL is
// not the one that was recorded (spec §4.3). A real URL of that form is
// reported too: it cannot be told apart.
func HasMarker(u *url.URL) bool {
	if u.User != nil && u.User.String() == "REDACTED" {
		return true
	}
	return hasMarkerValue(strings.Split(u.RawQuery, "&")) ||
		hasMarkerValue(strings.FieldsFunc(u.EscapedFragment(), func(c rune) bool { return strings.ContainsRune(fragmentSeparators, c) }))
}

// hasMarkerValue reports whether one of the name=value elements has a
// marker for its value.
func hasMarkerValue(elements []string) bool {
	for _, part := range elements {
		_, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		if isMarker(value) {
			return true
		}
		if unescaped, err := url.QueryUnescape(value); err == nil && isMarker(unescaped) {
			return true
		}
	}
	return false
}

func isMarker(v string) bool {
	hexDigits, ok := strings.CutPrefix(v, "[REDACTED:")
	if !ok {
		return false
	}
	if hexDigits, ok = strings.CutSuffix(hexDigits, "]"); !ok || len(hexDigits) != 8 {
		return false
	}
	_, err := hex.DecodeString(hexDigits)
	return err == nil
}

func mask(v string) string {
	sum := sha256.Sum256([]byte(v))
	return "[REDACTED:" + hex.EncodeToString(sum[:4]) + "]"
}
