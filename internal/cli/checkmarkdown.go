package cli

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"github.com/pmlucero/retrace/internal/summary"
)

// markdownText escapes s, a value that may come from a cassette, for a line
// of Markdown or a table cell (spec §9.9.7, §10). Control and bidirectional
// control characters are escaped as in the text output, and then every ASCII
// punctuation character gets a backslash, which CommonMark renders as the
// character itself and never as markup: that rules out links, autolinks,
// HTML tags, entities, emphasis, code spans and the pipe that ends a table
// cell. The backslashes that escaping control characters adds are escaped
// too, so they render as typed.
func markdownText(s string) string {
	s = printable(s)
	var b strings.Builder
	for _, r := range s {
		if r < 0x80 && strings.ContainsRune("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// writeMarkdown writes the summary and the session lines as Markdown
// (spec §9.9.7). Every value goes through markdownText, including the ones
// that come from flags, which are as free-form as a cassette's.
func (r checkReport) writeMarkdown(buf *bytes.Buffer) {
	md := markdownText
	mode := r.mode
	if r.model != "" {
		mode += " (" + r.model + ")"
	}
	fmt.Fprintf(buf, "### retrace check: %s, repeat %d, %s\n\n", md(mode), r.repeat, md(plural(int64(len(r.sessions)), "session")))
	sessions, replays, cost := r.tally()
	counts := func(n map[string]int) string { return outcomesText(func(o string) int { return n[o] }) }

	buf.WriteString("| session | replays | diverged at |\n|---|---|---|\n")
	for _, s := range r.sessions {
		at := s.pointsText()
		if note := textDiffersNote(s.textSteps()); note != "" {
			if at != "" {
				at += "; "
			}
			at += note
		}
		fmt.Fprintf(buf, "| %s | %s | %s |\n", md(s.id), md(outcomesText(s.count)), md(at))
	}
	fmt.Fprintf(buf, "\n- **sessions:** %s\n- **replays:** %s\n", md(counts(sessions)), md(counts(replays)))
	if at := r.divergedAt(); at != "" {
		fmt.Fprintf(buf, "- **diverged at:** %s\n", md(at))
	}
	fmt.Fprintf(buf, "- **cost (est.):** %s\n", md(summary.FormatCost(cost)))
	switch {
	case r.budgetRefused:
		fmt.Fprintf(buf, "- **budget:** %s\n", md(fmt.Sprintf("refused: the estimate $%.4f exceeds --max-cost $%.4f; no replay started", r.budgetEstimate, r.budgetMax)))
	case r.budgetSkipped() > 0:
		fmt.Fprintf(buf, "- **budget:** %s\n", md(fmt.Sprintf("reached: %s skipped (--max-cost $%.4f; costs are estimates)",
			plural(int64(r.budgetSkipped()), "replay"), r.budgetMax)))
	}
	if r.interrupted {
		buf.WriteString("- **interrupted:** the replays that never started are skipped\n")
	}
	if !r.budgetRefused {
		fmt.Fprintf(buf, "- **replays in:** %s\n", md(r.out))
	}
	buf.WriteString("\n")
}

// openMarkdown opens path to append to, creating it when it does not exist,
// as the file of $GITHUB_STEP_SUMMARY expects. A new file is private to its
// owner, like the other files retrace creates for the user. check opens it
// before the first replay, so that a bad path costs nothing (spec §9.9.7).
func openMarkdown(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // G304: the path is the user's --markdown flag
}

// appendMarkdown writes the Markdown report to f, in one write, and closes it.
func (r checkReport) appendMarkdown(f *os.File) error {
	var buf bytes.Buffer
	r.writeMarkdown(&buf)
	if _, err := f.Write(buf.Bytes()); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
