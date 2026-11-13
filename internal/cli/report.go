package cli

import (
	"fmt"
	"io"

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
// The upstream, request, paths and values come from the replayed requests
// and the source cassette, both untrusted (SECURITY.md), so every piece of
// text goes through printable, as in diff.
func (r divergenceReport) write(w io.Writer) {
	source, id := printable(r.source), printable(r.replay)
	fmt.Fprintf(w, "retrace: replay %s diverged from %s\n", id, source)
	miss := r.first.Upstream
	if r.first.Request != "" {
		miss += " " + r.first.Request
	}
	fmt.Fprintf(w, "  first miss: %s\n", printable(miss))
	for _, e := range r.first.Diff {
		fmt.Fprintf(w, "    %s: %s → %s\n", printable(e.Path), diffValue(e.Expected), diffValue(e.Received))
	}
	switch {
	case len(r.first.Diff) == 0:
		fmt.Fprintf(w, "    no diff: %s\n", printable(r.first.NoDiff))
	case r.first.MorePaths:
		fmt.Fprintf(w, "    … more paths differ\n")
	}
	fmt.Fprintf(w, "  misses: %d\n", r.misses)
	if r.droppedEvents > 0 {
		fmt.Fprintf(w, "  warning: %d events were not written; the event log is incomplete\n", r.droppedEvents)
	}
	fmt.Fprintf(w, "  compare: retrace diff %s %s\n", source, id)
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
