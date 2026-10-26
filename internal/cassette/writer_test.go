package cassette

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// newWriter creates a cassette with a session row and starts a Writer on it.
func newWriter(t *testing.T, opts WriterOptions) (*Writer, *Cassette, string) {
	t.Helper()
	c, path := newCassette(t)
	write(t, c, func(tx *Tx) error { return tx.InsertSession(context.Background(), testSession()) })
	w, err := NewWriter(context.Background(), c, opts)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	return w, c, path
}

// stall returns a beforeBatch hook that blocks the writer until release is
// called, and a channel that receives once each time the writer stalls.
func stall() (hook func(context.Context), stalled <-chan struct{}, release func()) {
	ch := make(chan struct{}, 1)
	gate := make(chan struct{})
	var once sync.Once
	hook = func(ctx context.Context) {
		select {
		case ch <- struct{}{}:
		default:
		}
		select {
		case <-gate:
		case <-ctx.Done():
		}
	}
	return hook, ch, func() { once.Do(func() { close(gate) }) }
}

// closeWriter ends the session as completed.
func closeWriter(t *testing.T, w *Writer) {
	t.Helper()
	if err := w.Close(context.Background(), SessionEnd{Status: "completed", EndedAt: at(30)}); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func llmRequest(sec int, body []byte) Interaction {
	return Interaction{
		Channel: "llm", Upstream: "anthropic", HTTPMethod: "POST", Path: "/v1/messages",
		RequestHeaders: json.RawMessage(`{}`), RequestBlob: ptr(BlobKey(body)),
		StartedAt: at(sec), Provenance: "recorded",
	}
}

func interactionByID(t *testing.T, c *Cassette) map[int64]Interaction {
	t.Helper()
	all, err := c.Interactions(context.Background())
	if err != nil {
		t.Fatalf("Interactions: %v", err)
	}
	out := map[int64]Interaction{}
	for _, in := range all {
		out[in.ID] = in
	}
	return out
}

// noBlock fails the test if fn does not return promptly: recording must never
// stall the proxy (AC-03).
func noBlock(t *testing.T, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { fn(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a Writer call blocked while the writer was stalled")
	}
}

func TestWriterRecordsASession(t *testing.T) {
	ctx := context.Background()
	w, c, _ := newWriter(t, WriterOptions{})

	body := []byte(`{"model":"m","messages":[]}`)
	id := w.BeginInteraction(llmRequest(1, body), body)
	for i, data := range []string{"event: message_start\n\n", "event: message_stop\n\n"} {
		w.AppendChunk(Chunk{InteractionID: id, Index: i, OffsetMillis: int64(10 * i), Data: []byte(data)})
	}
	w.CompleteInteraction(id, Completion{StatusCode: ptr(200), Streamed: true, DurationMillis: 20})
	parent := w.AppendEvent(Event{InteractionID: &id, Kind: "model_response", Source: "model", Target: "agent",
		StartedAt: at(1), Status: "ok", Provenance: "recorded", DeriverVersion: 1})
	child := w.AppendEvent(Event{ParentSeq: &parent, InteractionID: &id, Kind: "tool_use", Source: "model",
		Target: "agent", StartedAt: at(1), Tool: ptr("search"), Status: "ok", Provenance: "recorded", DeriverVersion: 1})
	servers := json.RawMessage(`[{"name":"orders","protocol_version":"2025-06-18"}]`)
	w.SetMCPServers(servers)
	closeWriter(t, w)

	if id != 1 || parent != 1 || child != 2 {
		t.Errorf("assigned id %d and seqs %d, %d; want 1 and 1, 2", id, parent, child)
	}
	in := interactionByID(t, c)[id]
	if in.DurationMillis == nil || *in.DurationMillis != 20 || in.Error != nil {
		t.Errorf("interaction = %+v, want completed in 20 ms without error", in)
	}
	if got, err := c.Blob(ctx, *in.RequestBlob); err != nil || string(got) != string(body) {
		t.Errorf("request blob = %q, %v; want the request body", got, err)
	}
	if chunks, _ := c.Chunks(ctx, id); len(chunks) != 2 {
		t.Errorf("got %d chunks, want 2", len(chunks))
	}
	events, _ := c.Events(ctx)
	if len(events) != 2 || events[1].ParentSeq == nil || *events[1].ParentSeq != parent {
		t.Errorf("events = %+v, want a model_response and its tool_use", events)
	}
	s, _ := c.Session(ctx)
	if s.Status != "completed" || string(s.MCPServers) != string(servers) {
		t.Errorf("session status %q, MCP servers %s; want completed and the new servers", s.Status, s.MCPServers)
	}
	if st := w.Stats(); st.Written != 7 || st.Dropped != 0 {
		t.Errorf("stats = %+v, want 7 written and none dropped", st)
	}
}

func TestWriterAssignsUniqueIDsUnderConcurrency(t *testing.T) {
	const workers, perWorker = 16, 50
	w, c, _ := newWriter(t, WriterOptions{})

	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for range perWorker {
				body := []byte("request")
				id := w.BeginInteraction(llmRequest(1, body), body)
				w.AppendChunk(Chunk{InteractionID: id, Data: []byte("data: {}\n\n")})
				w.CompleteInteraction(id, Completion{StatusCode: ptr(200), DurationMillis: 1})
				w.AppendEvent(Event{InteractionID: &id, Kind: "model_response", Source: "model", Target: "agent",
					StartedAt: at(1), Status: "ok", Provenance: "recorded", DeriverVersion: 1})
			}
		})
	}
	wg.Wait()
	closeWriter(t, w)

	all := interactionByID(t, c)
	if len(all) != workers*perWorker {
		t.Fatalf("got %d interactions, want %d", len(all), workers*perWorker)
	}
	for id := int64(1); id <= workers*perWorker; id++ {
		if in, ok := all[id]; !ok || in.DurationMillis == nil {
			t.Fatalf("interaction %d missing or not completed", id)
		}
	}
	if events, _ := c.Events(context.Background()); len(events) != workers*perWorker {
		t.Errorf("got %d events, want %d", len(events), workers*perWorker)
	}
	if st := w.Stats(); st.Dropped != 0 {
		t.Errorf("stats = %+v, want nothing dropped", st)
	}
}

