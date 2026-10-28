package anthropic

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pmlucero/retrace/internal/match"
	"github.com/pmlucero/retrace/internal/redact"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// canonicalCases are request bodies whose canonical form and fingerprint are
// pinned in testdata/canonicalize. A changed golden fingerprint means
// recorded cassettes stop matching, so it must be deliberate.
var canonicalCases = []struct {
	name, body string
}{
	{"key_order", `{"stream":false,"model":"claude-sonnet-4-5","max_tokens":1024,"messages":[{"content":"hi","role":"user"}]}`},
	{"whitespace", "{\n  \"model\" : \"claude-sonnet-4-5\",\r\n\t\"messages\": [ { \"role\": \"user\", \"content\": \"hi\" } ]\n}\n"},
	{"number_text", `{"model":"claude-sonnet-4-5","max_tokens":1024,"temperature":0.70,"top_p":1e0,"top_k":-0,"messages":[]}`},
	{"unicode_escapes", `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"caf\u00e9 é \u2028 <b>&amp; \"q\" \ud83d\ude00 \/ \t"}]}`},
	{"metadata_removed", `{"metadata":{"user_id":"u-123"},"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`},
	{"nested_tools_messages", `{
		"model": "claude-sonnet-4-5", "max_tokens": 512, "stream": true,
		"system": [{"type": "text", "text": "Be brief.", "cache_control": {"type": "ephemeral"}}],
		"tools": [{"name": "get_time", "description": "Current time",
			"input_schema": {"type": "object", "properties": {"tz": {"type": "string"}, "metadata": {"type": "object"}}, "required": ["tz"]}}],
		"tool_choice": {"type": "auto", "disable_parallel_tool_use": false},
		"messages": [
			{"role": "user", "content": "What time is it?"},
			{"role": "assistant", "content": [{"type": "tool_use", "id": "toolu_01", "name": "get_time", "input": {"tz": "UTC", "metadata": {"x": 1}}}]},
			{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_01", "content": "12:00"}]}
		]}`},
	{"invalid_utf8", "{\"model\":\"m\",\"messages\":[{\"role\":\"user\",\"content\":\"cut \xf0\x9f \\ud83d \\udfff\"}]}"},
	{"stream_true", `{"model":"claude-sonnet-4-5","stream":true,"messages":[]}`},
	{"stream_false", `{"model":"claude-sonnet-4-5","stream":false,"messages":[]}`},
	{"not_an_object", ` [ {"b":1,"a":2} ] `},
	{"not_json", "model=claude-sonnet-4-5&stream=true\n"},
	{"truncated_json", `{"model":"claude-sonnet-4-5","messages":[`},
	{"too_deep", `{"model":"m","messages":` + strings.Repeat("[", 1000) + strings.Repeat("]", 1000) + `}`},
}

// ADR 0007 §1, spec §5: the canonical form and fingerprint of LLM requests.
func TestCanonicalizeGolden(t *testing.T) {
	for _, tc := range canonicalCases {
		t.Run(tc.name, func(t *testing.T) {
			canonical := Canonicalize([]byte(tc.body))
			checkGolden(t, tc.name, canonical)
			if again := Canonicalize(canonical); !bytes.Equal(again, canonical) {
				t.Errorf("canonicalizing the canonical form changed it:\n%s\nwant:\n%s", again, canonical)
			}
		})
	}
}

