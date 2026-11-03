package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"unicode/utf8"

	"github.com/pmlucero/retrace/internal/cassette"
)

// rawInteraction is one interaction with its stored payloads, as
// `show --raw` dumps it.
type rawInteraction struct {
	in       cassette.Interaction
	request  rawBody
	response rawBody
	chunks   []cassette.Chunk
}

// rawBody is a body as stored: absent when the interaction has no blob key,
// missing when its key names no blob.
type rawBody struct {
	data    []byte
	absent  bool
	missing string // the key of a blob that is not in the cassette
}

// errNoInteraction says that the session has no interaction with the id
// given to --raw.
var errNoInteraction = errors.New("no such interaction")

// readRawInteraction reads interaction id of the cassette at path, with its
// bodies and chunks. It opens the file with OpenForRead, like readSession,
// so that a live recording's WAL is left alone.
func readRawInteraction(ctx context.Context, path string, id int64, env *Env) (rawInteraction, error) {
	c, err := cassette.OpenForRead(ctx, path)
	if err != nil {
		return rawInteraction{}, err
	}
	defer func() {
		if err := c.Close(); err != nil {
			env.Logger.Debug("closing a cassette after reading it", "file", path, "error", err)
		}
	}()
	in, err := c.Interaction(ctx, id)
	if errors.Is(err, cassette.ErrNotFound) {
		return rawInteraction{}, errNoInteraction
	}
	if err != nil {
		return rawInteraction{}, err
	}
	raw := rawInteraction{in: in}
	if raw.request, err = readBody(ctx, c, in.RequestBlob); err != nil {
		return rawInteraction{}, err
	}
	if in.Streamed {
		raw.chunks, err = c.Chunks(ctx, id)
	} else {
		raw.response, err = readBody(ctx, c, in.ResponseBlob)
	}
	if err != nil {
		return rawInteraction{}, err
	}
	return raw, nil
}

func readBody(ctx context.Context, c *cassette.Cassette, key *string) (rawBody, error) {
	if key == nil {
		return rawBody{absent: true}, nil
	}
	data, err := c.Blob(ctx, *key)
	if errors.Is(err, cassette.ErrNotFound) {
		// An untrusted cassette can name a blob it does not hold: the rest of
		// the interaction can still be shown.
		return rawBody{missing: *key}, nil
	}
	return rawBody{data: data}, err
}

// writeShowRaw dumps one interaction (spec §9.6): its metadata, its headers,
// and its bodies or SSE chunks as stored. Text is escaped with printable, or
// printableText for bodies, because the cassette is untrusted (SECURITY.md).
func writeShowRaw(w io.Writer, raw rawInteraction) error {
	in := raw.in
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	row := func(name, value string) { fmt.Fprintf(tw, "%s\t%s\n", name, printable(value)) }
	optInt := func(name string, v *int64) {
		if v != nil {
			row(name, strconv.FormatInt(*v, 10))
		}
	}
	optString := func(name string, v *string) {
		if v != nil {
			row(name, *v)
		}
	}

	row("id", strconv.FormatInt(in.ID, 10))
	row("channel", in.Channel)
	row("upstream", in.Upstream)
	row("request", in.HTTPMethod+" "+in.Path)
	if in.StatusCode != nil {
		row("status_code", strconv.Itoa(*in.StatusCode))
	}
	row("started_at", in.StartedAt.In(displayLocation).Format(createdLayout))
	optInt("ttfb_ms", in.TTFBMillis)
	optInt("duration_ms", in.DurationMillis)
	row("provenance", in.Provenance)
	optInt("source_interaction_id", in.SourceInteractionID)
	optString("error", in.Error)
	optString("fingerprint", in.Fingerprint)
	if err := tw.Flush(); err != nil {
		return err
	}

	section := func(title string) { fmt.Fprintf(&buf, "\n--- %s ---\n", title) }
	section("request headers")
	writeHeaders(&buf, in.RequestHeaders)
	section("request body")
	writeBody(&buf, raw.request)
	section("response headers")
	writeHeaders(&buf, in.ResponseHeaders)
	if !in.Streamed {
		section("response body")
		writeBody(&buf, raw.response)
	} else {
		section("response chunks")
		if len(raw.chunks) == 0 {
			buf.WriteString("(none)\n")
		}
		for _, ch := range raw.chunks {
			fmt.Fprintf(&buf, "+%dms\n", ch.OffsetMillis)
			writeText(&buf, ch.Data)
		}
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// writeHeaders writes stored headers, already redacted (ADR 0008), one
// "name: value" line per value, sorted by name. Headers that are not the
// JSON object the proxy stores are written as they are.
func writeHeaders(buf *bytes.Buffer, stored json.RawMessage) {
	if len(stored) == 0 {
		buf.WriteString("(none)\n")
		return
	}
	var h map[string][]string
	if err := json.Unmarshal(stored, &h); err != nil {
		buf.WriteString(printable(string(stored)) + "\n")
		return
	}
	if len(h) == 0 {
		buf.WriteString("(none)\n")
		return
	}
	for _, name := range slices.Sorted(maps.Keys(h)) {
		for _, v := range h[name] {
			fmt.Fprintf(buf, "%s: %s\n", printable(name), printable(v))
		}
	}
}

func writeBody(buf *bytes.Buffer, b rawBody) {
	switch {
	case b.absent:
		buf.WriteString("(none)\n")
	case b.missing != "":
		fmt.Fprintf(buf, "(none: blob %s is not in the cassette)\n", printable(b.missing))
	case len(b.data) == 0:
		buf.WriteString("(empty)\n")
	default:
		writeText(buf, b.data)
	}
}

// writeText writes a body or chunk as stored, ending in a newline. Bytes
// that are not valid UTF-8 are not written (spec §9.6), and valid text goes
// through printableText.
func writeText(buf *bytes.Buffer, data []byte) {
	if !utf8.Valid(data) {
		fmt.Fprintf(buf, "<%d bytes, not UTF-8>\n", len(data))
		return
	}
	text := printableText(string(data))
	buf.WriteString(text)
	if !strings.HasSuffix(text, "\n") {
		buf.WriteString("\n")
	}
}
