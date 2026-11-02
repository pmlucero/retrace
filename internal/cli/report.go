package cli

import (
	"fmt"
	"io"

	"github.com/pmlucero/retrace/internal/divergence"
	"github.com/pmlucero/retrace/internal/replay"
)

// reportValueRunes is how long a diff value in the divergence report can be
// before it is cut (spec §6.4).
const reportValueRunes = 80

// divergenceReport is what the report of a diverged replay shows (spec
// §6.4).
type divergenceReport struct {
	source, replay string
	first          replay.MissReport
	misses         int
	droppedEvents  int
}

// write prints the report. Its content is normative and its layout is not.
func (r divergenceReport) write(w io.Writer) {
	fmt.Fprintf(w, "retrace: replay %s diverged from %s\n", r.replay, r.source)
	miss := r.first.Upstream
	if r.first.Request != "" {
		miss += " " + r.first.Request
	}
	fmt.Fprintf(w, "  first miss: %s\n", miss)
	for _, e := range r.first.Diff {
		fmt.Fprintf(w, "    %s: %s → %s\n", e.Path, reportValue(e.Expected), reportValue(e.Received))
	}
	switch {
	case len(r.first.Diff) == 0:
		fmt.Fprintf(w, "    no diff: %s\n", r.first.NoDiff)
	case r.first.DiffTruncated && len(r.first.Diff) == divergence.MaxEntries:
		fmt.Fprintf(w, "    … more paths differ\n")
	}
	fmt.Fprintf(w, "  misses: %d\n", r.misses)
	if r.droppedEvents > 0 {
		fmt.Fprintf(w, "  warning: %d events were not written; the event log is incomplete\n", r.droppedEvents)
	}
	fmt.Fprintf(w, "  compare: retrace diff %s %s\n", r.source, r.replay)
}

// reportValue is a diff value as the report shows it: compact JSON, cut
// after reportValueRunes, or (missing) for a side without the path.
func reportValue(v []byte) string {
	if v == nil {
		return "(missing)"
	}
	s := []rune(string(v))
	if len(s) <= reportValueRunes {
		return string(s)
	}
	return string(s[:reportValueRunes-1]) + "…"
}
