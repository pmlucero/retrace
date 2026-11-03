package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/summary"
)

var showCommand = command{
	name:    "show",
	summary: "show a session's summary and its timeline of events",
	run:     runShow,
}

const showUsage = `Usage: retrace show <id> [--json]

Shows session <id>: its metadata and totals, then a timeline of its events.
Times are relative to the session's start. Costs are estimates from
retrace's price table.

Flags:
  --json   emit the events as JSON Lines, one event per line
`

// showArgumentRunes is where show cuts the arguments of an event (spec §9.6).
const showArgumentRunes = 60

// runShow implements `retrace show` (spec §9.6).
func runShow(ctx context.Context, env *Env, args []string) int {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), showUsage) }
	asJSON := fs.Bool("json", false, "")
	positional, code, ok := parseWithPositional(fs, args, env)
	if !ok {
		return code
	}
	if len(positional) != 1 {
		return usageError(env.Stderr, "show needs exactly one session id")
	}
	id := positional[0]
	if err := cassette.ValidateSessionID(id); err != nil {
		return usageError(env.Stderr, err.Error())
	}

	path := cassette.Path(env.Globals.Dir, id)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		env.Logger.Error("no such session", "session", id, "dir", env.Globals.Dir)
		return ExitError
	}
	s, events, err := readSession(ctx, path, env)
	if err != nil {
		env.Logger.Error("cannot read the session", "session", id, "error", err)
		return ExitError
	}
	if *asJSON {
		err = writeShowJSON(env.Stdout, events)
	} else {
		err = writeShowText(env.Stdout, s, events)
	}
	if err != nil {
		env.Logger.Error("cannot write the session", "error", err)
		return ExitError
	}
	return ExitOK
}

