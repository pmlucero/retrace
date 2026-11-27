package redact

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/pmlucero/retrace/internal/jsonvalue"
)

// withoutPrefilter returns r with the prefilter off: the reference every
// prefiltered result must equal.
func withoutPrefilter(r *Rules) *Rules {
	c := *r
	c.literals = nil
	return &c
}

func nest(n int, inner string) string {
	return strings.Repeat("[", n) + inner + strings.Repeat("]", n)
}

func TestPrefilterSameAsDecode(t *testing.T) {
	rulesets := map[string][]string{
		"literal":         {"$..email"},
		"two literals":    {"$.user.password", "$..api_key"},
		"quoted name":     {"$['a/b']"},
		"wildcard":        {"$.users[*].email"},
		"index":           {"$.items[0].secret"},
		"index first":     {"$[0].email"},
		"index recursive": {"$..items[2].token"},
		"index last":      {"$.a[0]"},
		"recursive index": {"$[0]..email"},
		"index and plain": {"$.a[0].b", "$..email"},
		"index wildcard":  {"$.a[0].b", "$.*.c"},
		"index only":      {"$[0]"},
		"index only plus": {"$[0]", "$..email"},
		"non-ASCII name":  {"$['clé']"},
		"name with space": {"$['a b']"},
	}
	bodies := map[string]string{
		"empty":             ``,
		"whitespace":        "  \n",
		"not JSON":          `hello world`,
		"invalid JSON":      `{"a":`,
		"trailing data":     `{"a":1} x`,
		"no key":            `{"model":"m","n":1}`,
		"key present":       `{"email":"a@b.c"}`,
		"key nested":        `{"x":[{"user":{"password":"p"}}]}`,
		"key in value only": `{"note":"email"}`,
		"escaped u":         `{"em\u0061il":"a@b.c"}`,
		"escaped u api":     `{"api\u005fkey":"k"}`,
		"escaped slash":     `{"a\/b":"v"}`,
		"escaped quote":     `{"x":"say \"hi\""}`,
		"escaped surrogate": `{"cl\u00e9":"v","k":"\ud83d\ude00"}`,
		"invalid UTF-8":     "{\"em\xffail\":\"a\"}",
		"invalid with key":  "{\"email\":\"a\xff\"}",
		"array of scalars":  `[1,2,3]`,
		"top array key":     `[{"email":"a@b.c"},{"email":"x"}]`,
		"top array no key":  `[{"n":1},{"n":2}]`,
		"index names":       `{"a":[{"b":"v"}],"items":[1,2,{"token":"t"}]}`,
		"index one name":    `{"a":[{"c":"v"}]}`,
		"index key escaped": `{"it\u0065ms":[{"secret":"s"}],"a":[{"b\u0000":1}]}`,
		"index val escaped": `{"items":[{"s\u0065cret":"s"}]}`,
		"too deep index":    nest(jsonvalue.MaxDepth+1, `{"items":[{"secret":1}]}`),
		"scalar":            `"email"`,
		"deep no key":       nest(jsonvalue.MaxDepth, "1"),
		"too deep no key":   nest(jsonvalue.MaxDepth+1, "1"),
		"too deep key":      nest(jsonvalue.MaxDepth+1, `{"email":1}`),
		"too deep invalid":  strings.Repeat("[", jsonvalue.MaxDepth+1) + "x",
		"invalid first":     "x" + strings.Repeat("[", jsonvalue.MaxDepth+1),
		"many brackets str": `["` + strings.Repeat("[", 2000) + `"]`,
		"too deep, escaped quotes": `["a\"]", "b\\"` + strings.Repeat(",[", jsonvalue.MaxDepth) + `1` +
			strings.Repeat("]", jsonvalue.MaxDepth) + `]`,
		"too deep after str": `["]]]"` + strings.Repeat(",[", 1) + strings.Repeat("[", jsonvalue.MaxDepth) +
			strings.Repeat("]", jsonvalue.MaxDepth+1) + `]`,
	}
	for rn, paths := range rulesets {
		r := mustRules(t, paths...)
		ref := withoutPrefilter(r)
		for bn, body := range bodies {
			got, gerr := r.Body([]byte(body))
			want, werr := ref.Body([]byte(body))
			if !bytes.Equal(got, want) || (gerr == nil) != (werr == nil) || errors.Is(gerr, ErrNotRedactable) != errors.Is(werr, ErrNotRedactable) {
				t.Errorf("%s / %s: Body = %q, %v; want %q, %v", rn, bn, got, gerr, want, werr)
			}
			event := "event: x\ndata: " + strings.ReplaceAll(body, "\n", " ") + "\n\n"
			if strings.ContainsAny(body, "\r") {
				continue
			}
			gotE, gerr := r.SSEEvent([]byte(event))
			wantE, werr := ref.SSEEvent([]byte(event))
			if !bytes.Equal(gotE, wantE) || (gerr == nil) != (werr == nil) || errors.Is(gerr, ErrNotRedactable) != errors.Is(werr, ErrNotRedactable) {
				t.Errorf("%s / %s: SSEEvent = %q, %v; want %q, %v", rn, bn, gotE, gerr, wantE, werr)
			}
		}
	}
}

