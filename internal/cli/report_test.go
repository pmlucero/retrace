package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
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
			Upstream: "anthropic", Request: "POST /v1/messages", Diff: many, DiffTruncated: true,
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			tc.report.write(&b)
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
