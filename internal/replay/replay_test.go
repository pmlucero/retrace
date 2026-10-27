package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
)

// fakeSource is an in-memory cassette.
type fakeSource struct {
	chunks map[int64][]cassette.Chunk
	blobs  map[string][]byte
	err    error
}

func (s *fakeSource) Chunks(_ context.Context, id int64) ([]cassette.Chunk, error) {
	return s.chunks[id], s.err
}

func (s *fakeSource) Blob(_ context.Context, key string) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	b, ok := s.blobs[key]
	if !ok {
		return nil, cassette.ErrNotFound
	}
	return b, nil
}

func headersJSON(t *testing.T, h http.Header) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// jsonInteraction returns a non-streamed interaction with body, and a source
// holding it.
func jsonInteraction(t *testing.T, status int, h http.Header, body []byte) (cassette.Interaction, *fakeSource) {
	t.Helper()
	key := cassette.BlobKey(body)
	in := cassette.Interaction{
		ID: 1, Channel: "llm", StatusCode: &status,
		ResponseHeaders: headersJSON(t, h), ResponseBlob: &key,
	}
	return in, &fakeSource{blobs: map[string][]byte{key: body}}
}

// sseEvents are recorded chunks with the framings a stream may use,
// including an unknown event type and an unterminated last event.
var sseEvents = []cassette.Chunk{
	{Index: 0, OffsetMillis: 0, Data: []byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n")},
	{Index: 1, OffsetMillis: 7, Data: []byte("event: ping\r\ndata: {\"type\": \"ping\"}\r\n\r\n")},
	{Index: 2, OffsetMillis: 7, Data: []byte("event: future_event\rdata: {\"x\":\"é\"}\r\r")},
	{Index: 3, OffsetMillis: 120, Data: []byte(": comment\ndata: {\"type\":\"content_block_delta\"}\n\n")},
	{Index: 4, OffsetMillis: 1500, Data: []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}")},
}

func sseInteraction(t *testing.T, chunks []cassette.Chunk) (cassette.Interaction, *fakeSource) {
	t.Helper()
	status := http.StatusOK
	in := cassette.Interaction{
		ID: 7, Channel: "llm", StatusCode: &status, Streamed: true,
		ResponseHeaders: headersJSON(t, http.Header{
			"Content-Type": {"text/event-stream; charset=utf-8"},
			"Request-Id":   {"req_1"},
		}),
	}
	return in, &fakeSource{chunks: map[int64][]cassette.Chunk{7: chunks}}
}

func concat(chunks []cassette.Chunk) []byte {
	var b []byte
	for _, ch := range chunks {
		b = append(b, ch.Data...)
	}
	return b
}

// fakeClock advances only when slept on, and records each sleep.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	return ctx.Err()
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// stampWriter records the clock time of each write and flush.
type stampWriter struct {
	*httptest.ResponseRecorder
	clock  Clock
	writes []time.Time
	// onWrite, if set, runs after each write.
	onWrite func()
}

func (w *stampWriter) Write(b []byte) (int, error) {
	w.writes = append(w.writes, w.clock.Now())
	n, err := w.ResponseRecorder.Write(b)
	if w.onWrite != nil {
		w.onWrite()
	}
	return n, err
}

func TestServeJSON(t *testing.T) {
	body := []byte(`{"id":"msg_1","type":"message","content":[{"type":"text","text":"hi"}]}`)
	tests := []struct {
		name   string
		status int
		body   []byte
	}{
		{"ok", http.StatusOK, body},
		{"overloaded", 529, []byte(`{"type":"error","error":{"type":"overloaded_error"}}`)},
		{"empty", http.StatusOK, []byte{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorded := http.Header{
				"Content-Type":      {"application/json"},
				"Request-Id":        {"req_1"},
				"Set-Cookie":        {"a=1", "b=2"},
				"X-Api-Key":         {"[REDACTED]"},
				"Content-Length":    {"9999"}, // the unredacted body's length
				"Connection":        {"keep-alive, X-Hop"},
				"X-Hop":             {"1"},
				"Keep-Alive":        {"timeout=5"},
				"Transfer-Encoding": {"chunked"},
			}
			in, src := jsonInteraction(t, tt.status, recorded, tt.body)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := Serve(r.Context(), w, src, in, Options{}); err != nil {
					t.Errorf("Serve: %v", err)
				}
			}))
			defer srv.Close()

			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tt.status {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.status)
			}
			if !bytes.Equal(got, tt.body) {
				t.Errorf("body = %q, want %q", got, tt.body)
			}
			if resp.ContentLength != int64(len(tt.body)) {
				t.Errorf("Content-Length = %d, want %d", resp.ContentLength, len(tt.body))
			}
			for _, name := range []string{"Content-Type", "Request-Id", "Set-Cookie", "X-Api-Key"} {
				if g, w := resp.Header.Values(name), recorded.Values(name); !equal(g, w) {
					t.Errorf("%s = %q, want %q", name, g, w)
				}
			}
			for _, name := range []string{"X-Hop", "Keep-Alive"} {
				if v := resp.Header.Values(name); len(v) > 0 {
					t.Errorf("hop-by-hop header %s = %q was replayed", name, v)
				}
			}
			if len(resp.TransferEncoding) > 0 {
				t.Errorf("Transfer-Encoding = %q, want none", resp.TransferEncoding)
			}
		})
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// AC-10: the agent receives exactly the recorded chunks, each one as soon as
// it is written: replay does not buffer the stream.
func TestServeStreamIncremental(t *testing.T) {
	in, src := sseInteraction(t, sseEvents)
	clock := newGatedClock()
	served := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served <- Serve(r.Context(), w, src, in, Options{Realtime: true, Clock: clock})
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream; charset=utf-8" ||
		resp.Header.Get("Request-Id") != "req_1" {
		t.Fatalf("status %d, headers %v", resp.StatusCode, resp.Header)
	}

	var got []byte
	for i, ch := range sseEvents {
		// The next chunk is held back until this one has been read, so a
		// buffered stream would stall here until the timeout.
		buf := make([]byte, len(ch.Data))
		if _, err := io.ReadFull(resp.Body, buf); err != nil {
			t.Fatalf("reading chunk %d: %v", i, err)
		}
		got = append(got, buf...)
		clock.release()
	}
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, rest...)
	if want := concat(sseEvents); !bytes.Equal(got, want) {
		t.Errorf("stream = %q, want %q", got, want)
	}
	if err := <-served; err != nil {
		t.Errorf("Serve: %v", err)
	}
}