// When the queue is full, calls return at once and the interactions that lost
// records are completed with an error (docs/plan/mvp.md, Design points).
func TestWriterDropsInsteadOfBlockingWhenTheQueueIsFull(t *testing.T) {
	ctx := context.Background()
	hook, stalled, release := stall()
	w, c, _ := newWriter(t, WriterOptions{QueueLen: 2, beforeBatch: hook})
	defer release()

	first := w.BeginInteraction(llmRequest(1, []byte("a")), []byte("a"))
	<-stalled // the writer holds the first record; the queue is empty

	var partial, missing int64
	noBlock(t, func() {
		partial = w.BeginInteraction(llmRequest(2, []byte("b")), []byte("b"))     // queued
		w.AppendChunk(Chunk{InteractionID: partial, Index: 0, Data: []byte("x")}) // queued, queue full
		w.AppendChunk(Chunk{InteractionID: partial, Index: 1, Data: []byte("y")}) // dropped
		w.CompleteInteraction(partial, Completion{StatusCode: ptr(200), DurationMillis: 5})
		w.AppendEvent(Event{InteractionID: &partial, Kind: "model_response", Source: "model", Target: "agent",
			StartedAt: at(2), Status: "ok", Provenance: "recorded", DeriverVersion: 1})
		missing = w.BeginInteraction(llmRequest(3, []byte("c")), []byte("c")) // dropped
		w.CompleteInteraction(missing, Completion{StatusCode: ptr(502), DurationMillis: 7})
	})
	release()
	waitFor(t, func() bool { return w.Stats().Written == 2 }) // both begin records; the queue has room again
	w.CompleteInteraction(first, Completion{StatusCode: ptr(200), DurationMillis: 3})
	event := func(id int64) {
		w.AppendEvent(Event{InteractionID: &id, Kind: "model_response", Source: "model", Target: "agent",
			StartedAt: at(2), Status: "ok", Provenance: "recorded", DeriverVersion: 1})
	}
	event(partial) // its row exists, so the event is kept
	event(missing) // its row does not exist yet, so the event is dropped
	closeWriter(t, w)

	all := interactionByID(t, c)
	if in := all[first]; in.Error != nil || in.DurationMillis == nil {
		t.Errorf("interaction %d = %+v, want complete without error", first, in)
	}

	in := all[partial]
	if in.Error == nil || !strings.Contains(*in.Error, RecordingIncomplete) || in.StatusCode == nil || *in.StatusCode != 200 {
		t.Errorf("interaction %d = %+v, want its completion with a 'recording incomplete' error", partial, in)
	}
	if in.RequestBlob == nil {
		t.Errorf("interaction %d lost its request blob, but its begin record was written", partial)
	}
	if chunks, _ := c.Chunks(ctx, partial); len(chunks) != 0 {
		t.Errorf("interaction %d has %d chunks, want none after a dropped chunk", partial, len(chunks))
	}

	in = all[missing]
	if in.Error == nil || in.StatusCode == nil || *in.StatusCode != 502 || in.RequestBlob != nil {
		t.Errorf("interaction %d = %+v, want it inserted on close, with its completion, an error and no blob", missing, in)
	}

	if events, _ := c.Events(ctx); len(events) != 1 || *events[0].InteractionID != partial {
		t.Errorf("events = %+v, want only the one of interaction %d", events, partial)
	}
	if st := w.Stats(); st.LostInteractions != 2 || st.DroppedEvents != 2 {
		t.Errorf("stats = %+v, want 2 lost interactions and 2 dropped events", st)
	}
}

