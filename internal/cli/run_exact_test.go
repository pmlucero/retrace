package cli

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
)

// failOutboundDials makes every dialer that retrace could use without being
// given one fail the test if called: those of http.DefaultTransport, which
// http.DefaultClient and a proxy.Proxy without a Transport use, and of
// net.DefaultResolver. The tests of this package do not run in parallel, so
// swapping them is safe.
func failOutboundDials(t *testing.T) {
	t.Helper()
	fail := func(_ context.Context, network, addr string) (net.Conn, error) {
		t.Errorf("outbound dial to %s %s in exact mode", network, addr)
		return nil, errors.New("outbound dials fail this test")
	}
	transport, resolver := http.DefaultTransport, net.DefaultResolver
	http.DefaultTransport = &http.Transport{DialContext: fail, DialTLSContext: fail}
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: fail}
	t.Cleanup(func() { http.DefaultTransport, net.DefaultResolver = transport, resolver })
}

// AC-09: run builds no proxy and no HTTP client, and the cassette does not
// hold the upstream's address. This checks it from outside: the recorded
// upstream stays up and fails the test on any connection, and so do the
// default dialers, while a streamed session is replayed.
func TestRunOpensNoOutboundConnections(t *testing.T) {
	dir := t.TempDir()
	var replaying atomic.Bool
	up := httptest.NewUnstartedServer(streamingAnthropic(0))
	up.Config.ConnState = func(c net.Conn, s http.ConnState) {
		if s == http.StateNew && replaying.Load() {
			t.Errorf("the replay connected to the recorded upstream from %s", c.RemoteAddr())
		}
	}
	up.Start()
	t.Cleanup(up.Close)
	want := recordAgainst(t, dir, "s", 2, up.URL)

	replaying.Store(true)
	failOutboundDials(t)
	r := runRunCmd(t, dir, "s", "--listen", "127.0.0.1:0")
	if r.code != ExitOK {
		t.Fatalf("run: exit %d\n%s", r.code, r.stderr)
	}
	if got := r.stdout.String(); got != want || !strings.Contains(got, "event: message_stop") {
		t.Errorf("replayed agent output:\n%s\nwant the recorded stream:\n%s", got, want)
	}
}

// AC-29: a transport error recorded by the proxy is replayed as the same 502,
// and the replay goes on.
func TestRunReplaysARecordedTransportError(t *testing.T) {
	dir := t.TempDir()
	// The upstream drops the first exchange with no response, which the agent
	// gets as retrace's 502, and answers the agent's second try.
	var calls atomic.Int64
	stream := streamingAnthropic(0)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 {
			stream(w, r)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijacking the first exchange: %v", err)
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(up.Close)
	want := recordAgainst(t, dir, "s", 2, up.URL)
	up.Close()
	if !strings.HasPrefix(want, "502 retrace: upstream request failed: ") || !strings.Contains(want, "\n200 event: message_start") {
		t.Fatalf("recorded agent output %q, want a 502 from retrace, then a 200", want)
	}

	r := runRunCmd(t, dir, "s", "--listen", "127.0.0.1:0")
	if r.code != ExitOK {
		t.Fatalf("run: exit %d, want %d\n%s", r.code, ExitOK, r.stderr)
	}
	if got := r.stdout.String(); got != want {
		t.Errorf("replayed agent output:\n%s\nwant the recorded one:\n%s", got, want)
	}
	if strings.Contains(r.stderr.String(), "never replayed") {
		t.Errorf("unexpected unconsumed warning:\n%s", r.stderr)
	}

	s, ins := sessionOf(t, dir, "s.exact.1")
	_, srcIns := sessionOf(t, dir, "s")
	if s.Status != "completed" {
		t.Errorf("replay status %q, want completed", s.Status)
	}
	if len(ins) != 2 || len(srcIns) != 2 {
		t.Fatalf("replay has %d interactions, source %d; want 2 each", len(ins), len(srcIns))
	}
	in, src := ins[0], srcIns[0]
	if in.Provenance != "replayed" || in.SourceInteractionID == nil || *in.SourceInteractionID != src.ID ||
		in.StatusCode != nil || in.Error == nil || src.Error == nil || *in.Error != *src.Error {
		t.Errorf("replayed interaction %+v, want replayed from %d with no status and its error %v", in, src.ID, src.Error)
	}

	events := eventsOf(t, dir, "s.exact.1")
	srcEvents := eventsOf(t, dir, "s")
	for _, e := range events {
		if e.Kind == "divergence" {
			t.Errorf("divergence event %+v", e)
		}
	}
	got, wantEvent := modelResponseOf(events, in.ID), modelResponseOf(srcEvents, src.ID)
	if got == nil || wantEvent == nil || got.Status != "error" || got.Status != wantEvent.Status ||
		got.Error == nil || wantEvent.Error == nil || *got.Error != *wantEvent.Error {
		t.Errorf("model_response %+v, want the source's %+v", got, wantEvent)
	}
}

// AC-10: run --realtime serves each chunk no earlier than its recorded
// offset, byte for byte. The ±20 ms bound is checked against the injected
// clock in package replay; here a pause recorded upstream must show in the
// replay's offsets, which it would not without --realtime.
func TestRunRealtimeKeepsTheRecordedOffsets(t *testing.T) {
	const pause = 100 * time.Millisecond
	dir := t.TempDir()
	up := httptest.NewServer(streamingAnthropic(pause))
	t.Cleanup(up.Close)
	want := recordAgainst(t, dir, "s", 1, up.URL)
	up.Close()

	r := runRunCmd(t, dir, "s", "--listen", "127.0.0.1:0", "--realtime")
	if r.code != ExitOK {
		t.Fatalf("run: exit %d\n%s", r.code, r.stderr)
	}
	if got := r.stdout.String(); got != want {
		t.Errorf("replayed agent output:\n%s\nwant the recorded one:\n%s", got, want)
	}

	srcChunks := chunksOf(t, dir, "s")
	chunks := chunksOf(t, dir, "s.exact.1")
	if len(chunks) != len(srcChunks) || len(chunks) < 2 {
		t.Fatalf("replayed %d chunks, want the %d recorded ones", len(chunks), len(srcChunks))
	}
	if paused := srcChunks[1].OffsetMillis; paused < pause.Milliseconds() {
		t.Fatalf("second chunk recorded at %d ms, want the upstream's %v pause", paused, pause)
	}
	for i, ch := range chunks {
		src := srcChunks[i]
		if string(ch.Data) != string(src.Data) || ch.OffsetMillis < src.OffsetMillis {
			t.Errorf("chunk %d served at %d ms as %q, want no earlier than %d ms as %q",
				i, ch.OffsetMillis, ch.Data, src.OffsetMillis, src.Data)
		}
	}
}

func eventsOf(t *testing.T, dir, id string) []cassette.Event {
	t.Helper()
	events, err := openCassette(t, cassette.Path(dir, id)).Events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func modelResponseOf(events []cassette.Event, interactionID int64) *cassette.Event {
	for i, e := range events {
		if e.Kind == "model_response" && e.InteractionID != nil && *e.InteractionID == interactionID {
			return &events[i]
		}
	}
	return nil
}

// chunksOf returns the chunks of the only streamed interaction of session id.
func chunksOf(t *testing.T, dir, id string) []cassette.Chunk {
	t.Helper()
	c := openCassette(t, cassette.Path(dir, id))
	ins, err := c.Interactions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ins) != 1 || !ins[0].Streamed {
		t.Fatalf("session %s has interactions %+v, want one streamed", id, ins)
	}
	chunks, err := c.Chunks(context.Background(), ins[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	return chunks
}
