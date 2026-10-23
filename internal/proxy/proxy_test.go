package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/redact"
)

// harness runs a Proxy that records into a fresh cassette and forwards
// /llm/anthropic to upstream, or to an address where nothing listens if
// upstream is nil.
type harness struct {
	t      *testing.T
	c      *cassette.Cassette
	w      *cassette.Writer
	server *httptest.Server
}

func newHarness(t *testing.T, upstream http.Handler) *harness {
	t.Helper()
	return newHarnessWith(t, upstream, 0, redact.NewRules(nil, nil))
}

// newHarnessWith is newHarness with a MaxBodyBytes and redaction rules.
func newHarnessWith(t *testing.T, upstream http.Handler, maxBody int64, rules *redact.Rules) *harness {
	t.Helper()
	ctx := context.Background()
	target, _ := url.Parse("http://127.0.0.1:1")
	if upstream != nil {
		up := httptest.NewServer(upstream)
		t.Cleanup(up.Close)
		target, _ = url.Parse(up.URL)
	}

	c, err := cassette.Create(ctx, filepath.Join(t.TempDir(), "s.cassette"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	err = c.Write(ctx, func(tx *cassette.Tx) error {
		return tx.InsertSession(ctx, cassette.Session{ID: "s", Mode: "record", Status: "recording",
			CreatedAt: time.Now(), Command: []string{"agent"}, Cwd: "/", OnMiss: "strict", RetraceVersion: "dev"})
	})
	if err != nil {
		t.Fatal(err)
	}
	w, err := cassette.NewWriter(ctx, c, cassette.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p := New(Config{
		Routes:       []Route{{Prefix: "/llm/anthropic", Channel: "llm", Upstream: "anthropic", Target: target}},
		Writer:       w,
		Redact:       rules,
		MaxBodyBytes: maxBody,
	})
	server := httptest.NewServer(p)
	t.Cleanup(server.Close)
	return &harness{t: t, c: c, w: w, server: server}
}

// interactions ends the session and returns what was recorded. Like record,
// it first waits for in-flight handlers: an exchange completes when the proxy
// closes the upstream body, which can be after the agent has the response.
func (h *harness) interactions() []cassette.Interaction {
	h.t.Helper()
	h.server.Close()
	if err := h.w.Close(context.Background(), cassette.SessionEnd{Status: "completed", EndedAt: time.Now()}); err != nil {
		h.t.Fatalf("closing writer: %v", err)
	}
	all, err := h.c.Interactions(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	return all
}

func (h *harness) blob(key *string) string {
	h.t.Helper()
	if key == nil {
		h.t.Fatal("blob reference is nil")
	}
	b, err := h.c.Blob(context.Background(), *key)
	if err != nil {
		h.t.Fatal(err)
	}
	return string(b)
}

func headerValue(t *testing.T, raw json.RawMessage, name string) string {
	t.Helper()
	var h http.Header
	if err := json.Unmarshal(raw, &h); err != nil {
		t.Fatalf("headers %s: %v", raw, err)
	}
	return h.Get(name)
}

// AC-02
func TestProxyRecordsAJSONExchange(t *testing.T) {
	var gotPath, gotKey, gotEncoding string
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotKey, gotEncoding = r.URL.RequestURI(), r.Header.Get("X-Api-Key"), r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Request-Id", "req_1")
		_, _ = io.WriteString(w, `{"type":"message","content":[]}`)
	}))

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, h.server.URL+"/llm/anthropic/v1/messages?beta=true&api_key=sk-q",
		strings.NewReader(`{"model":"m","messages":[]}`))
	req.Header.Set("X-Api-Key", "sk-ant-secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != 200 || string(body) != `{"type":"message","content":[]}` {
		t.Fatalf("agent got %d %q", resp.StatusCode, body)
	}
	if gotPath != "/v1/messages?beta=true&api_key=sk-q" || gotKey != "sk-ant-secret" {
		t.Errorf("upstream got %s with key %q, want the route stripped and the real key", gotPath, gotKey)
	}
	if gotEncoding != "gzip" {
		t.Errorf("upstream Accept-Encoding = %q, want the transport's own gzip (spec §3.3)", gotEncoding)
	}

	all := h.interactions()
	if len(all) != 1 {
		t.Fatalf("got %d interactions, want 1", len(all))
	}
	in := all[0]
	if in.Channel != "llm" || in.Upstream != "anthropic" || in.HTTPMethod != "POST" ||
		!strings.HasPrefix(in.Path, "/v1/messages?beta=true&api_key=[REDACTED:") {
		t.Errorf("request line = %s %s %s %s, want the query credential redacted (spec §3.5)", in.Channel, in.Upstream, in.HTTPMethod, in.Path)
	}
	if got := h.blob(in.RequestBlob); got != `{"model":"m","messages":[]}` {
		t.Errorf("request body = %q", got)
	}
	if got := h.blob(in.ResponseBlob); got != string(body) {
		t.Errorf("response body = %q, want what the agent got", got)
	}
	if in.StatusCode == nil || *in.StatusCode != 200 || in.Streamed || in.Error != nil {
		t.Errorf("completion = status %v, streamed %v, error %v", in.StatusCode, in.Streamed, in.Error)
	}
	if in.TTFBMillis == nil || in.DurationMillis == nil || *in.DurationMillis < *in.TTFBMillis {
		t.Errorf("timing = ttfb %v, duration %v", in.TTFBMillis, in.DurationMillis)
	}
	if got := headerValue(t, in.RequestHeaders, "X-Api-Key"); !strings.HasPrefix(got, "[REDACTED:") {
		t.Errorf("stored X-Api-Key = %q, want it redacted (AC-06)", got)
	}
	if got := headerValue(t, in.ResponseHeaders, "Request-Id"); got != "req_1" {
		t.Errorf("stored Request-Id = %q", got)
	}
}