func TestWriterBoundsQueuedBytes(t *testing.T) {
	hook, stalled, release := stall()
	w, c, _ := newWriter(t, WriterOptions{QueueBytes: 4 << 10, beforeBatch: hook})
	defer release()

	w.SetMCPServers(json.RawMessage(`[]`))
	<-stalled
	big := make([]byte, 8<<10)
	id := w.BeginInteraction(llmRequest(1, big), big)
	release()
	closeWriter(t, w)

	if in := interactionByID(t, c)[id]; in.Error == nil || in.RequestBlob != nil {
		t.Errorf("interaction = %+v, want it dropped by the byte limit and marked incomplete", in)
	}
	if st := w.Stats(); st.MaxQueueBytes > 4<<10 {
		t.Errorf("queued up to %d bytes, limit is %d", st.MaxQueueBytes, 4<<10)
	}
}

func TestWriterAcceptsALargeRecordIntoAnEmptyQueue(t *testing.T) {
	w, c, _ := newWriter(t, WriterOptions{QueueBytes: 4 << 10})
	big := make([]byte, 8<<10)
	id := w.BeginInteraction(llmRequest(1, big), big)
	closeWriter(t, w)

	if in := interactionByID(t, c)[id]; in.Error != nil || in.RequestBlob == nil {
		t.Errorf("interaction = %+v, want a record larger than the byte limit written when nothing else is queued", in)
	}
}

