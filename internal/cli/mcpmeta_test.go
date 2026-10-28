package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
)

// spec §3.4, §7.2: the metadata comes from initialize responses that answer
// an initialize request, and the last one of each server counts.
func TestStoreMCPServerInfoReadsInitializeResults(t *testing.T) {
	ctx := context.Background()
	c, err := cassette.Create(ctx, filepath.Join(t.TempDir(), "s.cassette"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	initResp := func(id int, version string) string {
		return `{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"result":{"protocolVersion":"` + version + `","serverInfo":{"name":"a"}}}`
	}
	const initReq = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	exchanges := []struct {
		upstream, request, response string
		incomplete                  bool
	}{
		{"mcp:a", initReq, initResp(1, "2025-03-26"), false},
		{"mcp:a", initReq, initResp(1, "2025-06-18"), false},                                                              // a second initialize: the last counts
		{"mcp:a", initReq, `{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"no"}}`, false},                      // no result
		{"mcp:a", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, initResp(1, "1999-01-01"), false},                     // not an initialize
		{"mcp:a", initReq, initResp(1, "1999-01-02"), true},                                                               // incomplete recording
		{"mcp:a", "not json", initResp(1, "1999-01-03"), false},                                                           // opaque request
		{"mcp:a", initReq, initResp(2, "1999-01-04"), false},                                                              // answers another id
		{"mcp:a", initReq, `{"jsonrpc":"2.0","id":1,"result":{}}`, false},                                                 // no protocol version
		{"mcp:a", initReq, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":7}}`, false},                              // not a string
		{"mcp:b", initReq, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26","serverInfo":null}}`, false}, // no serverInfo object
		{"mcp:other", initReq, initResp(1, "1999-01-05"), false},                                                          // not registered
	}
	servers := json.RawMessage(`[{"name":"a","url":"http://a"},{"name":"b","url":"http://b"}]`)
	err = c.Write(ctx, func(tx *cassette.Tx) error {
		if err := tx.InsertSession(ctx, cassette.Session{ID: "s", Mode: "record", Status: "recording",
			CreatedAt: time.Now(), OnMiss: "strict", MCPServers: servers}); err != nil {
			return err
		}
		for i, ex := range exchanges {
			req, err := tx.PutBlob(ctx, []byte(ex.request))
			if err != nil {
				return err
			}
			resp, err := tx.PutBlob(ctx, []byte(ex.response))
			if err != nil {
				return err
			}
			id := int64(i + 1)
			if err := tx.BeginInteraction(ctx, cassette.Interaction{ID: id, Channel: "mcp", Upstream: ex.upstream,
				HTTPMethod: "POST", Path: "/", RequestBlob: &req, StartedAt: time.Now(), Provenance: "recorded"}); err != nil {
				return err
			}
			status := 200
			done := cassette.Completion{StatusCode: &status, ResponseBlob: &resp, DurationMillis: 1}
			if ex.incomplete {
				msg := cassette.RecordingIncomplete + ": test"
				done.Error = &msg
			}
			if err := tx.CompleteInteraction(ctx, id, done); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := storeMCPServerInfo(ctx, c); err != nil {
		t.Fatal(err)
	}
	s, err := c.Session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"name":"a","url":"http://a","protocol_version":"2025-06-18","server_info":{"name":"a"}},{"name":"b","url":"http://b","protocol_version":"2025-03-26"}]`
	if string(s.MCPServers) != want {
		t.Errorf("mcp_servers = %s\nwant %s", s.MCPServers, want)
	}
}
