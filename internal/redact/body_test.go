package redact

import (
	"strings"
	"testing"
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
	m := mask // over the JSON text of the value
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
		{"member order, numbers and characters are kept", []string{"$.z"},
			`{"z":"s","b":1.50,"a":1e3,"html":"<a&b>","u":"ñ"}`,
			`{"z":"` + m(`"s"`) + `","b":1.50,"a":1e3,"html":"<a&b>","u":"ñ"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(mustRules(t, tt.rules...).Body([]byte(tt.in))); got != tt.want {
				t.Errorf("Body =\n  %s\nwant\n  %s", got, tt.want)
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
		if got := string(r.Body([]byte(in))); got != in {
			t.Errorf("Body(%q) = %q, want it unchanged", in, got)
		}
	}
	if got := string(NewRules(nil, nil).Body([]byte(`{"email":"x"}`))); got != `{"email":"x"}` {
		t.Errorf("no rules changed the body: %q", got)
	}
}

func TestRulesSSEEvent(t *testing.T) {
	r := mustRules(t, "$..text")
	m := mask(`"secret"`)
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
		{"comments and ids are kept",
			": keepalive\nid: 7\ndata:{\"text\":\"secret\"}\n\n",
			": keepalive\nid: 7\ndata: {\"text\":\"" + m + "\"}\n\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(r.SSEEvent([]byte(tt.in))); got != tt.want {
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