// checkGolden compares a canonical form and its fingerprint with
// testdata/canonicalize/<name>.golden.
func checkGolden(t *testing.T, name string, canonical []byte) {
	t.Helper()
	got := append(append(bytes.Clone(canonical), '\n'), match.Fingerprint(canonical)+"\n"...)
	golden := filepath.Join("testdata", "canonicalize", name+".golden")
	if *update {
		if err := os.WriteFile(golden, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden) //nolint:gosec // G304: a test fixture path
	if err != nil {
		t.Fatalf("reading golden file (run with -update to create it): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("canonical form and fingerprint:\n%s\nwant:\n%s", got, want)
	}
}

// ADR 0007 §1: requests that differ only in encoding, or in metadata, get the
// same fingerprint.
func TestCanonicalizeEquivalentBodies(t *testing.T) {
	groups := map[string][]string{
		"key order and whitespace": {
			`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`,
			`{"max_tokens":10,"messages":[{"content":"hi","role":"user"}],"model":"m"}`,
			" {\n \"model\": \"m\",\n \"messages\": [{\"role\": \"user\", \"content\": \"hi\"}],\n \"max_tokens\": 10\n}\n",
		},
		"string escapes": {
			`{"model":"m","system":"café </b>"}`,
			`{"model":"m","system":"caf\u00e9 \u003c\/b\u003e"}`,
			`{"\u006dodel":"m","system":"caf\u00E9 <\/b>"}`,
		},
		"metadata": {
			`{"model":"m","messages":[]}`,
			`{"model":"m","messages":[],"metadata":{"user_id":"a"}}`,
			`{"metadata":{"user_id":"b"},"model":"m","messages":[]}`,
			`{"metadata":null,"model":"m","messages":[]}`,
		},
	}
	for name, bodies := range groups {
		want := match.Fingerprint(Canonicalize([]byte(bodies[0])))
		for _, b := range bodies[1:] {
			if got := match.Fingerprint(Canonicalize([]byte(b))); got != want {
				t.Errorf("%s: %s has fingerprint %s, want %s as for %s", name, b, got, want, bodies[0])
			}
		}
	}
}

// ADR 0007 §1: every field but the top-level metadata is significant.
func TestCanonicalizeDistinctBodies(t *testing.T) {
	pairs := []struct {
		name, a, b string
	}{
		{"stream", `{"model":"m","stream":true}`, `{"model":"m","stream":false}`},
		{"stream absent", `{"model":"m"}`, `{"model":"m","stream":false}`},
		{"model", `{"model":"m1"}`, `{"model":"m2"}`},
		{"number text", `{"model":"m","temperature":1}`, `{"model":"m","temperature":1.0}`},
		{"message order", `{"messages":[{"content":"a"},{"content":"b"}]}`, `{"messages":[{"content":"b"},{"content":"a"}]}`},
		{"nested metadata", `{"model":"m","tools":[{"metadata":1}]}`, `{"model":"m","tools":[{}]}`},
		{"tool_choice", `{"model":"m","tool_choice":{"type":"auto"}}`, `{"model":"m","tool_choice":{"type":"any"}}`},
		{"not JSON vs JSON", `{"model":"m"`, `{"model":"m"}`},
	}
	for _, p := range pairs {
		if bytes.Equal(Canonicalize([]byte(p.a)), Canonicalize([]byte(p.b))) {
			t.Errorf("%s: %s and %s have the same canonical form", p.name, p.a, p.b)
		}
	}
}

// Spec §5: body redaction runs before canonicalization, so a redacted body
// must canonicalize the same way however the agent encoded it.
func TestCanonicalizeRedactedBodies(t *testing.T) {
	var paths []redact.Path
	for _, raw := range []string{"$..email", "$.system"} {
		p, err := redact.ParsePath(raw)
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	rules := redact.NewRules(nil, paths)
	bodies := []string{
		`{"model":"m","system":"secret prompt","messages":[{"role":"user","content":[{"type":"text","text":"hi","email":"a@b.c"}]}]}`,
		"{\"messages\": [{\"content\": [{\"email\": \"a@b.c\", \"text\": \"hi\", \"type\": \"text\"}], \"role\": \"user\"}],\n \"system\": \"secret prompt\", \"model\": \"m\"}",
		`{"system":"secret\u0020prompt","model":"m","messages":[{"role":"user","content":[{"email":"a\u0040b.c","type":"text","text":"hi"}]}],"metadata":{"user_id":"u"}}`,
	}
	var want []byte
	for i, b := range bodies {
		redacted, err := rules.Body([]byte(b))
		if err != nil {
			t.Fatalf("Body(%s): %v", b, err)
		}
		if strings.Contains(string(redacted), "secret prompt") || strings.Contains(string(redacted), "a@b.c") {
			t.Fatalf("Body(%s) = %s, want the values masked", b, redacted)
		}
		got := Canonicalize(redacted)
		if i == 0 {
			checkGolden(t, "redacted", got)
			want = got
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("redacted %s canonicalizes to\n%s\nwant\n%s", b, got, want)
		}
	}
}
