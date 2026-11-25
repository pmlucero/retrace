package cli

import (
	"bytes"
	"strings"
	"testing"
)

// AC-45: the Markdown report says when the budget refused or skipped
// replays, and when a signal stopped the check, and escapes what it says.
func TestCheckMarkdownBudgetAndInterrupt(t *testing.T) {
	var buf bytes.Buffer
	r := sampleCheckReport()
	r.interrupted = true
	r.writeMarkdown(&buf)
	for _, want := range []string{`**budget:** reached\: 1 replay skipped \(\-\-max\-cost \$1\.0000\; costs are estimates\)`,
		"**interrupted:**", `**replays in:** out dir`} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("Markdown lacks %q:\n%s", want, buf.String())
		}
	}
	buf.Reset()
	r.budgetRefused, r.out = true, ""
	r.writeMarkdown(&buf)
	if !strings.Contains(buf.String(), `**budget:** refused\: the estimate \$0\.5000 exceeds`) || strings.Contains(buf.String(), "replays in") {
		t.Errorf("unexpected refused report:\n%s", buf.String())
	}
}
