package cli

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pmlucero/retrace/internal/diff"
	"github.com/pmlucero/retrace/internal/summary"
)

// checkSchemaPath is the committed schema of `check --format json`.
var checkSchemaPath = filepath.Join("..", "..", "docs", "schema", "check.v1.json")

// hostileTool holds what a tool name from an untrusted cassette can hold:
// control and bidi characters, a table cell end, an HTML tag, a link, a code
// span, an autolink and an entity.
const hostileTool = "a|b<script>\x1b[2J\u202e[x](http://evil.example)`c`*d*_e_ www.evil.example &amp; <https://x.y>\r\n# h"

// sampleCheckReport has every kind of replay: identical, diverged at a tool
// with and without a point, error with a reason, and skipped by the budget.
func sampleCheckReport() checkReport {
	cost := summary.Totals{CostUSD: 0.0125, Priced: 2, Unpriced: 1}
	return checkReport{
		mode: "swap-model", model: "claude-haiku-4-5", repeat: 2, out: "out dir",
		budgetMax: 1, budgetEstimate: 0.5,
		sessions: []checkSession{
			{id: "a", replays: []replayOutcome{{id: "a.swap-model.1", outcome: outcomeIdentical}, {id: "a.swap-model.2", outcome: outcomeIdentical}}},
			{id: "b", replays: []replayOutcome{
				{id: "b.swap-model.1", outcome: outcomeDiverged, point: toolPoint(3, "tool_use", hostileTool), totals: cost},
				{id: "b.swap-model.2", outcome: outcomeError, reason: "timed out after 10m0s", textSteps: []int{2}},
			}},
			{id: "c", replays: []replayOutcome{
				{id: "c.swap-model.1", outcome: outcomeDiverged, point: &diff.Point{Step: 4, Kind: diff.KindStop}},
				{id: "c.swap-model.2", outcome: outcomeSkipped, reason: reasonBudget},
			}},
		},
	}
}

// AC-45: the JSON of check validates against the committed schema, for a
// real check that diverges and one that the budget refuses.
func TestCheckJSON(t *testing.T) {
	schema := loadSchema(t, checkSchemaPath)
	dir := t.TempDir()
	recordHelperSession(t, dir, "a", fakeAnthropic(t).URL)
	recordHelperSession(t, dir, "b", fakeAnthropic(t).URL)
	up := swapChangingUpstream(t)
	model := []string{"--model", "claude-haiku-4-5", "--anthropic-upstream", up.URL}

	t.Run("diverged", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "out")
		r := runCheckCmd(t, dir, afterDash(append([]string{"--format", "json", "--out", out, "--repeat", "3"}, model...), agentCommand())...)
		if r.code != ExitDiverged {
			t.Fatalf("exit %d, want %d\n%s", r.code, ExitDiverged, r.stderr)
		}
		doc := decodeJSON(t, r.stdout.String())
		if err := schema.validate(doc); err != nil {
			t.Fatalf("output does not validate against %s: %v\n%s", checkSchemaPath, err, r.stdout)
		}
		var got checkDocument
		if err := json.Unmarshal([]byte(r.stdout.String()), &got); err != nil {
			t.Fatal(err)
		}
		wantCounts := checkCountsJSON{Diverged: 3, Identical: 3}
		if got.FormatVersion != 1 || got.Mode != "swap-model" || got.CorpusSize != 2 || got.Repeat != 3 ||
			got.Totals.Replays != wantCounts || got.Out == nil || *got.Out != out || got.Budget != nil || got.ExitCode != ExitDiverged {
			t.Errorf("unexpected document:\n%s", r.stdout)
		}
		if len(got.DivergedAt) != 1 || got.DivergedAt[0].Tool == nil || *got.DivergedAt[0].Tool != "get_weather" || got.DivergedAt[0].Sessions != 2 {
			t.Errorf("diverged_at = %+v, want get_weather in 2 sessions", got.DivergedAt)
		}
		for _, s := range got.Sessions {
			for _, rep := range s.Replays {
				if (rep.Outcome == outcomeDiverged) != (rep.Divergence != nil) {
					t.Errorf("replay %s: outcome %s, divergence %v", rep.ID, rep.Outcome, rep.Divergence)
				}
			}
		}
		if strings.Contains(r.stdout.String(), "retrace check  ") {
			t.Errorf("stdout holds the text report:\n%s", r.stdout)
		}
	})

	t.Run("refused by the budget", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "out")
		r := runCheckCmd(t, dir, afterDash(append([]string{"--format", "json", "--out", out, "--max-cost", "0.0002"}, model...), agentCommand())...)
		if r.code != ExitBudget {
			t.Fatalf("exit %d, want %d\n%s", r.code, ExitBudget, r.stderr)
		}
		if err := schema.validate(decodeJSON(t, r.stdout.String())); err != nil {
			t.Fatalf("output does not validate: %v\n%s", err, r.stdout)
		}
		var got checkDocument
		if err := json.Unmarshal([]byte(r.stdout.String()), &got); err != nil {
			t.Fatal(err)
		}
		if got.Out != nil || got.Budget == nil || !got.Budget.Refused || got.Budget.Skipped != 2 || got.Totals.Replays.Skipped != 2 {
			t.Errorf("unexpected document:\n%s", r.stdout)
		}
		assertNotCreated(t, out)
	})
}

