package derive

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
)

// spec §3.4: the initialize result of an interaction, from a JSON or SSE
// response, whatever shape the response has.
func TestInitializeResult(t *testing.T) {
	const req = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	ok := func(id, version, info string) string {
		return `{"jsonrpc":"2.0","id":` + id + `,"result":{"protocolVersion":"` + version + `"` + info + `}}`
	}
	sse := func(data string) string { return "event: message\ndata: " + data + "\n\n" }
	for _, tc := range []struct {
		name        string
		request     string
		response    *string // nil: no response body
		chunks      []string
		wantOK      bool
		wantVersion string
		wantInfo    string
	}{
		{name: "json", request: req, response: ptr(ok("1", "2025-06-18", `,"serverInfo":{"name":"a"}`)), wantOK: true, wantVersion: "2025-06-18", wantInfo: `{"name":"a"}`},
		{name: "sse one event", request: req, chunks: []string{sse(ok("1", "2025-06-18", ""))}, wantOK: true, wantVersion: "2025-06-18"},
		{name: "sse several events, the first answer to the id counts", request: req, chunks: []string{
			sse(`{"jsonrpc":"2.0","method":"notifications/message"}`), sse(ok("2", "1999-01-01", "")), sse(ok("1", "2025-03-26", "")), sse(ok("1", "2025-06-18", "")),
		}, wantOK: true, wantVersion: "2025-03-26"},
		{name: "sse event that is not json-rpc is skipped", request: req, chunks: []string{"data: nope\n\n", sse(ok("1", "2025-06-18", ""))}, wantOK: true, wantVersion: "2025-06-18"},
		{name: "batch", request: `[` + req + `,{"jsonrpc":"2.0","id":2,"method":"tools/list"}]`,
			response: ptr(`[` + ok("2", "1999-01-01", "") + `,` + ok("1", "2025-03-26", "") + `]`), wantOK: true, wantVersion: "2025-03-26"},
		{name: "error result", request: req, response: ptr(`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"no"}}`)},
		{name: "missing protocol version", request: req, response: ptr(`{"jsonrpc":"2.0","id":1,"result":{}}`)},
		{name: "empty protocol version", request: req, response: ptr(ok("1", "", ""))},
		{name: "protocol version not a string", request: req, response: ptr(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":7}}`)},
		{name: "server info null is dropped", request: req, response: ptr(ok("1", "2025-03-26", `,"serverInfo":null`)), wantOK: true, wantVersion: "2025-03-26"},
		{name: "server info not an object is dropped", request: req, response: ptr(ok("1", "2025-03-26", `,"serverInfo":"x"`)), wantOK: true, wantVersion: "2025-03-26"},
		{name: "response answers another id", request: req, response: ptr(ok("2", "2025-03-26", ""))},
		{name: "not an initialize", request: `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, response: ptr(ok("1", "2025-03-26", ""))},
		{name: "opaque request", request: "not json", response: ptr(ok("1", "2025-03-26", ""))},
		{name: "opaque response", request: req, response: ptr("not json")},
		{name: "no response body", request: req},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			c, err := cassette.Create(ctx, filepath.Join(t.TempDir(), "s.cassette"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.Close() })
			streamed := tc.chunks != nil
			err = c.Write(ctx, func(tx *cassette.Tx) error {
				if err := tx.InsertSession(ctx, cassette.Session{ID: "s", Mode: "record", Status: "recording", CreatedAt: time.Now(), OnMiss: "strict"}); err != nil {
					return err
				}
				reqBlob, err := tx.PutBlob(ctx, []byte(tc.request))
				if err != nil {
					return err
				}
				if err := tx.BeginInteraction(ctx, cassette.Interaction{ID: 1, Channel: "mcp", Upstream: "mcp:a", HTTPMethod: "POST", Path: "/",
					RequestBlob: &reqBlob, StartedAt: time.Now(), Provenance: "recorded"}); err != nil {
					return err
				}
				status := 200
				done := cassette.Completion{StatusCode: &status, Streamed: streamed, DurationMillis: 1}
				if tc.response != nil {
					b, err := tx.PutBlob(ctx, []byte(*tc.response))
					if err != nil {
						return err
					}
					done.ResponseBlob = &b
				}
				for i, ch := range tc.chunks {
					if err := tx.AppendChunk(ctx, cassette.Chunk{InteractionID: 1, Index: i, Data: []byte(ch)}); err != nil {
						return err
					}
				}
				return tx.CompleteInteraction(ctx, 1, done)
			})
			if err != nil {
				t.Fatal(err)
			}
			all, err := c.Interactions(ctx)
			if err != nil {
				t.Fatal(err)
			}
			res, got, err := InitializeResult(ctx, c, all[0])
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.wantOK || res.ProtocolVersion != tc.wantVersion || string(res.ServerInfo) != tc.wantInfo {
				t.Errorf("InitializeResult = %q, %s, %t; want %q, %s, %t", res.ProtocolVersion, res.ServerInfo, got, tc.wantVersion, tc.wantInfo, tc.wantOK)
			}
		})
	}
}
