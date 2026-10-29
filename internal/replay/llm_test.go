package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/derive"
	"github.com/pmlucero/retrace/internal/match"
	"github.com/pmlucero/retrace/internal/redact"
)

// recorded is one LLM interaction of a source cassette.
type recorded struct {
	request    string
	body       string           // non-streamed response body
	chunks     []cassette.Chunk // streamed response, if set
	noResponse bool             // a transport error: no status, no headers
	noBody     bool             // a status and headers, but no body recorded
	incomplete bool             // the recording is marked incomplete
}

// testFingerprint stands in for canonicalization: the body is its own
// fingerprint.
func testFingerprint(body []byte) string { return "fp:" + string(body) }

func newCassette(t *testing.T, mode string) *cassette.Cassette {
	t.Helper()
	ctx := context.Background()
	c, err := cassette.Create(ctx, filepath.Join(t.TempDir(), mode+".cassette"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	err = c.Write(ctx, func(tx *cassette.Tx) error {
		return tx.InsertSession(ctx, cassette.Session{ID: mode, Mode: mode, Status: "recording",
			CreatedAt: time.Now(), Command: []string{"agent"}, Cwd: "/", OnMiss: "strict", RetraceVersion: "dev"})
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// sourceCassette writes recs as the recorded LLM interactions 1..n of a source
// cassette.
func sourceCassette(t *testing.T, recs ...recorded) *cassette.Cassette {
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
			if err := tx.BeginInteraction(ctx, cassette.Interaction{ID: id, Channel: "llm", Upstream: "anthropic",
				HTTPMethod: http.MethodPost, Path: "/v1/messages", RequestBlob: &req,
				StartedAt: time.Now(), Provenance: "recorded"}); err != nil {
				return err
			}
			status := http.StatusOK
			done := cassette.Completion{StatusCode: &status, DurationMillis: 10,
				ResponseHeaders: json.RawMessage(`{"Content-Type":["application/json"],"Request-Id":["req_` + fmt.Sprint(id) + `"]}`)}
			switch {
			case r.noResponse:
				msg := "dial tcp: connection refused"
				done = cassette.Completion{DurationMillis: 10, Error: &msg}
			case r.noBody: // the status and headers above, with no response blob
			case r.chunks != nil:
				done.Streamed = true
				done.ResponseHeaders = json.RawMessage(`{"Content-Type":["text/event-stream"]}`)
				for _, ch := range r.chunks {
					ch.InteractionID = id
					if err := tx.AppendChunk(ctx, ch); err != nil {
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

// replayHarness replays a source cassette through an LLMHandler into a new
// session.
type replayHarness struct {
	t       *testing.T
	h       *LLMHandler
	queues  *match.Queues
	out     *cassette.Cassette
	w       *cassette.Writer
	server  *httptest.Server
	stopped bool
}

func newReplayHarness(t *testing.T, src *cassette.Cassette, rules *redact.Rules, opts Options) *replayHarness {
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
		return testFingerprint(b), true
	})
	out := newCassette(t, "exact")
	w, err := cassette.NewWriter(ctx, out, cassette.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	h := NewLLMHandler(LLMConfig{Source: src, Queues: queues, Redact: rules,
		Fingerprint: testFingerprint, Writer: w, Options: opts})
	server := httptest.NewServer(http.StripPrefix("/llm/anthropic", h))
	t.Cleanup(server.Close)
	return &replayHarness{t: t, h: h, queues: queues, out: out, w: w, server: server}
}

// post sends body to the replayed Messages API and returns the status and
// the response body.
func (r *replayHarness) post(body string) (int, string) {
	r.t.Helper()
	return r.postTo("/v1/messages", body)
}

// postTo sends body to path under the LLM route.
func (r *replayHarness) postTo(path, body string) (int, string) {
	r.t.Helper()
	status, _, b := r.do(path, body)
	return status, b
}

// do sends body to path under the LLM route and returns the status, the
// headers and the body of the response.
func (r *replayHarness) do(path, body string) (int, http.Header, string) {
	r.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		r.server.URL+"/llm/anthropic"+path, strings.NewReader(body))
	if err != nil {
		r.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		r.t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, string(b)
}

// stop ends the new session and returns what it recorded.
func (r *replayHarness) stop() ([]cassette.Interaction, []cassette.Event) {
	r.t.Helper()
	ctx := context.Background()
	if !r.stopped {
		r.server.Close()
		if err := r.w.Close(ctx, cassette.SessionEnd{Status: "completed", EndedAt: time.Now()}); err != nil {
			r.t.Fatalf("closing writer: %v", err)
		}
		r.stopped = true
	}
	ins, err := r.out.Interactions(ctx)
	if err != nil {
		r.t.Fatal(err)
	}
	events, err := r.out.Events(ctx)
	if err != nil {
		r.t.Fatal(err)
	}
	return ins, events
}

func (r *replayHarness) blob(key *string) string {
	r.t.Helper()
	if key == nil {
		r.t.Fatal("blob reference is nil")
	}
	b, err := r.out.Blob(context.Background(), *key)
	if err != nil {
		r.t.Fatal(err)
	}
	return string(b)
}

func (r *replayHarness) diverged() bool {
	select {
	case <-r.h.Diverged():
		return true
	default:
		return false
	}
}

// AC-12
func TestLLMHandlerServesRepeatedRequestsInOrder(t *testing.T) {
	src := sourceCassette(t,
		recorded{request: `{"q":"same"}`, body: `{"n":1}`},
		recorded{request: `{"q":"other"}`, body: `{"n":"other"}`},
		recorded{request: `{"q":"same"}`, body: `{"n":2}`},
		recorded{request: `{"q":"same"}`, body: `{"n":3}`},
	)
	r := newReplayHarness(t, src, nil, Options{})
	for _, want := range []string{`{"n":1}`, `{"n":2}`, `{"n":3}`} {
		if status, body := r.post(`{"q":"same"}`); status != http.StatusOK || body != want {
			t.Errorf("got %d %s, want 200 %s", status, body, want)
		}
	}
	if status, body := r.post(`{"q":"other"}`); status != http.StatusOK || body != `{"n":"other"}` {
		t.Errorf("got %d %s, want the other response", status, body)
	}
	if r.diverged() {
		t.Error("diverged without a miss")
	}
	if n := len(r.queues.Unconsumed()); n != 0 {
		t.Errorf("%d interactions unconsumed, want 0", n)
	}

	ins, events := r.stop()
	if len(events) != 0 {
		t.Errorf("got %d events, want none", len(events))
	}
	wantSources := []int64{1, 3, 4, 2}
	if len(ins) != len(wantSources) {
		t.Fatalf("recorded %d interactions, want %d", len(ins), len(wantSources))
	}
	for i, in := range ins {
		if in.Provenance != "replayed" || in.SourceInteractionID == nil || *in.SourceInteractionID != wantSources[i] {
			t.Errorf("interaction %d: provenance %q, source %v; want replayed from %d",
				in.ID, in.Provenance, in.SourceInteractionID, wantSources[i])
		}
		if in.Channel != "llm" || in.Upstream != "anthropic" || in.HTTPMethod != http.MethodPost || in.Path != "/v1/messages" {
			t.Errorf("interaction %d: %s %s %s %s", in.ID, in.Channel, in.Upstream, in.HTTPMethod, in.Path)
		}
		if in.Incomplete() || in.Error != nil || in.StatusCode == nil || *in.StatusCode != http.StatusOK {
			t.Errorf("interaction %d: status %v, error %v", in.ID, in.StatusCode, in.Error)
		}
		if in.Fingerprint == nil || *in.Fingerprint != testFingerprint([]byte(r.blob(in.RequestBlob))) {
			t.Errorf("interaction %d: fingerprint %v", in.ID, in.Fingerprint)
		}
	}
	if got := r.blob(ins[1].ResponseBlob); got != `{"n":2}` {
		t.Errorf("second response stored as %s", got)
	}
	if got := string(ins[0].ResponseHeaders); !strings.Contains(got, "req_1") {
		t.Errorf("response headers %s, want the recorded ones", got)
	}
}

func TestLLMHandlerRecordsServedChunks(t *testing.T) {
	src := sourceCassette(t, recorded{request: `{"stream":true}`, chunks: sseEvents})
	clock := newFakeClock()
	r := newReplayHarness(t, src, nil, Options{Realtime: true, Clock: clock})
	status, body := r.post(`{"stream":true}`)
	if status != http.StatusOK || body != string(concat(sseEvents)) {
		t.Fatalf("got %d %q, want the recorded stream", status, body)
	}

	ins, _ := r.stop()
	if len(ins) != 1 {
		t.Fatalf("recorded %d interactions, want 1", len(ins))
	}
	in := ins[0]
	if !in.Streamed || in.ResponseBlob != nil || in.DurationMillis == nil || *in.DurationMillis != 1500 {
		t.Errorf("streamed %v, blob %v, duration %v; want a 1500 ms stream", in.Streamed, in.ResponseBlob, in.DurationMillis)
	}
	chunks, err := r.out.Chunks(context.Background(), in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != len(sseEvents) {
		t.Fatalf("recorded %d chunks, want %d", len(chunks), len(sseEvents))
	}
	for i, ch := range chunks {
		// The fake clock advances only by the realtime sleeps, so the served
		// offsets are the recorded ones.
		want := sseEvents[i]
		if ch.Index != i || ch.OffsetMillis != want.OffsetMillis || string(ch.Data) != string(want.Data) {
			t.Errorf("chunk %d: index %d, offset %d, %q; want offset %d, %q",
				i, ch.Index, ch.OffsetMillis, ch.Data, want.OffsetMillis, want.Data)
		}
	}
}

func TestLLMHandlerRedactsBeforeFingerprinting(t *testing.T) {
	path, err := redact.ParsePath("$.metadata.user_id")
	if err != nil {
		t.Fatal(err)
	}
	rules := redact.NewRules(nil, []redact.Path{path})
	raw := `{"metadata":{"user_id":"alice"},"q":"hi"}`
	stored, err := rules.Body([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), "alice") {
		t.Fatalf("rules did not redact: %s", stored)
	}
	src := sourceCassette(t, recorded{request: string(stored), body: `{"ok":true}`})
	r := newReplayHarness(t, src, rules, Options{})

	if status, body := r.post(raw); status != http.StatusOK || body != `{"ok":true}` {
		t.Fatalf("got %d %s, want the recorded response", status, body)
	}
	ins, _ := r.stop()
	if len(ins) != 1 {
		t.Fatalf("recorded %d interactions, want 1", len(ins))
	}
	if got := r.blob(ins[0].RequestBlob); got != string(stored) {
		t.Errorf("request stored as %s, want the redacted %s", got, stored)
	}
}

// divergence returns the result of the only event in events, checking that
// it is a divergence.
func divergence(t *testing.T, events []cassette.Event) map[string]any {
	t.Helper()
	if len(events) != 1 {
		t.Fatalf("got %d events, want one divergence", len(events))
	}
	e := events[0]
	if e.Kind != "divergence" || e.Status != "divergence" || e.Source != "retrace" || e.Target != "agent" ||
		e.Error == nil || !strings.Contains(*e.Error, "cassette miss") || e.InteractionID != nil {
		t.Errorf("event %+v is not a divergence", e)
	}
	var res map[string]any
	if err := json.Unmarshal(e.Result, &res); err != nil {
		t.Fatalf("divergence result %s: %v", e.Result, err)
	}
	if res["channel"] != "llm" || res["upstream"] != "anthropic" {
		t.Errorf("divergence result %s: want channel llm, upstream anthropic", e.Result)
	}
	if diff, ok := res["diff"].([]any); !ok || len(diff) != 0 {
		t.Errorf("divergence diff %v, want an empty list", res["diff"])
	}
	return res
}

// checkMissResponse checks the 422 body of a cassette miss (spec §6.2).
func checkMissResponse(t *testing.T, status int, body, wantMessage string) {
	t.Helper()
	if status != http.StatusUnprocessableEntity {
		t.Errorf("status %d, want 422", status)
	}
	var resp struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("miss body %s: %v", body, err)
	}
	if resp.Type != "error" || resp.Error.Type != "retrace_cassette_miss" || !strings.Contains(resp.Error.Message, wantMessage) {
		t.Errorf("miss body %s, want a retrace_cassette_miss error saying %q", body, wantMessage)
	}
}

// AC-28
func TestLLMHandlerMissOnEmptyQueue(t *testing.T) {
	for _, tc := range []struct {
		name    string
		before  int // requests served before the miss
		request string
	}{
		{"never recorded", 0, `{"q":"b"}`},
		{"queue exhausted", 1, `{"q":"a"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReplayHarness(t, sourceCassette(t, recorded{request: `{"q":"a"}`, body: `{}`}), nil, Options{})
			for range tc.before {
				if status, _ := r.post(`{"q":"a"}`); status != http.StatusOK {
					t.Fatalf("status %d before the miss", status)
				}
			}
			status, body := r.post(tc.request)
			checkMissResponse(t, status, body, "no recorded request matches")
			if !r.diverged() {
				t.Error("Diverged not closed after a miss")
			}

			ins, events := r.stop()
			res := divergence(t, events)
			if want := testFingerprint([]byte(tc.request)); res["received_fingerprint"] != want {
				t.Errorf("received_fingerprint %v, want %s", res["received_fingerprint"], want)
			}
			if _, ok := res["incomplete_interaction"]; ok {
				t.Errorf("incomplete_interaction set on an empty queue: %v", res)
			}
			if len(ins) != tc.before {
				t.Errorf("recorded %d interactions, want only the %d served", len(ins), tc.before)
			}
		})
	}
}

// AC-28
func TestLLMHandlerMissOnIncompleteRecording(t *testing.T) {
	src := sourceCassette(t,
		recorded{request: `{"q":"a"}`, body: `{"n":1}`},
		recorded{request: `{"q":"a"}`, body: `{"n":2}`, incomplete: true},
		recorded{request: `{"q":"a"}`, body: `{"n":3}`},
	)
	r := newReplayHarness(t, src, nil, Options{})
	if status, body := r.post(`{"q":"a"}`); status != http.StatusOK || body != `{"n":1}` {
		t.Fatalf("got %d %s, want the first response", status, body)
	}
	// The incomplete head blocks its queue: every later request misses.
	for range 2 {
		status, body := r.post(`{"q":"a"}`)
		checkMissResponse(t, status, body, "recording of the matching interaction 2 is incomplete")
	}
	if !r.diverged() {
		t.Error("Diverged not closed after a miss")
	}
	var unconsumed []int64
	for _, in := range r.queues.Unconsumed() {
		unconsumed = append(unconsumed, in.ID)
	}
	if !slices.Equal(unconsumed, []int64{3}) {
		t.Errorf("unconsumed %v, want [3]: the incomplete interaction is not reported", unconsumed)
	}

	ins, events := r.stop()
	if len(ins) != 1 {
		t.Errorf("recorded %d interactions, want only the served one", len(ins))
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want a divergence per miss", len(events))
	}
	res := divergence(t, events[:1])
	if res["incomplete_interaction"] != float64(2) {
		t.Errorf("incomplete_interaction %v, want 2", res["incomplete_interaction"])
	}
	if res["received_fingerprint"] != testFingerprint([]byte(`{"q":"a"}`)) {
		t.Errorf("received_fingerprint %v", res["received_fingerprint"])
	}
}

// AC-29
func TestLLMHandlerServesATransportError(t *testing.T) {
	const req = `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`
	src := sourceCassette(t,
		recorded{request: req, noResponse: true},
		recorded{request: req, body: `{"n":2}`},
	)
	r := newReplayHarness(t, src, nil, Options{})

	// The agent gets the answer the proxy gave it while recording.
	status, header, body := r.do("/v1/messages", req)
	if status != http.StatusBadGateway {
		t.Errorf("status %d, want 502", status)
	}
	if got := header.Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type %q, want the proxy's text/plain", got)
	}
	if got := header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options %q, want nosniff", got)
	}
	if !strings.HasPrefix(body, "retrace: upstream request failed: ") || !strings.Contains(body, "dial tcp: connection refused") {
		t.Errorf("body %q, want the upstream failure with the recorded error", body)
	}
	if r.diverged() {
		t.Error("Diverged closed by a recorded transport error")
	}
	// The transport error was consumed: the same request gets the next one.
	if status, body := r.post(req); status != http.StatusOK || body != `{"n":2}` {
		t.Errorf("second request got %d %s, want the second recorded response", status, body)
	}
	if un := r.queues.Unconsumed(); len(un) != 0 {
		t.Errorf("unconsumed %v, want none", un)
	}

	ins, events := r.stop()
	if len(events) != 0 {
		t.Errorf("got events %+v, want no divergence", events)
	}
	if len(ins) != 2 {
		t.Fatalf("recorded %d interactions, want 2", len(ins))
	}
	srcIns, err := src.Interactions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	in, want := ins[0], srcIns[0]
	if in.Provenance != "replayed" || in.SourceInteractionID == nil || *in.SourceInteractionID != want.ID {
		t.Errorf("provenance %s from %v, want replayed from %d", in.Provenance, in.SourceInteractionID, want.ID)
	}
	if in.StatusCode != nil || string(in.ResponseHeaders) != string(want.ResponseHeaders) || in.ResponseBlob != nil || in.Streamed {
		t.Errorf("recorded a response %+v, want none", in)
	}
	if in.Error == nil || *in.Error != *want.Error || in.Incomplete() {
		t.Errorf("error %v, want the complete recording of %q", in.Error, *want.Error)
	}
	got, wantEvent := modelResponse(t, r.out, in), modelResponse(t, src, want)
	if got.Status != wantEvent.Status || got.Error == nil || wantEvent.Error == nil || *got.Error != *wantEvent.Error ||
		string(got.Result) != string(wantEvent.Result) || !equalPtr(got.Model, wantEvent.Model) ||
		!equalPtr(got.RequestedModel, wantEvent.RequestedModel) || !equalPtr(got.TokensOut, wantEvent.TokensOut) {
		t.Errorf("model_response %+v, want the source's %+v", got, wantEvent)
	}
}

func equalPtr[T comparable](a, b *T) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

// modelResponse derives the model_response event of in, an interaction of c.
func modelResponse(t *testing.T, c *cassette.Cassette, in cassette.Interaction) cassette.Event {
	t.Helper()
	req, err := c.Blob(context.Background(), *in.RequestBlob)
	if err != nil {
		t.Fatal(err)
	}
	var events []cassette.Event
	emit := func(e cassette.Event) int64 {
		events = append(events, e)
		return int64(len(events))
	}
	d := derive.New()
	d.Begin(in, req, emit)
	d.Complete(in.ID, cassette.Completion{StatusCode: in.StatusCode, ResponseHeaders: in.ResponseHeaders,
		ResponseBlob: in.ResponseBlob, Streamed: in.Streamed, DurationMillis: *in.DurationMillis, Error: in.Error}, nil, emit)
	for _, e := range events {
		if e.Kind == "model_response" {
			return e
		}
	}
	t.Fatalf("interaction %d derived no model_response: %+v", in.ID, events)
	return cassette.Event{}
}

// A head with a status but no recorded body has no response to replay: it is
// a miss, so nothing is fabricated (ADR 0006).
func TestLLMHandlerMissOnUnservableHead(t *testing.T) {
	src := sourceCassette(t, recorded{request: `{"q":"a"}`, noBody: true})
	r := newReplayHarness(t, src, nil, Options{})
	status, body := r.post(`{"q":"a"}`)
	checkMissResponse(t, status, body, "interaction 1 has no recorded response")
	ins, events := r.stop()
	divergence(t, events)
	if len(ins) != 0 {
		t.Errorf("recorded %d interactions for a miss, want none", len(ins))
	}
}

func TestLLMHandlerMissOnUnredactableBody(t *testing.T) {
	path, err := redact.ParsePath("$.secret")
	if err != nil {
		t.Fatal(err)
	}
	r := newReplayHarness(t, sourceCassette(t), redact.NewRules(nil, []redact.Path{path}), Options{})
	deep := strings.Repeat("[", 10000) + strings.Repeat("]", 10000)
	status, body := r.post(deep)
	checkMissResponse(t, status, body, "cannot be redacted")
	_, events := r.stop()
	if res := divergence(t, events); res["received_fingerprint"] != nil {
		t.Errorf("received_fingerprint %v, want null", res["received_fingerprint"])
	}
}

func TestLLMHandlerConcurrentRequests(t *testing.T) {
	const n = 16
	var recs []recorded
	for i := range n {
		recs = append(recs, recorded{request: `{"q":"same"}`, body: fmt.Sprintf(`{"n":%d}`, i)})
	}
	r := newReplayHarness(t, sourceCassette(t, recs...), nil, Options{})
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		seen []string
	)
	for range n {
		wg.Go(func() {
			status, body := r.post(`{"q":"same"}`)
			if status != http.StatusOK {
				t.Errorf("status %d", status)
			}
			mu.Lock()
			seen = append(seen, body)
			mu.Unlock()
		})
	}
	wg.Wait()
	slices.Sort(seen)
	if seen = slices.Compact(seen); len(seen) != n {
		t.Errorf("got %d distinct responses, want each of the %d served once", len(seen), n)
	}
	ins, _ := r.stop()
	sources := map[int64]bool{}
	for _, in := range ins {
		sources[*in.SourceInteractionID] = true
	}
	if len(ins) != n || len(sources) != n {
		t.Errorf("recorded %d interactions from %d sources, want %d", len(ins), len(sources), n)
	}
}

// failingSource fails every read of a response body.
type failingSource struct{ Source }

func (failingSource) Blob(context.Context, string) ([]byte, error) {
	return nil, errors.New("disk I/O error")
}

func TestLLMHandlerCassetteReadError(t *testing.T) {
	r := newReplayHarness(t, sourceCassette(t, recorded{request: `{"q":"a"}`, body: `{}`}), nil, Options{})
	r.h.rc.source = failingSource{r.h.rc.source}
	status, body := r.post(`{"q":"a"}`)
	if status != http.StatusInternalServerError || !strings.Contains(body, "disk I/O error") {
		t.Errorf("got %d %s, want 500 with the cassette error", status, body)
	}
	if r.diverged() {
		t.Error("a cassette read error is not a divergence")
	}
	ins, events := r.stop()
	if len(events) != 0 {
		t.Errorf("got %d events, want none", len(events))
	}
	if len(ins) != 1 || ins[0].Error == nil || !strings.Contains(*ins[0].Error, "disk I/O error") || ins[0].StatusCode != nil {
		t.Errorf("recorded %+v, want one interaction with the read error and no response", ins)
	}
}

// blockingClock blocks every realtime sleep until the request is canceled.
type blockingClock struct{ fakeClock }

func (c *blockingClock) Sleep(ctx context.Context, _ time.Duration) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestLLMHandlerRecordsAnAbandonedStream(t *testing.T) {
	chunks := []cassette.Chunk{
		{Index: 0, OffsetMillis: 0, Data: []byte("data: {\"type\":\"message_start\"}\n\n")},
		{Index: 1, OffsetMillis: 100, Data: []byte("data: {\"type\":\"message_stop\"}\n\n")},
	}
	src := sourceCassette(t, recorded{request: `{"q":"a"}`, chunks: chunks})
	clock := &blockingClock{fakeClock: *newFakeClock()}
	r := newReplayHarness(t, src, nil, Options{Realtime: true, Clock: clock})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.server.URL+"/llm/anthropic/v1/messages",
		strings.NewReader(`{"q":"a"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	first := make([]byte, len(chunks[0].Data))
	if _, err := io.ReadFull(resp.Body, first); err != nil || string(first) != string(chunks[0].Data) {
		t.Fatalf("first chunk %q, %v", first, err)
	}
	cancel() // the agent leaves while the second chunk waits for its offset
	_ = resp.Body.Close()

	ins, _ := r.stop()
	if len(ins) != 1 {
		t.Fatalf("recorded %d interactions, want 1", len(ins))
	}
	in := ins[0]
	if in.Error == nil || !strings.Contains(*in.Error, "agent closed the connection") || in.Incomplete() {
		t.Errorf("error %v, want the agent leaving, not an incomplete recording", in.Error)
	}
	got, err := r.out.Chunks(context.Background(), in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || string(got[0].Data) != string(chunks[0].Data) {
		t.Errorf("recorded %d chunks, want only the one served", len(got))
	}
}

// blockingSource blocks every read of the cassette until the request is
// canceled.
type blockingSource struct {
	Source
	reading chan struct{}
}

func (s blockingSource) Blob(ctx context.Context, _ string) ([]byte, error) {
	close(s.reading)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestLLMHandlerRecordsAnAgentThatLeavesBeforeTheResponse(t *testing.T) {
	r := newReplayHarness(t, sourceCassette(t, recorded{request: `{"q":"a"}`, body: `{}`}), nil, Options{})
	reading := make(chan struct{})
	r.h.rc.source = blockingSource{Source: r.h.rc.source, reading: reading}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-reading
		cancel()
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.server.URL+"/llm/anthropic/v1/messages",
		strings.NewReader(`{"q":"a"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
		t.Fatal("request succeeded, want it canceled")
	}

	ins, _ := r.stop()
	if len(ins) != 1 || ins[0].Error == nil || !strings.Contains(*ins[0].Error, "before the response arrived") {
		t.Errorf("recorded %+v, want the agent leaving before the response", ins)
	}
}

func TestLLMHandlerRecordsEmptyResponses(t *testing.T) {
	src := sourceCassette(t,
		recorded{request: `{"q":"json"}`, body: ""},
		recorded{request: `{"q":"sse"}`, chunks: []cassette.Chunk{}},
	)
	r := newReplayHarness(t, src, nil, Options{})
	for _, req := range []string{`{"q":"json"}`, `{"q":"sse"}`} {
		if status, body := r.post(req); status != http.StatusOK || body != "" {
			t.Errorf("%s: got %d %q, want an empty 200", req, status, body)
		}
	}
	ins, _ := r.stop()
	if len(ins) != 2 {
		t.Fatalf("recorded %d interactions, want 2", len(ins))
	}
	if in := ins[0]; in.Streamed || in.ResponseBlob == nil || r.blob(in.ResponseBlob) != "" || in.Incomplete() {
		t.Errorf("empty body recorded as %+v", in)
	}
	if in := ins[1]; !in.Streamed || in.ResponseBlob != nil || in.Incomplete() {
		t.Errorf("empty stream recorded as %+v", in)
	}
}

func TestLLMHandlerRedactsTheQuery(t *testing.T) {
	r := newReplayHarness(t, sourceCassette(t, recorded{request: `{"q":"a"}`, body: `{}`}), nil, Options{})
	if status, _ := r.postTo("/v1/messages?beta=true&key=sk-secret", `{"q":"a"}`); status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	ins, _ := r.stop()
	if len(ins) != 1 {
		t.Fatalf("recorded %d interactions, want 1", len(ins))
	}
	if p := ins[0].Path; strings.Contains(p, "sk-secret") || !strings.HasPrefix(p, "/v1/messages?beta=true&key=") {
		t.Errorf("path stored as %q, want the key masked", p)
	}
}

// Spec §6.2: a miss on any channel ends the replay, so the handlers of one
// replay share its Divergence.
func TestLLMHandlerSharedDivergence(t *testing.T) {
	d := NewDivergence()
	r := newReplayHarness(t, sourceCassette(t, recorded{request: `{"q":"a"}`, body: `{}`}), nil, Options{})
	r.h.rc.divergence = d
	other := NewLLMHandler(LLMConfig{Divergence: d})
	select {
	case <-other.Diverged():
		t.Fatal("diverged before any miss")
	default:
	}
	if status, _ := r.post(`{"q":"b"}`); status != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want a miss", status)
	}
	select {
	case <-other.Diverged():
	default:
		t.Error("a miss on one handler did not close the shared Diverged")
	}
}
