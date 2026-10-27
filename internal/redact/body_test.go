package redact

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/pmlucero/retrace/internal/jsonvalue"
)

func mustRules(t *testing.T, paths ...string) *Rules {
	t.Helper()
	var ps []Path
	for _, raw := range paths {
		p, err := ParsePath(raw)
		if err != nil {
			t.Fatalf("ParsePath(%q): %v", raw, err)
		}
		ps = append(ps, p)
	}
	return NewRules(nil, ps)
}

func TestParsePath(t *testing.T) {
	valid := []string{"$.a", "$.a.b", "$['a b']", "$.a[0]", "$.a[*].b", "$.*", "$..email", "$..['e-mail']", "$.messages[0].content[*].text"}
	for _, raw := range valid {
		p, err := ParsePath(raw)
		if err != nil {
			t.Errorf("ParsePath(%q) = %v, want valid", raw, err)
		} else if p.String() != raw {
			t.Errorf("String() = %q, want %q", p.String(), raw)
		}
	}
	invalid := []string{"", "$", "a.b", "$.", "$..", "$.a[", "$.a[-1]", "$.a[x]", "$['']", "$.a[?(@.x)]", "$.a b", "$..*"}
	for _, raw := range invalid {
		if _, err := ParsePath(raw); err == nil {
			t.Errorf("ParsePath(%q) succeeded, want an error", raw)
		}
	}
}

