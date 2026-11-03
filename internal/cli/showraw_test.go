package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
)

// rawSeed is one interaction to seed, with its bodies and chunks. A nil body
// stores no blob key.
type rawSeed struct {
	in       cassette.Interaction
	done     *cassette.Completion
	request  []byte
	response []byte
	chunks   []cassette.Chunk
}

// seedRawSession writes session "s" with the given interactions.
func seedRawSession(t *testing.T, dir string, seeds ...rawSeed) {
	t.Helper()
	seedSession(t, dir, cassette.Session{ID: "s", Mode: "record", Status: "recording", CreatedAt: lsT0}, nil)
	ctx := context.Background()
	c, err := cassette.Open(ctx, cassette.Path(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	err = c.Write(ctx, func(tx *cassette.Tx) error {
		for _, sd := range seeds {
			if sd.request != nil {
				key, err := tx.PutBlob(ctx, sd.request)
				if err != nil {
					return err
				}
				sd.in.RequestBlob = &key
			}
			if err := tx.BeginInteraction(ctx, sd.in); err != nil {
				return err
			}
			for _, ch := range sd.chunks {
				ch.InteractionID = sd.in.ID
				if err := tx.AppendChunk(ctx, ch); err != nil {
					return err
				}
			}
			if sd.done == nil {
				continue
			}
			if sd.response != nil {
				key, err := tx.PutBlob(ctx, sd.response)
				if err != nil {
					return err
				}
				sd.done.ResponseBlob = &key
			}
			if err := tx.CompleteInteraction(ctx, sd.in.ID, *sd.done); err != nil {
				return err
			}
		}
		return nil
	})
	if cerr := c.Close(); err != nil || cerr != nil {
		t.Fatal(err, cerr)
	}
}

func ptrTo[T any](v T) *T { return &v }

func rawSeeds() []rawSeed {
	return []rawSeed{
		{
			in: cassette.Interaction{
				ID: 1, Channel: "llm", Upstream: "anthropic", HTTPMethod: "POST", Path: "/v1/messages",
				RequestHeaders: json.RawMessage(`{"X-Api-Key":["[REDACTED:1a2b3c4d]"],"Content-Type":["application/json"]}`),
				Fingerprint:    ptrTo("fp-1"), StartedAt: lsT0.Add(time.Second), Provenance: "recorded",
			},
			done: &cassette.Completion{
				StatusCode: ptrTo(200), ResponseHeaders: json.RawMessage(`{"Content-Type":["application/json"]}`),
				TTFBMillis: ptrTo[int64](120), DurationMillis: 450,
			},
			request:  []byte("{\n\t\"model\": \"claude-sonnet-4-5\"\n}"),
			response: []byte(`{"stop_reason":"end_turn"}` + "\n"),
		},
		{
			in: cassette.Interaction{
				ID: 2, Channel: "llm", Upstream: "anthropic", HTTPMethod: "POST", Path: "/v1/messages",
				RequestHeaders: json.RawMessage(`{}`), StartedAt: lsT0.Add(2 * time.Second), Provenance: "recorded",
			},
			done: &cassette.Completion{
				StatusCode: ptrTo(200), ResponseHeaders: json.RawMessage(`{"Content-Type":["text/event-stream"]}`),
				Streamed: true, TTFBMillis: ptrTo[int64](40), DurationMillis: 900,
			},
			request: []byte(`{"stream":true}`),
			chunks: []cassette.Chunk{
				{Index: 0, OffsetMillis: 40, Data: []byte("event: message_start\ndata: {}\n\n")},
				{Index: 1, OffsetMillis: 300, Data: []byte("data: \xff\xfe\n\n")},
				{Index: 2, OffsetMillis: 900, Data: []byte("event: message_stop\ndata: {}\n\n")},
			},
		},
		{
			in: cassette.Interaction{
				ID: 3, Channel: "mcp", Upstream: "mcp:shop", HTTPMethod: "POST", Path: "/mcp",
				RequestHeaders: json.RawMessage(`{"Mcp-Session-Id":["abc"]}`), StartedAt: lsT0.Add(3 * time.Second),
				Provenance: "replayed", SourceInteractionID: ptrTo[int64](7),
			},
			done: &cassette.Completion{
				StatusCode: ptrTo(500), ResponseHeaders: json.RawMessage(`{}`), DurationMillis: 5,
				Error: ptrTo("upstream\x1b[31m failed"),
			},
			request:  []byte("{\"q\":\"\x1b]0;pwned\x07\r\u202eexe\"}"),
			response: []byte{0x00, 0xff, 0x10},
		},
		{
			// In flight: nothing of the response is recorded yet.
			in: cassette.Interaction{
				ID: 4, Channel: "llm", Upstream: "anthropic", HTTPMethod: "POST", Path: "/v1/messages",
				StartedAt: lsT0.Add(4 * time.Second), Provenance: "recorded",
			},
		},
	}
}

// AC-22: show --raw dumps one interaction as stored, with its headers, its
// bodies or its SSE chunks with their offsets.
func TestShowRaw(t *testing.T) {
	utcDisplay(t)
	dir := t.TempDir()
	seedRawSession(t, dir, rawSeeds()...)

	for _, tc := range []struct {
		id, golden string
	}{
		{"1", "show/raw-body.txt"},
		{"2", "show/raw-streamed.txt"},
		{"3", "show/raw-untrusted.txt"},
		{"4", "show/raw-in-flight.txt"},
	} {
		t.Run(tc.golden, func(t *testing.T) {
			for _, args := range [][]string{{"s", "--raw", tc.id}, {"--raw=" + tc.id, "s"}} {
				r := runShowCmd(dir, args...)
				if r.code != ExitOK {
					t.Fatalf("%v: exit code %d, stderr:\n%s", args, r.code, r.stderr)
				}
				checkGolden(t, tc.golden, r.stdout.String())
			}
		})
	}
}

// SECURITY.md: a body from an untrusted cassette cannot drive the terminal.
// Newlines and tabs keep the layout; other control characters and bidi
// formatting characters are escaped.
func TestShowRawEscapesBodies(t *testing.T) {
	dir := t.TempDir()
	seedRawSession(t, dir, rawSeeds()...)
	r := runShowCmd(dir, "s", "--raw", "3")
	if r.code != ExitOK {
		t.Fatalf("exit code %d, stderr:\n%s", r.code, r.stderr)
	}
	out := r.stdout.String()
	for _, bad := range []string{"\x1b", "\x07", "\r", "\u202e", "\x00"} {
		if strings.Contains(out, bad) {
			t.Errorf("output holds %q:\n%q", bad, out)
		}
	}
	for _, want := range []string{`{"q":"\x1b]0;pwned\a\r\u202eexe"}`, "<3 bytes, not UTF-8>", `upstream\x1b[31m failed`} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %s:\n%s", want, out)
		}
	}
}