// AC-45: the JSON of a report with every outcome, a point without a tool and
// a hostile tool name is pinned by a golden file, validates against the
// schema, and holds the tool name as the cassette has it.
func TestCheckJSONGolden(t *testing.T) {
	var buf bytes.Buffer
	r := sampleCheckReport()
	if err := r.writeJSON(&buf); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "check/report.json", buf.String())
	if err := loadSchema(t, checkSchemaPath).validate(decodeJSON(t, buf.String())); err != nil {
		t.Fatalf("output does not validate: %v\n%s", err, buf.String())
	}
	var got checkDocument
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if p := got.Sessions[1].Replays[0].Divergence; p == nil || p.A != nil {
		t.Errorf("divergence = %+v, want a point with no event on side a", p)
	}
	if got.DivergedAt[1].Tool == nil || *got.DivergedAt[1].Tool != hostileTool {
		t.Errorf("diverged_at = %+v, want the tool name unescaped second", got.DivergedAt)
	}
}

// The validator must be able to fail: otherwise TestCheckJSON proves nothing.
func TestCheckSchemaRejects(t *testing.T) {
	schema := loadSchema(t, checkSchemaPath)
	var buf bytes.Buffer
	r := sampleCheckReport()
	if err := r.writeJSON(&buf); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(doc map[string]any)
	}{
		{"missing member", func(doc map[string]any) { delete(doc, "totals") }},
		{"extra member", func(doc map[string]any) { doc["extra"] = true }},
		{"wrong version", func(doc map[string]any) { doc["format_version"] = 2 }},
		{"missing count", func(doc map[string]any) {
			delete(doc["totals"].(map[string]any)["replays"].(map[string]any), "skipped")
		}},
		{"unknown outcome", func(doc map[string]any) { doc["sessions"].([]any)[0].(map[string]any)["outcome"] = "ok" }},
		{"wrong type", func(doc map[string]any) { doc["interrupted"] = "no" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := decodeJSON(t, buf.String()).(map[string]any)
			tc.change(doc)
			if err := schema.validate(doc); err == nil {
				t.Error("the schema accepts the changed document")
			}
		})
	}
}