// spec §3.5: body redaction
func TestRulesBody(t *testing.T) {
	m := func(jsonText string) string { // strings hash as they are, other values over their JSON text
		if v, err := jsonvalue.Decode([]byte(jsonText)); err == nil {
			if str, ok := v.(string); ok {
				return mask(str)
			}
		}
		return mask(jsonText)
	}
	tests := []struct {
		name  string
		rules []string
		in    string
		want  string
	}{
		{"child", []string{"$.user.email"},
			`{"user":{"email":"a@b.c","name":"Ana"}}`,
			`{"user":{"email":"` + m(`"a@b.c"`) + `","name":"Ana"}}`},
		{"any depth, every match", []string{"$..email"},
			`{"email":"x","list":[{"email":"y"},{"other":1}]}`,
			`{"email":"` + m(`"x"`) + `","list":[{"email":"` + m(`"y"`) + `"},{"other":1}]}`},
		{"index and wildcard", []string{"$.a[1]", "$.b[*].k"},
			`{"a":[1,2,3],"b":[{"k":true},{"k":null}]}`,
			`{"a":[1,"` + m(`2`) + `",3],"b":[{"k":"` + m(`true`) + `"},{"k":"` + m(`null`) + `"}]}`},
		{"objects are masked whole", []string{"$.secret"},
			`{"secret":{"x":[1,2]},"keep":1}`,
			`{"secret":"` + m(`{"x":[1,2]}`) + `","keep":1}`},
		{"overlapping rules mask once, over the original", []string{"$..email", "$.user"},
			`{"user":{"email":"x"}}`,
			`{"user":"` + m(`{"email":"x"}`) + `"}`},
		{"the same overlap in the other rule order", []string{"$.user", "$..email"},
			`{"user":{"email":"x"}}`,
			`{"user":"` + m(`{"email":"x"}`) + `"}`},
		{"nested recursive matches mask once", []string{"$..a..b"},
			`{"a":{"a":{"b":1}}}`,
			`{"a":{"a":{"b":"` + m(`1`) + `"}}}`},
		{"member order, numbers and characters are kept", []string{"$.z"},
			`{"z":"s","b":1.50,"a":1e3,"html":"<a&b>","u":"ñ"}`,
			`{"z":"` + m(`"s"`) + `","b":1.50,"a":1e3,"html":"<a&b>","u":"ñ"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := mustRules(t, tt.rules...).Body([]byte(tt.in))
			if err != nil || string(got) != tt.want {
				t.Errorf("Body =\n  %s, %v\nwant\n  %s", got, err, tt.want)
			}
		})
	}
}

func TestRulesBodyLeavesUnmatchedBodiesByteForByte(t *testing.T) {
	r := mustRules(t, "$..email")
	for _, in := range []string{
		"{ \"name\" : \"Ana\" ,\n \"n\": 1.0 }", // JSON without a match keeps its formatting
		"not json",
		`{"a":1} trailing`,
		"",
	} {
		if got, err := r.Body([]byte(in)); err != nil || string(got) != in {
			t.Errorf("Body(%q) = %q, %v; want it unchanged", in, got, err)
		}
	}
	if got, _ := NewRules(nil, nil).Body([]byte(`{"email":"x"}`)); string(got) != `{"email":"x"}` {
		t.Errorf("no rules changed the body: %q", got)
	}
}

// A string body value and the same header value get the same marker, so a
// secret can be correlated wherever it appears (spec §3.5).
func TestRulesBodyStringsHashLikeHeaders(t *testing.T) {
	got, _ := mustRules(t, "$.key").Body([]byte(`{"key":"sk-123"}`))
	header := NewRules(nil, nil).Headers(http.Header{"X-Api-Key": {"sk-123"}}).Get("X-Api-Key")
	if string(got) != `{"key":"`+header+`"}` {
		t.Errorf("body %s, header marker %s: want the same marker", got, header)
	}
}

// A body too deep to decode safely is reported, never stored unredacted.
func TestRulesBodyRefusesTooDeepJSON(t *testing.T) {
	deep := strings.Repeat("[", jsonvalue.MaxDepth+1) + strings.Repeat("]", jsonvalue.MaxDepth+1)
	deeper := strings.Repeat("[", 20000) + strings.Repeat("]", 20000) // past encoding/json's own limit too
	for _, in := range []string{deep, `{"email":` + deep + `}`, deeper} {
		got, err := mustRules(t, "$..email").Body([]byte(in))
		if !errors.Is(err, ErrNotRedactable) || got != nil {
			t.Errorf("Body(deep) = %d bytes, %v; want ErrNotRedactable and nothing", len(got), err)
		}
	}
	ok := strings.Repeat("[", jsonvalue.MaxDepth) + strings.Repeat("]", jsonvalue.MaxDepth)
	if _, err := mustRules(t, "$..email").Body([]byte(ok)); err != nil {
		t.Errorf("Body at the depth limit: %v", err)
	}
	if _, err := mustRules(t, "$..text").SSEEvent([]byte("data: " + deep + "\n\n")); !errors.Is(err, ErrNotRedactable) {
		t.Errorf("SSEEvent(deep) error = %v, want ErrNotRedactable", err)
	}
}

func TestRulesSSEEvent(t *testing.T) {
	r := mustRules(t, "$..text")
	m := mask("secret")
	tests := []struct{ name, in, want string }{
		{"one data line",
			"event: content_block_delta\ndata: {\"delta\":{\"text\":\"secret\"}}\n\n",
			"event: content_block_delta\ndata: {\"delta\":{\"text\":\"" + m + "\"}}\n\n"},
		{"CRLF framing is kept",
			"event: x\r\ndata: {\"text\":\"secret\"}\r\n\r\n",
			"event: x\r\ndata: {\"text\":\"" + m + "\"}\r\n\r\n"},
		{"data split over lines is joined",
			"data: {\"text\":\ndata: \"secret\"}\n\n",
			"data: {\"text\":\"" + m + "\"}\n\n"},
		{"no match is unchanged",
			"event: ping\ndata: {\"type\":\"ping\"}\n\n",
			"event: ping\ndata: {\"type\":\"ping\"}\n\n"},
		{"non-JSON data is unchanged",
			"data: [DONE]\n\n",
			"data: [DONE]\n\n"},
		{"an unterminated last event",
			"data: {\"text\":\"secret\"}",
			"data: {\"text\":\"" + m + "\"}"},
		{"comments and ids are kept",
			": keepalive\nid: 7\ndata:{\"text\":\"secret\"}\n\n",
			": keepalive\nid: 7\ndata: {\"text\":\"" + m + "\"}\n\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := r.SSEEvent([]byte(tt.in)); err != nil || string(got) != tt.want {
				t.Errorf("SSEEvent =\n  %q\nwant\n  %q", got, tt.want)
			}
		})
	}
}

func TestRulesBodyRules(t *testing.T) {
	if got := strings.Join(mustRules(t, "$..email", "$.a[0]").BodyRules(), ","); got != "$..email,$.a[0]" {
		t.Errorf("BodyRules() = %s", got)
	}
}