// Callers may reuse their buffers as soon as a call returns, such as the read
// buffer of an SSE stream; the cassette keeps what was passed (spec §7.3).
func TestWriterCopiesTheBuffersItIsGiven(t *testing.T) {
	ctx := context.Background()
	hook, stalled, release := stall()
	w, c, _ := newWriter(t, WriterOptions{beforeBatch: hook})

	w.SetMCPServers(json.RawMessage(`[]`))
	<-stalled
	buf := []byte("request")
	id := w.BeginInteraction(llmRequest(1, buf), buf)
	copy(buf, "XXXXXXX")
	buf = []byte("data: 1\n\n")
	w.AppendChunk(Chunk{InteractionID: id, Data: buf})
	copy(buf, "data: 2\n\n")
	release()
	closeWriter(t, w)

	in := interactionByID(t, c)[id]
	if got, err := c.Blob(ctx, *in.RequestBlob); err != nil || string(got) != "request" {
		t.Errorf("request blob = %q, %v; want the bytes passed to BeginInteraction", got, err)
	}
	if chunks, _ := c.Chunks(ctx, id); len(chunks) != 1 || string(chunks[0].Data) != "data: 1\n\n" {
		t.Errorf("chunks = %+v, want the bytes passed to AppendChunk", chunks)
	}
}

func TestWriterDropsEventsWhoseParentWasDropped(t *testing.T) {
	hook, stalled, release := stall()
	w, c, _ := newWriter(t, WriterOptions{QueueLen: 1, beforeBatch: hook})
	defer release()

	event := func(parent *int64) int64 {
		return w.AppendEvent(Event{ParentSeq: parent, Kind: "mcp_message", Source: "agent", Target: "mcp:orders",
			StartedAt: at(1), Status: "ok", Provenance: "recorded", DeriverVersion: 1})
	}
	kept := event(nil)
	<-stalled
	queued := event(nil)  // fills the queue
	dropped := event(nil) // queue full
	release()
	waitFor(t, func() bool { return w.Stats().Written == 2 })
	orphan := event(&dropped) // the queue has room, but its parent is missing
	child := event(&kept)
	closeWriter(t, w)

	events, err := c.Events(context.Background())
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var got []int64
	for _, e := range events {
		got = append(got, e.Seq)
	}
	if want := []int64{kept, queued, child}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("stored seqs %v, want %v (orphan %d dropped)", got, want, orphan)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(time.Millisecond)
	}
}

// Ending a session with writes still queued keeps every one of them: Close
// drains the queue before the session ends as interrupted. Foundation for AC-07.
func TestWriterCloseDrainsQueuedRecords(t *testing.T) {
	const n = 100
	hook, stalled, release := stall()
	w, c, path := newWriter(t, WriterOptions{beforeBatch: hook})

	w.SetMCPServers(json.RawMessage(`[]`))
	<-stalled
	for i := range n {
		body := []byte(fmt.Sprint("request ", i))
		id := w.BeginInteraction(llmRequest(1, body), body)
		w.CompleteInteraction(id, Completion{StatusCode: ptr(200), DurationMillis: 1})
	}
	closed := make(chan error)
	go func() { closed <- w.Close(context.Background(), SessionEnd{Status: "interrupted", EndedAt: at(30)}) }()
	release()
	if err := <-closed; err != nil {
		t.Fatalf("Close: %v", err)
	}

	if all := interactionByID(t, c); len(all) != n {
		t.Errorf("got %d interactions, want all %d queued ones", len(all), n)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("closing cassette: %v", err)
	}
	assertValidCassette(t, path, "interrupted")
}

func TestWriterCloseTimeoutStillEndsTheSession(t *testing.T) {
	hook, stalled, release := stall() // never released: the disk "hangs"
	defer release()
	w, c, path := newWriter(t, WriterOptions{beforeBatch: hook})

	w.SetMCPServers(json.RawMessage(`[]`))
	<-stalled
	id := w.BeginInteraction(llmRequest(1, []byte("a")), []byte("a"))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := w.Close(ctx, SessionEnd{Status: "interrupted", EndedAt: at(30)})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v, want a deadline exceeded", err)
	}

	in := interactionByID(t, c)[id]
	if in.Error == nil || !strings.Contains(*in.Error, "recording ended before it was written") || in.DurationMillis == nil {
		t.Errorf("interaction = %+v, want it inserted and completed with an error", in)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("closing cassette: %v", err)
	}
	assertValidCassette(t, path, "interrupted")
}