// stripEscapes removes each backslash and the ASCII punctuation character it
// escapes, which CommonMark renders as that character.
func stripEscapes(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// AC-45: a tool name cannot add markup to the Markdown report: once the
// escaped characters are removed, no punctuation, control or bidi character
// is left in what it contributes, and the table row keeps its cells.
func TestCheckMarkdownEscapes(t *testing.T) {
	esc := markdownText(hostileTool)
	if rest := stripEscapes(esc); strings.ContainsAny(rest, "|<>[]()`*_&#!:.\\/") || unsafeText(rest) {
		t.Errorf("unescaped markup left in %q (from %q)", rest, esc)
	}

	var buf bytes.Buffer
	r := sampleCheckReport()
	r.writeMarkdown(&buf)
	md := buf.String()
	if unsafeText(strings.ReplaceAll(md, "\n", "")) {
		t.Errorf("Markdown holds a control or bidi character:\n%q", md)
	}
	rows := 0
	for _, line := range strings.Split(md, "\n") {
		if !strings.HasPrefix(line, "| ") {
			continue
		}
		rows++
		if pipes := strings.Count(stripEscapes(line), "|"); pipes != 4 {
			t.Errorf("row has %d cell borders, want 4: %s", pipes, line)
		}
	}
	if rows != 4 { // the header and the three sessions
		t.Errorf("%d table rows, want 4:\n%s", rows, md)
	}
	if !strings.Contains(md, "step 3 tool\\_use "+esc) {
		t.Errorf("the tool name is not escaped as expected:\n%s", md)
	}
}

func unsafeText(s string) bool { return strings.ContainsFunc(s, unsafeRune) }

func TestMarkdownText(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"get_weather", `get\_weather`},
		{"a|b", `a\|b`},
		{"[x](y)", `\[x\]\(y\)`},
		{"<b>", `\<b\>`},
		{"`c`", "\\`c\\`"},
		{"x\x1b[2J", `x\\x1b\[2J`},
		{"\u202e", `\\u202e`},
		{"line\nbreak", `line\\nbreak`},
		{"plain text 42", "plain text 42"},
		{"é ü", "é ü"},
	} {
		if got := markdownText(tc.in); got != tc.want {
			t.Errorf("markdownText(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// AC-45: --markdown appends to an existing file, creates a missing one, and
// works with both formats, which leave stdout alone.
func TestCheckMarkdownAppends(t *testing.T) {
	dir := t.TempDir()
	recordHelperSession(t, dir, "s", fakeAnthropic(t).URL)
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			md := filepath.Join(t.TempDir(), "summary.md")
			if err := os.WriteFile(md, []byte("before\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				r := runCheckCmd(t, dir, afterDash([]string{"--format", format, "--markdown", md, "--out", filepath.Join(t.TempDir(), "out")}, agentCommand())...)
				if r.code != ExitOK {
					t.Fatalf("exit %d\n%s", r.code, r.stderr)
				}
			}
			got, err := os.ReadFile(md) //nolint:gosec // G304: a file of this test
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(string(got), "before\n### retrace check: exact") || strings.Count(string(got), "### retrace check") != 2 {
				t.Errorf("the file is not appended to:\n%s", got)
			}
			if !strings.Contains(string(got), `| s | 1 identical |`) {
				t.Errorf("the session line is missing:\n%s", got)
			}

			created := filepath.Join(t.TempDir(), "new.md")
			r := runCheckCmd(t, dir, afterDash([]string{"--format", format, "--markdown", created, "--out", filepath.Join(t.TempDir(), "out")}, agentCommand())...)
			if r.code != ExitOK {
				t.Fatalf("exit %d\n%s", r.code, r.stderr)
			}
			if st, err := os.Stat(created); err != nil || st.Mode().Perm() != 0o600 {
				t.Errorf("created file: %v, %v; want mode 0600", st, err)
			}
		})
	}
}

// Spec §9.9.8: a --markdown file that cannot be written is reported on stderr
// and exits 1, or 3 when a replay diverged.
func TestCheckMarkdownWriteError(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "missing", "summary.md")
	for _, tc := range []struct {
		name   string
		report checkReport
		want   int
	}{
		{"identical", checkReport{mode: "exact", repeat: 1, out: "o", sessions: []checkSession{{id: "s", replays: []replayOutcome{{outcome: outcomeIdentical}}}}}, ExitError},
		{"diverged", sampleCheckReport(), ExitDiverged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr syncBuffer
			env := &Env{Stdout: &stdout, Stderr: &stderr, Logger: slog.New(slog.NewTextHandler(&stderr, nil))}
			code := emitCheck(env, checkOptions{format: "text", markdown: bad}, tc.report)
			if code != tc.want {
				t.Errorf("exit %d, want %d", code, tc.want)
			}
			if !strings.Contains(stderr.String(), "cannot write the Markdown report") || !strings.Contains(stdout.String(), "retrace check") {
				t.Errorf("stderr:\n%s\nstdout:\n%s", &stderr, &stdout)
			}
		})
	}
}

func TestCheckOutputFlagErrors(t *testing.T) {
	dir := t.TempDir()
	writeSource(t, dir, "s", cwdAgentCommand(), "/")
	for _, args := range [][]string{{"--format", "xml"}, {"--format", ""}, {"--markdown", ""}} {
		out := filepath.Join(t.TempDir(), "out")
		r := runCheckCmd(t, dir, afterDash(append([]string{"--out", out}, args...), []string{"true"})...)
		if r.code != ExitUsage {
			t.Errorf("%v: exit %d, want %d\n%s", args, r.code, ExitUsage, r.stderr)
		}
		assertNotCreated(t, out)
	}
}