// A key written with an escape is still redacted, even though its bytes do
// not contain the rule's name.
func TestPrefilterEscapedKeyRedacted(t *testing.T) {
	r := mustRules(t, "$..api_key")
	if r.literals == nil {
		t.Fatal("prefilter is off for a literal rule")
	}
	for _, body := range []string{
		`{"api\u005fkey":"sk-secret"}`,
		`{"\u0061pi_key":"sk-secret"}`,
		`{"a":{"api_ke\u0079":"sk-secret"}}`,
	} {
		got, err := r.Body([]byte(body))
		if err != nil || bytes.Contains(got, []byte("sk-secret")) || !bytes.Contains(got, []byte("[REDACTED:")) {
			t.Errorf("Body(%s) = %q, %v; want the value masked", body, got, err)
		}
		event, err := r.SSEEvent([]byte("data: " + body + "\n\n"))
		if err != nil || bytes.Contains(event, []byte("sk-secret")) {
			t.Errorf("SSEEvent(%s) = %q, %v; want the value masked", body, event, err)
		}
	}
	index := mustRules(t, "$.items[0].secret")
	if index.literals == nil {
		t.Fatal("prefilter is off for an index rule")
	}
	got, _ := index.Body([]byte(`{"it\u0065ms":[{"secret":"sk-secret"}]}`))
	if bytes.Contains(got, []byte("sk-secret")) {
		t.Errorf("Body of an index rule with an escaped key = %q; want the value masked", got)
	}
	slash := mustRules(t, "$['a/b']")
	got, _ = slash.Body([]byte(`{"a\/b":"sk-secret"}`))
	if bytes.Contains(got, []byte("sk-secret")) {
		t.Errorf(`Body with key a\/b = %q; want the value masked`, got)
	}
}

func TestPrefilterBypass(t *testing.T) {
	for _, tt := range []struct {
		name  string
		paths []string
		want  bool // prefilter on
	}{
		{"literal", []string{"$..email"}, true},
		{"child path", []string{"$.a.b"}, true},
		{"wildcard", []string{"$.a[*].b"}, false},
		{"index", []string{"$.a[0].b"}, true},
		{"index only name", []string{"$.a[0]"}, true},
		{"index first", []string{"$[0].email"}, true},
		{"index after recursive", []string{"$..items[2].token"}, true},
		{"index then recursive", []string{"$[0]..email"}, true},
		{"index with plain rule", []string{"$.a[0].b", "$..email"}, true},
		{"index with wildcard rule", []string{"$.a[0].b", "$.*.c"}, false},
		{"index and wildcard step", []string{"$.a[0][*]"}, false},
		{"only index", []string{"$[0]"}, false},
		{"only indexes", []string{"$[0][1]"}, false},
		{"index rule without name next to literal", []string{"$..email", "$[0]"}, false},
		{"index rule with odd name", []string{`$['a"b'][0]`}, false},
		{"one wildcard among literals", []string{"$..email", "$.*.b"}, false},
		{"name with quote-like char", []string{`$['a"b']`}, false},
		{"name with backslash", []string{`$['a\b']`}, false},
	} {
		if got := mustRules(t, tt.paths...).literals != nil; got != tt.want {
			t.Errorf("%s: prefilter on = %t, want %t", tt.name, got, tt.want)
		}
	}
}

func TestPrefilterTooDeepStillRefused(t *testing.T) {
	r := mustRules(t, "$..email")
	_, err := r.Body([]byte(nest(jsonvalue.MaxDepth+1, "1")))
	if !errors.Is(err, ErrNotRedactable) {
		t.Errorf("Body of a too deep body without the key: err = %v, want ErrNotRedactable", err)
	}
}