// A write stuck in the kernel ignores cancellation. Close still returns, and
// the cassette is left as it is: valid, and still recording.
func TestWriterCloseGivesUpOnAStuckWriter(t *testing.T) {
	gate := make(chan struct{})
	stalled := make(chan struct{}, 1)
	hook := func(context.Context) {
		select {
		case stalled <- struct{}{}:
		default:
		}
		<-gate // ignores ctx, like a hung disk
	}
	w, c, path := newWriter(t, WriterOptions{beforeBatch: hook, closeGrace: 50 * time.Millisecond})

	w.SetMCPServers(json.RawMessage(`[]`))
	<-stalled
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := w.Close(ctx, SessionEnd{Status: "interrupted", EndedAt: at(30)})
	if !errors.Is(err, ErrWriterStuck) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v, want ErrWriterStuck and a deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Close took %v with a stuck writer", elapsed)
	}
	close(gate)
	<-w.done
	if err := c.Close(); err != nil {
		t.Fatalf("closing cassette: %v", err)
	}
	assertValidCassette(t, path, "recording")
}

// A record that fails to write loses only its own interaction, not the rest
// of its batch.
func TestWriterIsolatesARecordThatFailsToWrite(t *testing.T) {
	ctx := context.Background()
	hook, stalled, release := stall()
	w, c, _ := newWriter(t, WriterOptions{beforeBatch: hook})

	w.SetMCPServers(json.RawMessage(`[]`))
	<-stalled
	bad := w.BeginInteraction(llmRequest(1, []byte("a")), []byte("a"))
	w.AppendChunk(Chunk{InteractionID: bad, Index: 0, Data: []byte("x")})
	w.AppendChunk(Chunk{InteractionID: bad, Index: 0, Data: []byte("x")}) // duplicate key
	w.CompleteInteraction(bad, Completion{StatusCode: ptr(200), DurationMillis: 1})
	good := w.BeginInteraction(llmRequest(2, []byte("b")), []byte("b"))
	w.CompleteInteraction(good, Completion{StatusCode: ptr(200), DurationMillis: 1})
	release()
	closeWriter(t, w)

	all := interactionByID(t, c)
	if in := all[bad]; in.Error == nil || !strings.Contains(*in.Error, "writing to the cassette failed") {
		t.Errorf("interaction %d = %+v, want it marked with the write failure", bad, in)
	}
	if chunks, _ := c.Chunks(ctx, bad); len(chunks) != 1 {
		t.Errorf("interaction %d has %d chunks, want the first one", bad, len(chunks))
	}
	if in := all[good]; in.Error != nil || in.DurationMillis == nil {
		t.Errorf("interaction %d = %+v, want it written normally", good, in)
	}
}

// Completing twice keeps the first completion (ADR 0009), and is not a loss.
func TestWriterKeepsTheFirstCompletion(t *testing.T) {
	w, c, _ := newWriter(t, WriterOptions{})
	id := w.BeginInteraction(llmRequest(1, []byte("a")), []byte("a"))
	w.CompleteInteraction(id, Completion{StatusCode: ptr(200), DurationMillis: 1})
	w.CompleteInteraction(id, Completion{StatusCode: ptr(500), DurationMillis: 9})
	closeWriter(t, w)

	if in := interactionByID(t, c)[id]; *in.StatusCode != 200 || in.Error != nil {
		t.Errorf("interaction = %+v, want the first completion", in)
	}
	if st := w.Stats(); st.Dropped != 0 {
		t.Errorf("stats = %+v, want nothing dropped", st)
	}
}