func TestWriteBody(t *testing.T) {
	for _, tc := range []struct {
		name string
		body rawBody
		want string
	}{
		{"absent", rawBody{absent: true}, "(none)\n"},
		{"blob not in the cassette", rawBody{missing: "ab12"}, "(none: blob ab12 is not in the cassette)\n"},
		{"empty", rawBody{data: []byte{}}, "(empty)\n"},
		{"text keeps its layout", rawBody{data: []byte("a\n\tb")}, "a\n\tb\n"},
		{"not UTF-8", rawBody{data: []byte("ok\xc3")}, "<3 bytes, not UTF-8>\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			writeBody(&buf, tc.body)
			if buf.String() != tc.want {
				t.Errorf("writeBody = %q, want %q", buf.String(), tc.want)
			}
		})
	}
}

func TestPrintableText(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"line 1\n\tline 2", "line 1\n\tline 2"},
		{"\x1b[2J\r\n", `\x1b[2J\r` + "\n"},
		{"\u2067x", `\u2067x`},
	} {
		if got := printableText(tc.in); got != tc.want {
			t.Errorf("printableText(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestShowRawErrors(t *testing.T) {
	dir := t.TempDir()
	seedRawSession(t, dir, rawSeeds()...)
	for _, tc := range []struct {
		name   string
		args   []string
		code   int
		stderr string
	}{
		{"with --json", []string{"s", "--raw", "1", "--json"}, ExitUsage, "--raw and --json cannot be used together"},
		{"not a number", []string{"s", "--raw", "one"}, ExitUsage, "positive integer"},
		{"zero", []string{"s", "--raw", "0"}, ExitUsage, "positive integer"},
		{"negative", []string{"s", "--raw=-1"}, ExitUsage, "positive integer"},
		{"no value", []string{"s", "--raw"}, ExitUsage, "flag needs an argument"},
		{"missing interaction", []string{"s", "--raw", "99"}, ExitError, "no such interaction"},
		{"missing session", []string{"nope", "--raw", "1"}, ExitError, "no such session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runShowCmd(dir, tc.args...)
			if r.code != tc.code || !strings.Contains(r.stderr.String(), tc.stderr) {
				t.Errorf("exit code %d, stderr %q; want %d and %q", r.code, r.stderr, tc.code, tc.stderr)
			}
		})
	}
}
