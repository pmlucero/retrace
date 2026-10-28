package mcp

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

// canonicalCases are MCP request bodies whose canonical form and fingerprint
// are pinned in testdata/canonicalize. A changed golden fingerprint means
// recorded cassettes stop matching, so it must be deliberate.
var canonicalCases = []struct {
	name, server, body string
}{
	{"request", "fs", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_file","arguments":{"path":"/tmp/a"}}}`},
	{"string_id", "fs", `{"jsonrpc":"2.0","id":"req-7","method":"tools/call","params":{"name":"read_file","arguments":{"path":"/tmp/a"}}}`},
	{"key_order", "fs", "{\n \"params\": {\"arguments\": {\"path\": \"/tmp/a\"}, \"name\": \"read_file\"},\r\n\t\"method\": \"tools/call\", \"id\": 2, \"jsonrpc\": \"2.0\"}\n"},
	{"meta_removed", "fs", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"_meta":{"progressToken":"p-1"},"name":"read_file","arguments":{"path":"/tmp/a","_meta":{"kept":true}}}}`},
	{"no_params", "fs", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`},
	{"empty_params", "fs", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`},
	{"notification", "fs", `{"jsonrpc":"2.0","method":"notifications/initialized"}`},
	{"other_server", "git", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_file","arguments":{"path":"/tmp/a"}}}`},
	{"number_text", "fs", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add","arguments":{"a":1.0,"b":1e2,"c":-0}}}`},
	{"unicode_escapes", "fs", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":"café é   <b>&amp; \"q\" 😀 \/ \t"}}}`},
	{"batch_of_one", "fs", `[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`},
	{"batch", "fs", `[{"jsonrpc":"2.0","id":1,"method":"tools/list"},{"jsonrpc":"2.0","method":"notifications/progress","params":{"_meta":{"x":1},"progress":1}},{"jsonrpc":"2.0","id":"s-1","result":{"roots":[]}},{"jsonrpc":"2.0","id":"s-2","error":{"code":-1,"message":"no"}}]`},
	{"batch_reordered", "fs", `[{"jsonrpc":"2.0","method":"notifications/progress","params":{"progress":1}},{"jsonrpc":"2.0","id":1,"method":"tools/list"},{"jsonrpc":"2.0","id":"s-1","result":{"roots":[]}},{"jsonrpc":"2.0","id":"s-2","error":{"code":-1,"message":"no"}}]`},
	{"batch_invalid_element", "fs", `[{"jsonrpc":"2.0","id":1,"method":"tools/list"},{"b":1,"a":[2]},7]`},
	{"not_jsonrpc", "fs", `{"jsonrpc":"2.0","id":1}`},
	{"empty_batch", "fs", ` [ ] `},
	{"not_json", "fs", "method=tools/list&id=1\n"},
	{"truncated_json", "fs", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{`},
	{"too_deep", "fs", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"x":` + strings.Repeat("[", 999) + strings.Repeat("]", 999) + `}}`},
}

// ADR 0007 §1, spec §5 step 1: the canonical form and fingerprint of MCP
// requests.
func TestCanonicalizeGolden(t *testing.T) {
	for _, tc := range canonicalCases {
		t.Run(tc.name, func(t *testing.T) {
			checkGolden(t, tc.name, Canonicalize(tc.server, []byte(tc.body)))
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
		if err := os.MkdirAll(filepath.Dir(golden), 0o750); err != nil {
			t.Fatal(err)
		}
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

// ADR 0007 §1, spec §5 step 1: requests that differ only in their id, in
// params._meta or in encoding get the same fingerprint.
func TestCanonicalizeEquivalentBodies(t *testing.T) {
	groups := map[string][]string{
		"id": {
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"t"}}`,
			`{"jsonrpc":"2.0","id":99,"method":"tools/call","params":{"name":"t"}}`,
			`{"jsonrpc":"2.0","id":"abc","method":"tools/call","params":{"name":"t"}}`,
			`{"method":"tools/call","params":{"name":"t"},"id":0}`,
		},
		"params._meta": {
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"t"}}`,
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"_meta":{"progressToken":"a"},"name":"t"}}`,
			`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"t","_meta":{"progressToken":7}}}`,
			`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"t","_meta":null}}`,
		},
		"key order, whitespace and escapes": {
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"café","arguments":{"a":1,"b":2}}}`,
			" {\"params\": {\"arguments\": {\"b\": 2, \"a\": 1},\n \"name\": \"caf\\u00e9\"}, \"method\": \"tools\\/call\", \"id\": 5}\n",
		},
		"not JSON-RPC": {
			`{"id":null,"method":"m"}`,
			` { "method":"m", "id":null } `,
		},
		"empty batch": {`[]`, " [\n] "},
		"ids in a batch": {
			`[{"jsonrpc":"2.0","id":1,"method":"tools/list"},{"jsonrpc":"2.0","id":"s-1","result":{}}]`,
			`[{"jsonrpc":"2.0","id":"x","method":"tools/list"},{"jsonrpc":"2.0","id":"s-9","result":{}}]`,
		},
	}
	for name, bodies := range groups {
		want := match.Fingerprint(Canonicalize("fs", []byte(bodies[0])))
		for _, b := range bodies[1:] {
			if got := match.Fingerprint(Canonicalize("fs", []byte(b))); got != want {
				t.Errorf("%s: %s has fingerprint %s, want %s as for %s", name, b, got, want, bodies[0])
			}
		}
	}
}

// ADR 0007 §1, spec §5 step 1: the server, method, params and the shape of
// a batch are significant.
func TestCanonicalizeDistinctBodies(t *testing.T) {
	const req = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	const notif = `{"jsonrpc":"2.0","method":"tools/list"}`
	pairs := []struct {
		name       string
		serverA, a string
		serverB, b string
	}{
		{"server", "fs", req, "git", req},
		{"method", "fs", req, "fs", `{"jsonrpc":"2.0","id":1,"method":"prompts/list"}`},
		{"params", "fs", `{"id":1,"method":"m","params":{"a":1}}`, "fs", `{"id":1,"method":"m","params":{"a":2}}`},
		{"params absent", "fs", req, "fs", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`},
		{"nested _meta", "fs", `{"id":1,"method":"m","params":{"a":{"_meta":1}}}`, "fs", `{"id":1,"method":"m","params":{"a":{}}}`},
		{"number text", "fs", `{"id":1,"method":"m","params":{"a":1}}`, "fs", `{"id":1,"method":"m","params":{"a":1.0}}`},
		{"request vs notification", "fs", req, "fs", notif},
		{"batch of one vs bare", "fs", req, "fs", "[" + req + "]"},
		{"batch order", "fs", "[" + req + "," + notif + "]", "fs", "[" + notif + "," + req + "]"},
		{"batch with extra notification", "fs", "[" + req + "]", "fs", "[" + req + "," + notif + "]"},
		{"result vs error", "fs", `[{"id":1,"result":{}}]`, "fs", `[{"id":1,"error":{}}]`},
		{"not JSON vs JSON", "fs", `{"id":1,"method":"m"`, "fs", `{"id":1,"method":"m"}`},
		{"raw bodies of different servers", "a", `x`, "a\"\n", `x`},
		{"not JSON-RPC vs JSON-RPC", "fs", `{"id":1}`, "fs", `{"id":1,"method":"m"}`},
		{"empty batch", "fs", `[]`, "fs", `{}`},
		{"raw body vs form-like body", "fs", `{"message":{"kind":"request","method":"m"},"server":"fs"}`, "fs", `{"id":1,"method":"m"}`},
	}
	for _, p := range pairs {
		if bytes.Equal(Canonicalize(p.serverA, []byte(p.a)), Canonicalize(p.serverB, []byte(p.b))) {
			t.Errorf("%s: %s on %s and %s on %s have the same canonical form", p.name, p.a, p.serverA, p.b, p.serverB)
		}
	}
}

// Spec §5 step 1: body redaction runs before canonicalization, so a redacted
// body must canonicalize the same way however the agent encoded it.
func TestCanonicalizeRedactedBodies(t *testing.T) {
	p, err := redact.ParsePath("$.params.arguments.token")
	if err != nil {
		t.Fatal(err)
	}
	rules := redact.NewRules(nil, []redact.Path{p})
	bodies := []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"login","arguments":{"token":"s3cret"}}}`,
		`{"id":"x","params":{"_meta":{"progressToken":1},"arguments":{"token":"s3cret"},"name":"login"},"method":"tools/call","jsonrpc":"2.0"}`,
	}
	var want []byte
	for i, b := range bodies {
		redacted, err := rules.Body([]byte(b))
		if err != nil {
			t.Fatalf("Body(%s): %v", b, err)
		}
		if strings.Contains(string(redacted), "s3cret") {
			t.Fatalf("Body(%s) = %s, want the token masked", b, redacted)
		}
		got := Canonicalize("auth", redacted)
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