// gatedClock lets each sleep end only when the test releases it.
type gatedClock struct {
	mu    sync.Mutex
	now   time.Time
	gates chan struct{}
}

func newGatedClock() *gatedClock {
	return &gatedClock{now: time.Unix(0, 0), gates: make(chan struct{}, len(sseEvents))}
}

func (c *gatedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *gatedClock) Sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-c.gates:
	case <-ctx.Done():
		return ctx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return nil
}

func (c *gatedClock) release() { c.gates <- struct{}{} }

// AC-10: without --realtime the stream is byte-identical and nothing waits.
func TestServeStreamImmediate(t *testing.T) {
	in, src := sseInteraction(t, sseEvents)
	clock := newFakeClock()
	rec := httptest.NewRecorder()
	if err := Serve(context.Background(), rec, src, in, Options{Clock: clock}); err != nil {
		t.Fatal(err)
	}
	if got, want := rec.Body.Bytes(), concat(sseEvents); !bytes.Equal(got, want) {
		t.Errorf("stream = %q, want %q", got, want)
	}
	if len(clock.sleeps) > 0 {
		t.Errorf("slept %v without --realtime", clock.sleeps)
	}
	if rec.Header().Get("Content-Length") != "" {
		t.Errorf("a stream got Content-Length %q", rec.Header().Get("Content-Length"))
	}
}

// AC-10: with --realtime each chunk is written exactly at its recorded
// offset from the headers.
func TestServeRealtimeSchedule(t *testing.T) {
	tests := []struct {
		name string
		// lag is how long each write takes, on the fake clock.
		lag time.Duration
	}{
		{"instant writes", 0},
		{"slow writes", 3 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in, src := sseInteraction(t, sseEvents)
			clock := newFakeClock()
			start := clock.Now()
			w := &stampWriter{ResponseRecorder: httptest.NewRecorder(), clock: clock,
				onWrite: func() { clock.advance(tt.lag) }}
			if err := Serve(context.Background(), w, src, in, Options{Realtime: true, Clock: clock}); err != nil {
				t.Fatal(err)
			}
			if len(w.writes) != len(sseEvents) {
				t.Fatalf("%d writes, want %d", len(w.writes), len(sseEvents))
			}
			for i, ch := range sseEvents {
				want := time.Duration(ch.OffsetMillis) * time.Millisecond
				if i > 0 && sseEvents[i-1].OffsetMillis == ch.OffsetMillis {
					// A chunk recorded at the same offset follows at once.
					want = w.writes[i-1].Add(tt.lag).Sub(start)
				}
				if got := w.writes[i].Sub(start); got != want {
					t.Errorf("chunk %d written at %v, want %v", i, got, want)
				}
			}
			if got, want := w.Body.Bytes(), concat(sseEvents); !bytes.Equal(got, want) {
				t.Errorf("stream = %q, want %q", got, want)
			}
		})
	}
}

// AC-10: the one wall-clock check, loose on purpose. A chunk is never written
// before its recorded offset; being late is the scheduler's doing, so a late
// attempt is retried a few times before the ±20 ms bound fails the test.
func TestServeRealtimeWallClock(t *testing.T) {
	chunks := []cassette.Chunk{
		{Index: 0, OffsetMillis: 0, Data: []byte("data: 0\n\n")},
		{Index: 1, OffsetMillis: 60, Data: []byte("data: 1\n\n")},
		{Index: 2, OffsetMillis: 120, Data: []byte("data: 2\n\n")},
	}
	in, src := sseInteraction(t, chunks)
	var late string
	for range 3 {
		w := &stampWriter{ResponseRecorder: httptest.NewRecorder(), clock: systemClock{}}
		start := time.Now()
		if err := Serve(context.Background(), w, src, in, Options{Realtime: true}); err != nil {
			t.Fatal(err)
		}
		late = ""
		for i, ch := range chunks {
			want := time.Duration(ch.OffsetMillis) * time.Millisecond
			got := w.writes[i].Sub(start)
			if got < want {
				t.Fatalf("chunk %d written at %v, before its offset %v", i, got, want)
			}
			if got > want+20*time.Millisecond {
				late = fmt.Sprintf("chunk %d written at %v, want %v (±20ms)", i, got, want)
			}
		}
		if late == "" {
			return
		}
	}
	t.Error(late)
}