// AC-03
func TestProxyStreamsSSEWithoutBuffering(t *testing.T) {
	events := []string{
		"event: message_start\ndata: {\"type\":\"message_start\"}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\"}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}
	firstSeen := make(chan struct{})
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = io.WriteString(w, events[0])
		w.(http.Flusher).Flush()
		// The upstream holds the rest until the agent has the first event:
		// if the proxy buffered, this would never happen.
		select {
		case <-firstSeen:
		case <-time.After(5 * time.Second):
			return
		}
		time.Sleep(20 * time.Millisecond)
		for _, e := range events[1:] {
			_, _ = io.WriteString(w, e)
			w.(http.Flusher).Flush()
		}
	}))

	resp, err := post(h.server.URL+"/llm/anthropic/v1/messages", `{"stream":true}`)
	if err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(resp.Body)
	first, err := r.ReadString('\n')
	if err != nil || first != "event: message_start\n" {
		t.Fatalf("first line = %q, %v", first, err)
	}
	close(firstSeen)
	rest, _ := io.ReadAll(r)
	_ = resp.Body.Close()
	if got := first + string(rest); got != strings.Join(events, "") {
		t.Errorf("agent got %q", got)
	}

	all := h.interactions()
	if len(all) != 1 || !all[0].Streamed || all[0].ResponseBlob != nil || all[0].Error != nil {
		t.Fatalf("interactions = %+v, want one streamed interaction without a response blob", all)
	}
	chunks, err := h.c.Chunks(context.Background(), all[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != len(events) {
		t.Fatalf("got %d chunks, want %d", len(chunks), len(events))
	}
	for i, ch := range chunks {
		if string(ch.Data) != events[i] {
			t.Errorf("chunk %d = %q, want %q", i, ch.Data, events[i])
		}
	}
	if chunks[0].OffsetMillis > chunks[1].OffsetMillis || chunks[1].OffsetMillis < 20 {
		t.Errorf("offsets %d, %d: want them from the response start, the second after the upstream's pause",
			chunks[0].OffsetMillis, chunks[1].OffsetMillis)
	}
}

func TestProxyRecordsAnUnreachableUpstream(t *testing.T) {
	h := newHarness(t, nil)

	resp, err := post(h.server.URL+"/llm/anthropic/v1/messages", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("agent got %d, want 502", resp.StatusCode)
	}
	all := h.interactions()
	if len(all) != 1 || all[0].Error == nil || all[0].StatusCode != nil || all[0].DurationMillis == nil {
		t.Errorf("interactions = %+v, want one completed with an error and no status", all)
	}
}

func TestProxyRecordsAStreamTheAgentAbandons(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: 1\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done() // the proxy cancels when the agent leaves
	}))

	resp, err := post(h.server.URL+"/llm/anthropic/v1/messages", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	// The interaction completes asynchronously after the agent leaves:
	// begin, one chunk and the completion.
	waitFor(t, func() bool { return h.w.Stats().Written >= 3 })
	all := h.interactions()
	if len(all) != 1 {
		t.Fatalf("got %d interactions", len(all))
	}
	in := all[0]
	if in.Error == nil || *in.Error != "the agent closed the connection before the response ended" || in.DurationMillis == nil {
		t.Errorf("interaction error = %v, want it completed saying the agent left", in.Error)
	}
}