func writeShowText(w io.Writer, s cassette.Session, events []cassette.Event) error {
	t := summary.Of(s, events)
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	row := func(name, value string) { fmt.Fprintf(tw, "%s\t%s\n", name, printable(value)) }

	row("session", s.ID)
	row("mode", s.Mode)
	if s.ParentSessionID != nil {
		row("parent", *s.ParentSessionID)
	}
	row("status", s.Status)
	row("created", s.CreatedAt.In(displayLocation).Format(createdLayout))
	if s.EndedAt != nil {
		row("ended", s.EndedAt.In(displayLocation).Format(createdLayout))
	}
	row("command", strings.Join(s.Command, " "))
	row("cwd", s.Cwd)
	if s.AgentExitCode != nil {
		row("exit code", strconv.Itoa(*s.AgentExitCode))
	}
	if s.ModelOverride != nil {
		row("model override", *s.ModelOverride)
	}
	// A list that cannot be read only loses this line: show still has to
	// show the rest of an untrusted cassette.
	if names, err := mcpServerNames(s.MCPServers); err == nil && len(names) > 0 {
		row("mcp servers", strings.Join(names, ", "))
	}
	if len(s.Labels) > 0 {
		row("labels", formatLabels(s.Labels))
	}
	row("recorded by", "retrace "+s.RetraceVersion)
	row("steps", strconv.Itoa(t.Steps))
	row("tool calls", strconv.Itoa(t.ToolCalls))
	row("tokens", summary.FormatCount(t.TokensIn)+" in / "+summary.FormatCount(t.TokensOut)+" out")
	row("cost (est.)", summary.FormatCost(t))
	if len(t.Models) > 0 {
		row("models", strings.Join(t.Models, ", "))
	}
	if t.Duration != nil {
		row("duration", summary.FormatDuration(*t.Duration))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if s.DroppedEvents > 0 {
		// Spec §7.7: the steps and totals above come from an incomplete log.
		fmt.Fprintf(&buf, "\nwarning: the event log is incomplete: %d events were not written, so the totals may be low\n", s.DroppedEvents)
	}
	buf.WriteString("\n")

	tw = tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SEQ\tTIME\tSOURCE → TARGET\tKIND\tTOOL\tARGUMENTS\tSTATUS")
	next, missing := int64(1), s.DroppedEvents
	for _, e := range events {
		if e.Seq > next {
			// The marker sits in the ARGUMENTS column, so the columns stay
			// aligned across it.
			n := e.Seq - next
			missing -= int(n)
			fmt.Fprintf(tw, "…\t\t\t\t\t%s missing (%s)\t\n", plural(n, "event"), seqRange(next, e.Seq-1))
		}
		next = e.Seq + 1
		var tool string
		if e.Tool != nil {
			tool = *e.Tool
		}
		fmt.Fprintf(tw, "%d\t+%s\t%s → %s\t%s\t%s\t%s\t%s\n",
			e.Seq, summary.FormatDuration(e.StartedAt.Sub(s.CreatedAt)), printable(e.Source), printable(e.Target),
			printable(e.Kind), printable(tool), showArguments(e.Arguments), printable(e.Status))
	}
	if missing > 0 {
		// The count has events that left no gap in seq: dropped after the
		// last event written, or recovered by a reindex that renumbered the
		// log (spec §7.6). Their seqs are unknown, so none are named.
		where := ""
		if next > 1 {
			where = fmt.Sprintf(" after seq %d", next-1)
		}
		fmt.Fprintf(tw, "…\t\t\t\t\t%s missing%s\t\n", plural(int64(missing), "event"), where)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	return writeTrimmed(w, buf.String())
}

// printable makes text from an untrusted cassette (SECURITY.md) safe to print
// in a table: invalid UTF-8 becomes U+FFFD, and control characters and
// bidirectional formatting characters are escaped in Go syntax ("\x1b",
// "\n", "\u202e"), so they can neither drive the terminal nor break the
// columns.
func printable(s string) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	if !strings.ContainsFunc(s, unsafeRune) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if unsafeRune(r) {
			q := strconv.QuoteRuneToASCII(r)
			b.WriteString(q[1 : len(q)-1])
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func unsafeRune(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r)
}

// parseWithPositional parses a command's flags around its positional
// arguments, which flag.Parse would stop at: `show <id> --json` and
// `show --json <id>` are the same. It returns ok=false with the exit code
// when parsing ends the command.
func parseWithPositional(fs *flag.FlagSet, args []string, env *Env) (positional []string, code int, ok bool) {
	for {
		if code, ok := parseCommandFlags(fs, args, env); !ok {
			return nil, code, false
		}
		if fs.NArg() == 0 {
			return positional, 0, true
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// showArguments renders arguments as compact JSON cut after
// showArgumentRunes runes (spec §9.6). Stored bytes that are not JSON, which
// a valid cassette never holds, are shown through printable, cut the same
// way. JSON needs no escaping: its strings keep their escape sequences.
func showArguments(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var b bytes.Buffer
	text := string(raw)
	if json.Compact(&b, raw) == nil {
		text = b.String()
	}
	text = strings.ToValidUTF8(text, "�")
	if utf8.RuneCountInString(text) <= showArgumentRunes {
		return text
	}
	return string([]rune(text)[:showArgumentRunes]) + "…"
}

func formatLabels(labels map[string]string) string {
	pairs := make([]string, 0, len(labels))
	for k, v := range labels {
		pairs = append(pairs, k+"="+v)
	}
	slices.Sort(pairs)
	return strings.Join(pairs, ", ")
}

func plural(n int64, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func seqRange(from, to int64) string {
	if from == to {
		return fmt.Sprintf("seq %d", from)
	}
	return fmt.Sprintf("seq %d–%d", from, to)
}

// showEvent is one line of `show --json`: an event with the columns of the
// events table (spec §7.4), which is the stable format.
type showEvent struct {
	Seq              int64           `json:"seq"`
	ParentSeq        *int64          `json:"parent_seq"`
	InteractionID    *int64          `json:"interaction_id"`
	Kind             string          `json:"kind"`
	Source           string          `json:"source"`
	Target           string          `json:"target"`
	StartedAt        time.Time       `json:"started_at"`
	DurationMillis   *int64          `json:"duration_ms"`
	Model            *string         `json:"model"`
	RequestedModel   *string         `json:"requested_model"`
	Tool             *string         `json:"tool"`
	Arguments        json.RawMessage `json:"arguments"`
	Result           json.RawMessage `json:"result"`
	TokensIn         *int64          `json:"tokens_in"`
	TokensOut        *int64          `json:"tokens_out"`
	TokensCacheRead  *int64          `json:"tokens_cache_read"`
	TokensCacheWrite *int64          `json:"tokens_cache_write"`
	CostUSD          *float64        `json:"cost_usd"`
	Status           string          `json:"status"`
	Error            *string         `json:"error"`
	Provenance       string          `json:"provenance"`
	DeriverVersion   int             `json:"deriver_version"`
}

// writeShowJSON writes the events as JSON Lines. Only the events that exist
// are written: a consumer finds gaps from seq (spec §9.6).
func writeShowJSON(w io.Writer, events []cassette.Event) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, e := range events {
		if err := enc.Encode(showEvent{
			Seq: e.Seq, ParentSeq: e.ParentSeq, InteractionID: e.InteractionID, Kind: e.Kind,
			Source: e.Source, Target: e.Target, StartedAt: e.StartedAt, DurationMillis: e.DurationMillis,
			Model: e.Model, RequestedModel: e.RequestedModel, Tool: e.Tool,
			Arguments: jsonOrString(e.Arguments), Result: jsonOrString(e.Result),
			TokensIn: e.TokensIn, TokensOut: e.TokensOut, TokensCacheRead: e.TokensCacheRead,
			TokensCacheWrite: e.TokensCacheWrite, CostUSD: e.CostUSD, Status: e.Status, Error: e.Error,
			Provenance: e.Provenance, DeriverVersion: e.DeriverVersion,
		}); err != nil {
			return err
		}
	}
	return nil
}

// jsonOrString returns raw when it is JSON, null when it is absent, and
// otherwise raw as a JSON string, so that one bad value in an untrusted
// cassette does not stop the stream.
func jsonOrString(raw json.RawMessage) json.RawMessage {
	switch {
	case len(raw) == 0:
		return nil
	case json.Valid(raw):
		return raw
	}
	b, _ := json.Marshal(strings.ToValidUTF8(string(raw), "�"))
	return b
}