func TestWriterIgnoresRecordsAfterClose(t *testing.T) {
	w, _, _ := newWriter(t, WriterOptions{})
	closeWriter(t, w)

	w.BeginInteraction(llmRequest(1, []byte("a")), []byte("a"))
	if st := w.Stats(); st.Dropped != 1 {
		t.Errorf("stats = %+v, want the late record dropped", st)
	}
	if err := w.Close(context.Background(), SessionEnd{Status: "completed", EndedAt: at(30)}); err == nil {
		t.Error("second Close succeeded, want an error")
	}
}

func TestNewWriterContinuesExistingIDs(t *testing.T) {
	ctx := context.Background()
	c, _ := newCassette(t)
	write(t, c, func(tx *Tx) error {
		if err := tx.InsertSession(ctx, testSession()); err != nil {
			return err
		}
		if err := tx.BeginInteraction(ctx, Interaction{ID: 5, Channel: "llm", Upstream: "anthropic",
			HTTPMethod: "POST", Path: "/v1/messages", StartedAt: at(1), Provenance: "recorded"}); err != nil {
			return err
		}
		return tx.AppendEvent(ctx, Event{Seq: 7, Kind: "user_message", Source: "user", Target: "model",
			StartedAt: at(1), Status: "ok", Provenance: "recorded", DeriverVersion: 1})
	})
	w, err := NewWriter(ctx, c, WriterOptions{})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	id := w.BeginInteraction(llmRequest(2, []byte("a")), []byte("a"))
	seq := w.AppendEvent(Event{Kind: "user_message", Source: "user", Target: "model", StartedAt: at(2),
		Status: "ok", Provenance: "recorded", DeriverVersion: 1})
	closeWriter(t, w)
	if id != 6 || seq != 8 {
		t.Errorf("assigned id %d and seq %d, want 6 and 8", id, seq)
	}
}

func TestNewWriterRefusesNewerSchema(t *testing.T) {
	ctx := context.Background()
	c, path := newCassette(t)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	setUserVersion(t, path, len(migrations)+1)
	c, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer c.Close()
	if _, err := NewWriter(ctx, c, WriterOptions{}); !errors.Is(err, ErrNewerSchema) {
		t.Errorf("NewWriter error = %v, want ErrNewerSchema", err)
	}
}

// crashHelperEnv names the cassette that TestWriterCrashHelper records into.
const crashHelperEnv = "RETRACE_TEST_CRASH_CASSETTE"

// A recording killed without warning (SIGKILL, power loss) leaves a valid
// cassette: a prefix of the log, still marked as recording (ADR 0009).
func TestWriterSurvivesAKilledProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a subprocess")
	}
	path := t.TempDir() + "/killed.cassette"
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWriterCrashHelper$") //nolint:gosec // G204: re-runs this test binary
	cmd.Env = append(os.Environ(), crashHelperEnv+"="+path)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if sc.Text() == "ready" {
				ready <- true
				return
			}
		}
		ready <- false
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("helper exited before recording")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("helper did not start recording")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	c, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open after kill: %v", err)
	}
	if all := interactionByID(t, c); len(all) == 0 {
		t.Error("no interactions survived the kill")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("closing cassette: %v", err)
	}
	assertValidCassette(t, path, "recording")
}