// spec §3.3: the agent and the cassette both get the decoded bytes.
func TestProxyDecodesACompressedResponse(t *testing.T) {
	const body = `{"type":"message","content":[{"type":"text","text":"compressed"}]}`
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			http.Error(w, "expected gzip to be accepted", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		_, _ = io.WriteString(gz, body)
		_ = gz.Close()
	}))

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, h.server.URL+"/llm/anthropic/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Accept-Encoding", "gzip") // as an agent might; Go's client then does not decode it
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(got) != body || resp.Header.Get("Content-Encoding") != "" {
		t.Fatalf("agent got %q with Content-Encoding %q, want the plain body", got, resp.Header.Get("Content-Encoding"))
	}

	in := h.interactions()[0]
	if stored := h.blob(in.ResponseBlob); stored != body {
		t.Errorf("stored body = %q, want what the agent got", stored)
	}
	if enc := headerValue(t, in.ResponseHeaders, "Content-Encoding"); enc != "" {
		t.Errorf("stored Content-Encoding = %q, want none", enc)
	}
}

func TestProxyForwardsButDoesNotRecordOversizedBodies(t *testing.T) {
	big := strings.Repeat("x", 4096)
	h := newHarnessWith(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: small\n\n")
			_, _ = io.WriteString(w, "data: "+big+"\n\n") // one event over the limit
			return
		}
		_, _ = io.WriteString(w, big)
	}), 1024, redact.NewRules(nil, nil))

	for _, path := range []string{"/plain", "/stream"} {
		resp, err := post(h.server.URL+"/llm/anthropic"+path, `{}`)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if !strings.Contains(string(got), big) {
			t.Errorf("%s: the agent did not get the whole body", path)
		}
	}

	all := h.interactions()
	if len(all) != 2 {
		t.Fatalf("got %d interactions", len(all))
	}
	plain, stream := all[0], all[1]
	if plain.ResponseBlob != nil || plain.Error == nil || !strings.Contains(*plain.Error, "exceeded 1024 bytes") {
		t.Errorf("plain: blob %v, error %v; want no blob and an error", plain.ResponseBlob, plain.Error)
	}
	if stream.Error == nil || !strings.Contains(*stream.Error, "SSE event exceeded") {
		t.Errorf("stream: error %v, want the oversized event reported", stream.Error)
	}
	if chunks, _ := h.c.Chunks(context.Background(), stream.ID); len(chunks) != 1 {
		t.Errorf("stream: got %d chunks, want the one before the oversized event", len(chunks))
	}
}

// AC-25
func TestProxyRedactsBodiesOnlyInTheCassette(t *testing.T) {
	const (
		reqBody  = `{"messages":[{"role":"user","content":"hi"}],"metadata":{"email":"req@example.com"}}`
		respBody = `{"content":[{"type":"text","text":"hello"}],"email":"resp@example.com"}`
		sseEvent = "event: message_start\ndata: {\"message\":{\"email\":\"sse@example.com\"}}\n\n"
	)
	var gotReq string
	email, _ := redact.ParsePath("$..email")
	h := newHarnessWith(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotReq = string(b)
		if r.URL.Path == "/stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, sseEvent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respBody)
	}), 0, redact.NewRules(nil, []redact.Path{email}))

	// In this order: interaction 1 is plain, 2 is streamed.
	for _, c := range []struct{ path, want string }{{"/plain", respBody}, {"/stream", sseEvent}} {
		path, want := c.path, c.want
		resp, err := post(h.server.URL+"/llm/anthropic"+path, reqBody)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if string(got) != want {
			t.Errorf("%s: agent got %q, want the original", path, got)
		}
		if gotReq != reqBody {
			t.Errorf("%s: upstream got %q, want the original", path, gotReq)
		}
	}

	all := h.interactions()
	if len(all) != 2 {
		t.Fatalf("got %d interactions", len(all))
	}
	stored := h.blob(all[0].RequestBlob) + h.blob(all[0].ResponseBlob) + h.blob(all[1].RequestBlob)
	chunks, _ := h.c.Chunks(context.Background(), all[1].ID)
	for _, ch := range chunks {
		stored += string(ch.Data)
	}
	if strings.Count(stored, "[REDACTED:") != 4 {
		t.Errorf("stored bodies = %s, want the four emails redacted", stored)
	}
	path := h.c.Path()
	if err := h.c.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the cassette this test created
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("@example.com")) {
		t.Error("an email appears in the cassette file")
	}
}

func TestProxyRejectsUnknownRoutes(t *testing.T) {
	h := newHarness(t, http.NotFoundHandler())
	for _, path := range []string{"/", "/llm/anthropicx/v1", "/mcp/orders"} {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, h.server.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, resp.StatusCode)
		}
	}
	if all := h.interactions(); len(all) != 0 {
		t.Errorf("recorded %d interactions for unknown routes", len(all))
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func post(url, body string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(req)
}
