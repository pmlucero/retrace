package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/summary"
)

// usageUpstream answers every request with a tool_use that bills the given
// input tokens, and counts the requests.
func usageUpstream(t *testing.T, tokensIn int) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"type":"message","role":"assistant","model":"claude-haiku-4-5",`+
			`"content":[{"type":"text","text":"Checking."},{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Paris"}}],`+
			`"stop_reason":"tool_use","usage":{"input_tokens":%d,"output_tokens":10}}`, tokensIn)
	}))
	t.Cleanup(up.Close)
	return up, &calls
}

// budgetCorpus records sessions a and b, 100 input and 10 output tokens each,
// so that one replay at claude-haiku-4-5 is estimated at $0.00015.
func budgetCorpus(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	recordHelperSession(t, dir, "a", fakeAnthropic(t).URL)
	recordHelperSession(t, dir, "b", fakeAnthropic(t).URL)
	return dir
}

// AC-43: an estimate over the budget starts no replay, creates no --out, and
// exits 5 with every replay skipped.
func TestCheckBudgetRefusesTheCheck(t *testing.T) {
	dir, out := budgetCorpus(t), filepath.Join(t.TempDir(), "out")
	up, calls := usageUpstream(t, 100)

	r := runCheckCmd(t, dir, afterDash([]string{"--out", out, "--model", "claude-haiku-4-5", "--max-cost", "0.0002",
		"--anthropic-upstream", up.URL}, agentCommand())...)
	if r.code != ExitBudget {
		t.Fatalf("exit %d, want %d\n%s", r.code, ExitBudget, r.stderr)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("%d requests reached the API, want 0", n)
	}
	assertNotCreated(t, out)
	for _, want := range []string{"2 skipped", "budget       refused", "$0.0003 exceeds --max-cost $0.0002", "replays in   none"} {
		if !strings.Contains(r.stdout.String(), want) {
			t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
		}
	}
	if want := "estimated cost: $0.0003 (an estimate, not a cap on billing)"; !strings.Contains(r.stderr.String(), want) {
		t.Errorf("stderr lacks %q:\n%s", want, r.stderr)
	}
}

// AC-43: a budget reached during the check skips the remaining replays.
func TestCheckBudgetReachedMidCheck(t *testing.T) {
	dir, out := budgetCorpus(t), filepath.Join(t.TempDir(), "out")
	up, calls := usageUpstream(t, 1000) // a replay costs $0.00105, more than its estimate

	r := runCheckCmd(t, dir, afterDash([]string{"--out", out, "--model", "claude-haiku-4-5", "--max-cost", "0.0004",
		"--anthropic-upstream", up.URL}, agentCommand())...)
	if r.code != ExitBudget {
		t.Fatalf("exit %d, want %d\n%s%s", r.code, ExitBudget, r.stdout, r.stderr)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("%d requests reached the API, want 1", n)
	}
	for _, want := range []string{"a   1 identical", "b   1 skipped", "budget       reached: 1 replay skipped"} {
		if !strings.Contains(r.stdout.String(), want) {
			t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
		}
	}
	if strings.Contains(r.stdout.String(), "refused") {
		t.Errorf("a check cut short is not refused:\n%s", r.stdout)
	}
}

// AC-43: a budget that covers the estimate changes nothing.
func TestCheckBudgetWithinBudgetExitsZero(t *testing.T) {
	dir, out := budgetCorpus(t), filepath.Join(t.TempDir(), "out")
	up, _ := usageUpstream(t, 100)

	r := runCheckCmd(t, dir, afterDash([]string{"--out", out, "--model", "claude-haiku-4-5", "--max-cost", "1",
		"--anthropic-upstream", up.URL}, agentCommand())...)
	if r.code != ExitOK {
		t.Fatalf("exit %d, want 0\n%s%s", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stderr.String(), "estimated cost: $0.0003") || strings.Contains(r.stdout.String(), "budget ") {
		t.Errorf("stderr %q, stdout %q", r.stderr, r.stdout)
	}
}

// AC-43: a model missing from the price table has an unknown estimate, which
// a budget cannot guard.
func TestCheckBudgetUnknownPrice(t *testing.T) {
	dir := budgetCorpus(t)
	up, calls := usageUpstream(t, 100)

	r := runCheckCmd(t, dir, afterDash([]string{"--out", filepath.Join(t.TempDir(), "out"), "--model", "no-such-model",
		"--max-cost", "1", "--anthropic-upstream", up.URL}, agentCommand())...)
	if r.code != ExitUsage || !strings.Contains(r.stderr.String(), "--max-cost") {
		t.Errorf("exit %d, stderr %q; want a usage error naming --max-cost", r.code, r.stderr)
	}
	if calls.Load() != 0 {
		t.Error("a replay started")
	}

	r = runCheckCmd(t, dir, afterDash([]string{"--out", filepath.Join(t.TempDir(), "out"), "--model", "no-such-model",
		"--anthropic-upstream", up.URL}, agentCommand())...)
	if want := "estimated cost: unknown"; !strings.Contains(r.stderr.String(), want) {
		t.Errorf("stderr lacks %q:\n%s", want, r.stderr)
	}
}

// AC-43: --max-cost is only valid in swap-model, and takes a positive number.
func TestCheckBudgetFlagErrors(t *testing.T) {
	dir := budgetCorpus(t)
	for _, tt := range []struct {
		name string
		args []string
	}{
		{"exact", []string{"--max-cost", "1"}},
		{"live-tools", []string{"--tools", "live", "--max-cost", "1"}},
		{"zero", []string{"--model", "claude-haiku-4-5", "--max-cost", "0"}},
		{"negative", []string{"--model", "claude-haiku-4-5", "--max-cost", "-1"}},
		{"infinite", []string{"--model", "claude-haiku-4-5", "--max-cost", "inf"}},
		{"not a number", []string{"--model", "claude-haiku-4-5", "--max-cost", "NaN"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "out")
			r := runCheckCmd(t, dir, afterDash(append([]string{"--out", out}, tt.args...), agentCommand())...)
			if r.code != ExitUsage || !strings.Contains(r.stderr.String(), "--max-cost") {
				t.Errorf("exit %d, stderr %q; want a usage error naming --max-cost", r.code, r.stderr)
			}
			assertNotCreated(t, out)
		})
	}
}