// TestWriterCrashHelper records until it is killed. It only runs as the
// subprocess of TestWriterSurvivesAKilledProcess.
func TestWriterCrashHelper(t *testing.T) {
	path := os.Getenv(crashHelperEnv)
	if path == "" {
		t.Skip("subprocess of TestWriterSurvivesAKilledProcess")
	}
	ctx := context.Background()
	c, err := Create(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Write(ctx, func(tx *Tx) error { return tx.InsertSession(ctx, testSession()) }); err != nil {
		t.Fatal(err)
	}
	w, err := NewWriter(ctx, c, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; ; i++ {
		body := []byte(fmt.Sprint("request ", i))
		id := w.BeginInteraction(llmRequest(1, body), body)
		for j := range 5 {
			w.AppendChunk(Chunk{InteractionID: id, Index: j, Data: []byte("data: {}\n\n")})
		}
		w.CompleteInteraction(id, Completion{StatusCode: ptr(200), Streamed: true, DurationMillis: 1})
		if i == 50 {
			waitFor(t, func() bool { return w.Stats().Written > 0 })
			fmt.Println("ready")
		}
		time.Sleep(100 * time.Microsecond)
	}
}

// assertValidCassette checks a closed cassette file: SQLite integrity, no
// dangling references, a single file, and the session status.
func assertValidCassette(t *testing.T, path, status string) {
	t.Helper()
	ctx := context.Background()
	if _, err := os.Stat(path + "-wal"); !os.IsNotExist(err) {
		t.Errorf("%s-wal exists after close", path)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var integrity string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Errorf("integrity_check = %q, %v; want ok", integrity, err)
	}
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	if rows.Next() {
		t.Error("foreign_key_check reports dangling references")
	}
	_ = rows.Close()
	var got string
	if err := db.QueryRowContext(ctx, `SELECT status FROM session`).Scan(&got); err != nil || got != status {
		t.Errorf("session status = %q, %v; want %q", got, err, status)
	}
}

// BenchmarkWriterLongSSEStream measures the writer under one long streamed
// response per iteration (docs/plan/mvp.md, Design points):
//
//   - drain: the producer goes as fast as it can, with a queue large enough
//     to hold the whole stream. It measures how many chunks per second the
//     writer commits.
//   - paced: chunks arrive at a steady 5000 per second, far above a model's
//     token rate, with the default queue. It reports how deep the queue gets
//     and whether anything is dropped.
func BenchmarkWriterLongSSEStream(b *testing.B) {
	data := []byte(`event: content_block_delta` + "\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"a few tokens of text"}}` + "\n\n")

	run := func(b *testing.B, chunks int, interval time.Duration, opts WriterOptions) {
		ctx := context.Background()
		c, err := Create(ctx, b.TempDir()+"/bench.cassette")
		if err != nil {
			b.Fatal(err)
		}
		defer c.Close()
		if err := c.Write(ctx, func(tx *Tx) error { return tx.InsertSession(ctx, testSession()) }); err != nil {
			b.Fatal(err)
		}
		w, err := NewWriter(ctx, c, opts)
		if err != nil {
			b.Fatal(err)
		}

		b.SetBytes(int64(chunks * len(data)))
		b.ResetTimer()
		for i := range b.N {
			body := []byte(fmt.Sprint("request ", i))
			id := w.BeginInteraction(llmRequest(1, body), body)
			start := time.Now()
			for j := range chunks {
				if interval > 0 {
					time.Sleep(time.Until(start.Add(time.Duration(j) * interval)))
				}
				w.AppendChunk(Chunk{InteractionID: id, Index: j, OffsetMillis: int64(j), Data: data})
			}
			w.CompleteInteraction(id, Completion{StatusCode: ptr(200), Streamed: true, DurationMillis: int64(chunks)})
			want := (i + 1) * (chunks + 2)
			for st := w.Stats(); st.Written+st.Dropped < want; st = w.Stats() {
				time.Sleep(100 * time.Microsecond)
			}
		}
		b.StopTimer()
		if err := w.Close(ctx, SessionEnd{Status: "completed", EndedAt: at(30)}); err != nil {
			b.Fatal(err)
		}
		st := w.Stats()
		b.ReportMetric(float64(chunks*b.N)/b.Elapsed().Seconds(), "chunks/s")
		b.ReportMetric(float64(st.MaxQueueLen), "max-queue-len")
		b.ReportMetric(float64(st.Dropped)/float64(b.N), "dropped/op")
	}

	b.Run("drain", func(b *testing.B) {
		run(b, 10_000, 0, WriterOptions{QueueLen: 10_100})
	})
	b.Run("paced", func(b *testing.B) {
		run(b, 2_000, 200*time.Microsecond, WriterOptions{})
	})
}
