package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pmlucero/retrace/internal/divergence"
	"github.com/pmlucero/retrace/internal/replay"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// AC-15: the divergence report (spec §6.4).
func TestDivergenceReport(t *testing.T) {
	long := `"` + strings.Repeat("x", 100) + `"`
	many := make([]divergence.Entry, divergence.MaxEntries)
	for i := range many {
		many[i] = divergence.Entry{Path: fmt.Sprintf("$.k%02d", i), Expected: json.RawMessage(`1`), Received: json.RawMessage(`2`)}
	}
	elided := slices.Clone(many)
	elided[0].Received = json.RawMessage(`"[retrace: 2048 bytes elided]"`)
	for _, tc := range []struct {
		name   string
		report divergenceReport
	}{
		{"mcp", divergenceReport{source: "checkout", replay: "checkout.exact.1", misses: 2, first: replay.MissReport{
			Upstream: "mcp:orders", Request: "tools/call search_orders",
			Diff: []divergence.Entry{
				{Path: "$.params.arguments.status", Expected: json.RawMessage(`"open"`)},
				{Path: "$.params.arguments.limit", Received: json.RawMessage(`10`)},
				{Path: "$.params.arguments.query", Expected: json.RawMessage(`{"a":[1,2]}`), Received: json.RawMessage(long)},
			},
		}}},
		{"no diff", divergenceReport{source: "s", replay: "s.exact.1", misses: 1, first: replay.MissReport{
			Upstream: "anthropic", Request: "POST /v1/messages", NoDiff: replay.NoDiffIncomplete,
		}}},
		{"unreadable request", divergenceReport{source: "s", replay: "s.exact.1", misses: 1, droppedEvents: 3, first: replay.MissReport{
			Upstream: "mcp:fs", NoDiff: replay.NoDiffUnredactable,
		}}},
		{"truncated", divergenceReport{source: "s", replay: "s.exact.1", misses: 1, first: replay.MissReport{
			Upstream: "anthropic", Request: "POST /v1/messages", Diff: many, DiffTruncated: true, MorePaths: true,
		}}},
		// The cap holds every differing path; one value was elided.
		{"elided at the cap", divergenceReport{source: "s", replay: "s.exact.1", misses: 1, first: replay.MissReport{
			Upstream: "anthropic", Request: "POST /v1/messages", Diff: elided, DiffTruncated: true,
		}}},
		// SECURITY.md: the upstream, request, paths and values come from an
		// untrusted cassette and cannot drive the terminal.
		{"untrusted", untrustedReport(replay.MissReport{
			Upstream: "mcp:\x1b]0;pwned\x07shop", Request: "tools/call evil\x1b[2J\u009b31m\u202etool",
			Diff: []divergence.Entry{
				{Path: "$.params.arguments['\x1b[1A\rspoofed']", Expected: json.RawMessage(`1`), Received: json.RawMessage(`2`)},
				{Path: "$.params.arguments['\u202ekey']", Expected: json.RawMessage("\"\u009b\u202eopen\""), Received: json.RawMessage("\x1b]0;t\x07")},
			},
		})},
		{"untrusted no diff", untrustedReport(replay.MissReport{
			Upstream: "mcp:\u202eshop", NoDiff: "body\x1b[2J\n\u009b",
		})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			tc.report.write(&b)
			if strings.ContainsFunc(b.String(), func(r rune) bool { return r != '\n' && unsafeRune(r) }) {
				t.Errorf("report holds raw control or bidi characters:\n%q", b.String())
			}
			path := filepath.Join("testdata", "report", strings.ReplaceAll(tc.name, " ", "-")+".txt")
			if *update {
				if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path) //nolint:gosec // G304: a golden file of this test
			if err != nil {
				t.Fatalf("%v (run go test ./internal/cli -run TestDivergenceReport -update to create it)", err)
			}
			if b.String() != string(want) {
				t.Errorf("report differs from %s (rerun with -update if the change is intended)\ngot:\n%s\nwant:\n%s", path, b.String(), want)
			}
		})
	}
}

// untrustedReport is a report of a replay of the crafted session
// "evil\x1b]0;t\x07", whose first miss is first. run rejects such an id
// before it gets here (ValidateSessionID), so escaping the ids is defense in
// depth; the upstream, request, paths and values can carry these characters.
func untrustedReport(first replay.MissReport) divergenceReport {
	return divergenceReport{source: "evil\x1b]0;t\x07", replay: "evil\x1b]0;t\x07.exact.1", misses: 1, first: first}
}

func TestReportValue(t *testing.T) {
	exact := `"` + strings.Repeat("é", reportValueRunes-2) + `"`
	for _, tc := range []struct {
		in   []byte
		want string
	}{
		{nil, "(missing)"},
		{[]byte(`null`), "null"},
		{[]byte(exact), exact},
		{[]byte(exact + " "), exact[:len(exact)-1] + "…"},
	} {
		if got := reportValue(tc.in); got != tc.want {
			t.Errorf("reportValue(%s) = %s, want %s", tc.in, got, tc.want)
		}
	}
}