// AC-43: a replay with a response that has tokens but no cost counts with its
// estimate, so an unknown cost never lowers what was spent (spec §9.9.6).
func TestCheckBudgetRecordCountsUnpricedAtItsEstimate(t *testing.T) {
	b := &checkBudget{max: 1, perRun: map[string]float64{"a": 0.5}}
	b.record("a", replayOutcome{totals: summary.Totals{CostUSD: 0.125, Priced: 1}})
	if b.spent != 0.125 {
		t.Errorf("spent %v after a priced replay, want its cost 0.125", b.spent)
	}
	b.record("a", replayOutcome{totals: summary.Totals{CostUSD: 0.125, Priced: 1, Unpriced: 1}})
	if b.spent != 0.625 {
		t.Errorf("spent %v after an unpriced replay, want 0.125 + the 0.5 estimate", b.spent)
	}
	if b.allows("a") { // 0.625 + 0.5 > 1
		t.Error("the next replay was allowed over the budget")
	}
}

// AC-43: a replay whose billed cost cannot be read counts with its estimate,
// so the budget stops the next replay instead of undercounting.
func TestCheckBudgetRecordCountsCostUnknownAtItsEstimate(t *testing.T) {
	for _, tt := range []struct {
		name      string
		out       replayOutcome
		wantSpent float64
		wantAllow bool
	}{
		{"failed replay with no totals", replayOutcome{outcome: outcomeError, costUnknown: true}, 0.5, false},
		{"cost above the estimate is kept", replayOutcome{outcome: outcomeError, costUnknown: true, totals: summary.Totals{CostUSD: 0.75}}, 0.75, false},
		{"failure before the agent ran costs nothing", replayOutcome{outcome: outcomeError}, 0, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b := &checkBudget{max: 0.9, perRun: map[string]float64{"a": 0.5}}
			b.record("a", tt.out)
			if b.spent != tt.wantSpent {
				t.Errorf("spent %v, want %v", b.spent, tt.wantSpent)
			}
			if got := b.allows("a"); got != tt.wantAllow {
				t.Errorf("allows = %v, want %v", got, tt.wantAllow)
			}
		})
	}
}

// AC-43: a source session that cannot be priced after a priceable one makes
// the estimate unknown without failing the check (spec §9.9.6).
func TestNewCheckBudgetUnknownPriceMidCorpus(t *testing.T) {
	dir := t.TempDir()
	cheap, _ := usageUpstream(t, 100)
	huge, _ := usageUpstream(t, 300000) // beyond the 200000-token tier of the model
	recordHelperSession(t, dir, "a", cheap.URL)
	recordHelperSession(t, dir, "b", huge.URL)

	env := &Env{Globals: Globals{Dir: dir}}
	corpus := []cassette.Session{{ID: "a"}, {ID: "b"}}
	b, err := newCheckBudget(context.Background(), env, corpus, "claude-haiku-4-5", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if b.known {
		t.Error("known = true, want false when a later session cannot be priced")
	}
	if b.refuses() {
		t.Error("an unknown estimate must not refuse the check")
	}
}

// AC-43, AC-44: a divergence and an error take precedence over a budget skip.
func TestCheckExitCodeBudget(t *testing.T) {
	skipped := replayOutcome{outcome: outcomeSkipped, reason: reasonBudget}
	for _, tt := range []struct {
		name    string
		outcome []replayOutcome
		want    int
	}{
		{"budget only", []replayOutcome{{outcome: outcomeIdentical}, skipped}, ExitBudget},
		{"diverged and budget", []replayOutcome{{outcome: outcomeDiverged}, skipped}, ExitDiverged},
		{"error and budget", []replayOutcome{{outcome: outcomeError}, skipped}, ExitError},
		{"interruption skip is not the budget", []replayOutcome{{outcome: outcomeSkipped}}, ExitOK},
	} {
		got := (checkReport{sessions: []checkSession{{replays: tt.outcome}}}).exitCode()
		if got != tt.want {
			t.Errorf("%s: exit %d, want %d", tt.name, got, tt.want)
		}
	}
}
