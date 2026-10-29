package replay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/match"
	"github.com/pmlucero/retrace/internal/mcp"
	"github.com/pmlucero/retrace/internal/redact"
)

const testServer = "fs"

// mcpRecorded is one interaction of a source cassette with the MCP server
// testServer. Bodies are stored as given: a test with body rules passes them
// redacted, as record would have stored them.
type mcpRecorded struct {
	request    string
	body       string      // non-streamed response body
	chunks     []string    // SSE events of a streamed response, if set
	header     http.Header // response headers; nil selects JSON or SSE
	noResponse bool        // a transport error
	incomplete bool
}

func mcpFingerprint(body []byte) string { return match.Fingerprint(mcp.Canonicalize(testServer, body)) }

// mcpSourceCassette writes recs as the recorded interactions 1..n of a
// source cassette.
func mcpSourceCassette(t *testing.T, recs ...mcpRecorded) *cassette.Cassette {
	t.Helper()
	ctx := context.Background()
	c := newCassette(t, "record")
	err := c.Write(ctx, func(tx *cassette.Tx) error {
		for i, r := range recs {
			id := int64(i + 1)
			req, err := tx.PutBlob(ctx, []byte(r.request))
			if err != nil {
				return err
			}
			if err := tx.BeginInteraction(ctx, cassette.Interaction{ID: id, Channel: "mcp", Upstream: "mcp:" + testServer,
				HTTPMethod: http.MethodPost, Path: "/", RequestBlob: &req,
				StartedAt: time.Now(), Provenance: "recorded"}); err != nil {
				return err
			}
			header := r.header
			if header == nil {
				header = http.Header{"Content-Type": {"application/json"}}
				if r.chunks != nil {
					header = http.Header{"Content-Type": {"text/event-stream"}}
				}
			}
			hdr, err := json.Marshal(header)
			if err != nil {
				return err
			}
			status := http.StatusOK
			done := cassette.Completion{StatusCode: &status, DurationMillis: 10, ResponseHeaders: hdr}
			switch {
			case r.noResponse:
				msg := "dial tcp: connection refused"
				done = cassette.Completion{DurationMillis: 10, Error: &msg}
			case r.chunks != nil:
				done.Streamed = true
				for j, ev := range r.chunks {
					if err := tx.AppendChunk(ctx, cassette.Chunk{InteractionID: id, Index: j, Data: []byte(ev)}); err != nil {
						return err
					}
				}
			default:
				key, err := tx.PutBlob(ctx, []byte(r.body))
				if err != nil {
					return err
				}
				done.ResponseBlob = &key
			}
			if r.incomplete {
				msg := cassette.RecordingIncomplete + ": 1 record(s) not written: cassette writer queue full"
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
	return c
}

// mcpHarness replays a source cassette through an MCPHandler into a new
// session.
type mcpHarness struct {
	t      *testing.T
	h      *MCPHandler
	queues *match.Queues
	out    *cassette.Cassette
	w      *cassette.Writer
	server *httptest.Server
}

func newMCPHarness(t *testing.T, src *cassette.Cassette, rules *redact.Rules) *mcpHarness {
	t.Helper()
	ctx := context.Background()
	ins, err := src.Interactions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	queues := match.NewQueues(ins, func(in cassette.Interaction) (string, bool) {
		b, err := src.Blob(ctx, *in.RequestBlob)
		if err != nil {
			t.Errorf("reading request %d: %v", in.ID, err)
			return "", false
		}
		return mcpFingerprint(b), true
	})
	out := newCassette(t, "exact")
	w, err := cassette.NewWriter(ctx, out, cassette.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	h := NewMCPHandler(MCPConfig{Server: testServer, Source: src, Queues: queues, Redact: rules,
		Fingerprint: mcpFingerprint, Writer: w})
	server := httptest.NewServer(http.StripPrefix("/mcp/"+testServer, h))
	t.Cleanup(server.Close)
	return &mcpHarness{t: t, h: h, queues: queues, out: out, w: w, server: server}
}

// post sends body with the session id, if any, and returns the response.
func (m *mcpHarness) post(session, body string) (*http.Response, string) {
	m.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		m.server.URL+"/mcp/"+testServer, strings.NewReader(body))
	if err != nil {
		m.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if session != "" {
		req.Header.Set(sessionHeader, session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		m.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		m.t.Fatal(err)
	}
	return resp, string(b)
}

// stop ends the new session and returns what it recorded.
func (m *mcpHarness) stop() ([]cassette.Interaction, []cassette.Event) {
	m.t.Helper()
	ctx := context.Background()
	m.server.Close()
	if err := m.w.Close(ctx, cassette.SessionEnd{Status: "completed", EndedAt: time.Now()}); err != nil {
		m.t.Fatalf("closing writer: %v", err)
	}
	ins, err := m.out.Interactions(ctx)
	if err != nil {
		m.t.Fatal(err)
	}
	events, err := m.out.Events(ctx)
	if err != nil {
		m.t.Fatal(err)
	}
	return ins, events
}

func (m *mcpHarness) blob(key *string) string {
	m.t.Helper()
	if key == nil {
		m.t.Fatal("blob reference is nil")
	}
	b, err := m.out.Blob(context.Background(), *key)
	if err != nil {
		m.t.Fatal(err)
	}
	return string(b)
}

func (m *mcpHarness) diverged() bool {
	select {
	case <-m.h.Diverged():
		return true
	default:
		return false
	}
}

func storedHeader(t *testing.T, in cassette.Interaction) http.Header {
	t.Helper()
	var h http.Header
	if err := json.Unmarshal(in.ResponseHeaders, &h); err != nil {
		t.Fatalf("response headers %s: %v", in.ResponseHeaders, err)
	}
	return h
}

// AC-11: replayed responses carry the incoming ids, initialize gets a fresh
// session id, and later responses echo the session id retrace issued.
func TestMCPHandlerRewritesIDsAndSessions(t *testing.T) {
	recSession := http.Header{"Content-Type": {"application/json"}, sessionHeader: {"recorded-session"}}
	const (
		initReq  = `{"jsonrpc":"2.0","id":%s,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`
		initResp = `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-06-18"}}`
		listReq  = `{"jsonrpc":"2.0","id":%s,"method":"tools/list"}`
		listResp = `{"jsonrpc":"2.0", "id":%s, "result":{"tools":[]}}`
	)
	src := mcpSourceCassette(t,
		mcpRecorded{request: fmtID(initReq, "0"), body: fmtID(initResp, "0"), header: recSession},
		mcpRecorded{request: fmtID(listReq, "1"), body: fmtID(listResp, "1"), header: recSession},
		mcpRecorded{request: fmtID(initReq, "0"), body: fmtID(initResp, "0"), header: recSession},
	)
	m := newMCPHarness(t, src, nil)

	resp, body := m.post("", fmtID(initReq, `"a"`))
	if body != fmtID(initResp, `"a"`) {
		t.Errorf("initialize got %s, want the recorded response with id \"a\"", body)
	}
	if resp.ContentLength != int64(len(body)) {
		t.Errorf("Content-Length %d, want %d, the length served", resp.ContentLength, len(body))
	}
	first := resp.Header.Get(sessionHeader)
	if first == "" || first == "recorded-session" {
		t.Fatalf("Mcp-Session-Id %q, want a fresh one", first)
	}

	resp, body = m.post(first, fmtID(listReq, "42"))
	if body != fmtID(listResp, "42") {
		t.Errorf("tools/list got %s, want the recorded response with id 42", body)
	}
	if got := resp.Header.Get(sessionHeader); got != first {
		t.Errorf("Mcp-Session-Id %q, want the issued %q", got, first)
	}

	resp, _ = m.post(first, fmtID(initReq, "7"))
	second := resp.Header.Get(sessionHeader)
	if second == "" || second == first || second == "recorded-session" {
		t.Errorf("second initialize got Mcp-Session-Id %q, want a fresh one", second)
	}

	ins, events := m.stop()
	if len(events) != 0 || m.diverged() {
		t.Errorf("got events %+v, want no divergence", events)
	}
	if len(ins) != 3 {
		t.Fatalf("recorded %d interactions, want 3", len(ins))
	}
	for i, in := range ins {
		if in.Channel != "mcp" || in.Upstream != "mcp:fs" || in.Provenance != "replayed" ||
			in.SourceInteractionID == nil || *in.SourceInteractionID != int64(i+1) {
			t.Errorf("interaction %d: %s %s %s from %v, want mcp mcp:fs replayed from %d",
				i, in.Channel, in.Upstream, in.Provenance, in.SourceInteractionID, i+1)
		}
	}
	// The new session stores what the agent got.
	if got := m.blob(ins[1].ResponseBlob); got != fmtID(listResp, "42") {
		t.Errorf("stored response %s, want the one served", got)
	}
	if got := storedHeader(t, ins[0]).Get(sessionHeader); got != first {
		t.Errorf("stored Mcp-Session-Id %q, want the one served, %q", got, first)
	}
}

func fmtID(format, id string) string { return strings.Replace(format, "%s", id, 1) }

// Spec §5 step 3: a session id retrace did not issue is not echoed, and a
// recorded response without one gets none.
func TestMCPHandlerSessionHeader(t *testing.T) {
	withSession := http.Header{"Content-Type": {"application/json"}, sessionHeader: {"recorded-session"}}
	req := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	resp := `{"jsonrpc":"2.0","id":1,"result":{}}`
	for _, tc := range []struct {
		name     string
		recorded http.Header
		incoming string
	}{
		{"unknown session id", withSession, "other"},
		{"no session id", withSession, ""},
		{"none recorded", nil, "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMCPHarness(t, mcpSourceCassette(t, mcpRecorded{request: req, body: resp, header: tc.recorded}), nil)
			got, _ := m.post(tc.incoming, req)
			if v := got.Header.Values(sessionHeader); len(v) != 0 {
				t.Errorf("Mcp-Session-Id %v, want none", v)
			}
			ins, _ := m.stop()
			if v := storedHeader(t, ins[0]).Values(sessionHeader); len(v) != 0 {
				t.Errorf("stored Mcp-Session-Id %v, want none", v)
			}
		})
	}
}

// AC-10, AC-11: an SSE response is replayed event by event with only the ids
// changed, and a batch gets each request's id.
func TestMCPHandlerRewritesStreamsAndBatches(t *testing.T) {
	src := mcpSourceCassette(t,
		mcpRecorded{
			request: `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"read"}}`,
			chunks: []string{
				"event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n",
				"event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":3,\"result\":{}}\n\n",
			},
		},
		mcpRecorded{
			request: `[{"jsonrpc":"2.0","id":1,"method":"a"},{"jsonrpc":"2.0","id":2,"method":"b"}]`,
			body:    `[{"jsonrpc":"2.0","id":2,"result":"b"},{"jsonrpc":"2.0","id":1,"result":"a"}]`,
		},
	)
	m := newMCPHarness(t, src, nil)

	wantStream := "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n" +
		"event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":\"call-9\",\"result\":{}}\n\n"
	if _, body := m.post("", `{"jsonrpc":"2.0","id":"call-9","method":"tools/call","params":{"name":"read"}}`); body != wantStream {
		t.Errorf("stream got %q, want %q", body, wantStream)
	}
	wantBatch := `[{"jsonrpc":"2.0","id":20,"result":"b"},{"jsonrpc":"2.0","id":10,"result":"a"}]`
	if _, body := m.post("", `[{"jsonrpc":"2.0","id":10,"method":"a"},{"jsonrpc":"2.0","id":20,"method":"b"}]`); body != wantBatch {
		t.Errorf("batch got %s, want %s", body, wantBatch)
	}

	ins, _ := m.stop()
	chunks, err := m.out.Chunks(context.Background(), ins[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(concat(chunks)); got != wantStream {
		t.Errorf("stored chunks %q, want the events served", got)
	}
}

// Spec §3.5: an id the body rules select reaches the agent but not the new
// cassette, which stores it redacted as it stored the incoming request.
func TestMCPHandlerStoresRedactedIDs(t *testing.T) {
	path, err := redact.ParsePath("$..id")
	if err != nil {
		t.Fatal(err)
	}
	rules := redact.NewRules(nil, []redact.Path{path})
	stored := func(s string) string {
		b, err := rules.Body([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	src := mcpSourceCassette(t, mcpRecorded{
		request: stored(`{"jsonrpc":"2.0","id":"rec-id","method":"ping"}`),
		body:    stored(`{"jsonrpc":"2.0","id":"rec-id","result":{}}`),
	})
	m := newMCPHarness(t, src, rules)

	const served = `{"jsonrpc":"2.0","id":"secret-id","result":{}}`
	if _, body := m.post("", `{"jsonrpc":"2.0","id":"secret-id","method":"ping"}`); body != served {
		t.Errorf("got %s, want %s", body, served)
	}
	ins, _ := m.stop()
	if got := m.blob(ins[0].ResponseBlob); got != stored(served) {
		t.Errorf("stored response %s, want %s", got, stored(served))
	}
}

// missError is a JSON-RPC error response to a cassette miss.
type missError struct {
	ID    json.RawMessage `json:"id"`
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    string `json:"data"`
	} `json:"error"`
}

func checkMissError(t *testing.T, e missError, wantID, wantData string) {
	t.Helper()
	if string(e.ID) != wantID || e.Error.Code != -32099 || e.Error.Message != "retrace: cassette miss" ||
		!strings.Contains(e.Error.Data, wantData) {
		t.Errorf("miss error %+v, want id %s, code -32099 and data saying %q", e, wantID, wantData)
	}
}

// AC-31
func TestMCPHandlerMiss(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request string
		wantIDs []string // nil: a single error, not an array
		wantID  string
	}{
		{name: "request", request: `{"jsonrpc":"2.0","id":7,"method":"tools/list"}`, wantID: "7"},
		{name: "string id", request: `{"jsonrpc":"2.0","id":"x","method":"tools/list"}`, wantID: `"x"`},
		{
			name:    "batch",
			request: `[{"jsonrpc":"2.0","id":1,"method":"a"},{"jsonrpc":"2.0","method":"n"},{"jsonrpc":"2.0","id":"2","method":"b"}]`,
			wantIDs: []string{"1", `"2"`},
		},
		{name: "not JSON", request: `{"id":7,`, wantID: "null"},
		{name: "invalid message", request: `{"id":7}`, wantID: "null"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := mcpSourceCassette(t, mcpRecorded{request: `{"jsonrpc":"2.0","id":1,"method":"ping"}`, body: `{}`})
			m := newMCPHarness(t, src, nil)
			resp, body := m.post("", tc.request)
			if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/json" {
				t.Errorf("got %d %s, want 200 application/json", resp.StatusCode, resp.Header.Get("Content-Type"))
			}
			if tc.wantIDs != nil {
				var errs []missError
				if err := json.Unmarshal([]byte(body), &errs); err != nil || len(errs) != len(tc.wantIDs) {
					t.Fatalf("body %s: want %d errors (%v)", body, len(tc.wantIDs), err)
				}
				for i, id := range tc.wantIDs {
					checkMissError(t, errs[i], id, "no recorded request matches")
				}
			} else {
				var e missError
				if err := json.Unmarshal([]byte(body), &e); err != nil {
					t.Fatalf("body %s: %v", body, err)
				}
				checkMissError(t, e, tc.wantID, "no recorded request matches")
			}
			if !m.diverged() {
				t.Error("Diverged not closed after a miss")
			}
			ins, events := m.stop()
			res := divergenceOn(t, events, "mcp", "mcp:fs")
			if want := mcpFingerprint([]byte(tc.request)); res["received_fingerprint"] != want {
				t.Errorf("received_fingerprint %v, want %s", res["received_fingerprint"], want)
			}
			if len(ins) != 0 {
				t.Errorf("recorded %d interactions, want none", len(ins))
			}
		})
	}
}

// AC-31: the head of the queue is an incomplete recording.
func TestMCPHandlerMissOnIncompleteRecording(t *testing.T) {
	const req = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	m := newMCPHarness(t, mcpSourceCassette(t, mcpRecorded{request: req, body: `{}`, incomplete: true}), nil)
	_, body := m.post("", `{"jsonrpc":"2.0","id":5,"method":"tools/list"}`)
	var e missError
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("body %s: %v", body, err)
	}
	checkMissError(t, e, "5", "recording of the matching interaction 1 is incomplete")
	if un := m.queues.Unconsumed(); len(un) != 0 {
		t.Errorf("unconsumed %v, want none: the incomplete interaction is not reported", un)
	}
	_, events := m.stop()
	if res := divergenceOn(t, events, "mcp", "mcp:fs"); res["incomplete_interaction"] != float64(1) {
		t.Errorf("incomplete_interaction %v, want 1", res["incomplete_interaction"])
	}
}

// AC-29 (MCP)
func TestMCPHandlerServesATransportError(t *testing.T) {
	const req = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	m := newMCPHarness(t, mcpSourceCassette(t, mcpRecorded{request: req, noResponse: true}), nil)
	resp, body := m.post("", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if resp.StatusCode != http.StatusBadGateway || !strings.HasPrefix(body, "retrace: upstream request failed: ") ||
		!strings.Contains(body, "dial tcp: connection refused") {
		t.Errorf("got %d %q, want 502 with the recorded error", resp.StatusCode, body)
	}
	if m.diverged() {
		t.Error("Diverged closed by a recorded transport error")
	}
	ins, events := m.stop()
	if len(events) != 0 {
		t.Errorf("got events %+v, want no divergence", events)
	}
	if len(ins) != 1 {
		t.Fatalf("recorded %d interactions, want 1", len(ins))
	}
	in := ins[0]
	if in.Provenance != "replayed" || in.StatusCode != nil || in.Error == nil || *in.Error != "dial tcp: connection refused" ||
		in.SourceInteractionID == nil || *in.SourceInteractionID != 1 {
		t.Errorf("interaction %+v, want replayed from 1 with no status and the source's error", in)
	}
}