// An agent that disconnects mid-stream ends the replay at once, even while
// it waits for the next chunk.
func TestServeCancelMidStream(t *testing.T) {
	chunks := []cassette.Chunk{
		{Index: 0, OffsetMillis: 0, Data: []byte("data: first\n\n")},
		{Index: 1, OffsetMillis: time.Hour.Milliseconds(), Data: []byte("data: never\n\n")},
	}
	in, src := sseInteraction(t, chunks)
	served := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served <- Serve(r.Context(), w, src, in, Options{Realtime: true})
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, len(chunks[0].Data))
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatal(err)
	}
	cancel()

	select {
	case err := <-served:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Serve = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the agent disconnected")
	}
}

// A context already done writes the headers but no chunk.
func TestServeCanceledBeforeChunks(t *testing.T) {
	in, src := sseInteraction(t, sseEvents)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	if err := Serve(ctx, rec, src, in, Options{}); !errors.Is(err, context.Canceled) {
		t.Errorf("Serve = %v, want context.Canceled", err)
	}
	if rec.Body.Len() > 0 {
		t.Errorf("wrote %q after cancellation", rec.Body.Bytes())
	}
}

// Serve reports what it cannot replay before writing anything, so the
// caller can still answer the request.
func TestServeErrorsWriteNothing(t *testing.T) {
	status := http.StatusOK
	hdr := json.RawMessage(`{"Content-Type":["application/json"]}`)
	key := cassette.BlobKey([]byte("x"))
	boom := errors.New("disk on fire")
	tests := []struct {
		name string
		in   cassette.Interaction
		src  *fakeSource
		want error
	}{
		{"no status", cassette.Interaction{ResponseHeaders: hdr}, &fakeSource{}, ErrUnservable},
		{"no headers", cassette.Interaction{StatusCode: &status}, &fakeSource{}, ErrUnservable},
		{"body not recorded", cassette.Interaction{StatusCode: &status, ResponseHeaders: hdr}, &fakeSource{}, ErrUnservable},
		{"blob missing", cassette.Interaction{StatusCode: &status, ResponseHeaders: hdr, ResponseBlob: &key},
			&fakeSource{}, cassette.ErrNotFound},
		{"chunks unreadable", cassette.Interaction{StatusCode: &status, ResponseHeaders: hdr, Streamed: true},
			&fakeSource{err: boom}, boom},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			err := Serve(context.Background(), rec, tt.src, tt.in, Options{})
			if !errors.Is(err, tt.want) {
				t.Errorf("Serve = %v, want %v", err, tt.want)
			}
			if len(rec.Header()) > 0 || rec.Body.Len() > 0 || rec.Flushed {
				t.Errorf("wrote headers %v, body %q", rec.Header(), rec.Body.Bytes())
			}
		})
	}
}

// Serve replays from a real cassette too.
func TestServeFromCassette(t *testing.T) {
	ctx := context.Background()
	c, err := cassette.Create(ctx, t.TempDir()+"/s.cassette")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	status := http.StatusOK
	err = c.Write(ctx, func(tx *cassette.Tx) error {
		if err := tx.InsertSession(ctx, cassette.Session{ID: "s", Mode: "record", Status: "recording",
			CreatedAt: time.Now(), Command: []string{"agent"}, Cwd: "/", OnMiss: "strict", RetraceVersion: "dev"}); err != nil {
			return err
		}
		const id = 1
		if err := tx.BeginInteraction(ctx, cassette.Interaction{ID: id, Channel: "llm", Upstream: "anthropic",
			HTTPMethod: "POST", Path: "/v1/messages", RequestHeaders: json.RawMessage(`{}`),
			StartedAt: time.Now(), Provenance: "recorded"}); err != nil {
			return err
		}
		for _, ch := range sseEvents {
			ch.InteractionID = id
			if err := tx.AppendChunk(ctx, ch); err != nil {
				return err
			}
		}
		return tx.CompleteInteraction(ctx, id, cassette.Completion{StatusCode: &status, Streamed: true,
			ResponseHeaders: json.RawMessage(`{"Content-Type":["text/event-stream"]}`), DurationMillis: 1})
	})
	if err != nil {
		t.Fatal(err)
	}
	ins, err := c.Interactions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if err := Serve(ctx, rec, c, ins[0], Options{}); err != nil {
		t.Fatal(err)
	}
	if got, want := rec.Body.Bytes(), concat(sseEvents); !bytes.Equal(got, want) {
		t.Errorf("stream = %q, want %q", got, want)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q", got)
	}
}
