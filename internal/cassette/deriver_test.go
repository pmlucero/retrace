package cassette

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
)

// fakeDeriver emits one event per request and per completion, the second a
// child of the first, and logs what it saw.
type fakeDeriver struct {
	mu       sync.Mutex
	seen     []string
	requests map[int64]int64 // interaction id → seq of its request event
}

func (d *fakeDeriver) Begin(in Interaction, request []byte, emit EmitFunc) {
	d.log("begin %d %s", in.ID, request)
	id := in.ID
	kind := "user_message"
	if string(request) == "bad" {
		kind = "invalid" // fails the CHECK constraint
	}
	seq := emit(Event{InteractionID: &id, Kind: kind, Source: "user", Target: "model",
		StartedAt: in.StartedAt, Status: "ok", Provenance: "recorded", DeriverVersion: 1})
	d.mu.Lock()
	d.requests[in.ID] = seq
	d.mu.Unlock()
}

func (d *fakeDeriver) Chunk(ch Chunk) { d.log("chunk %d %s", ch.InteractionID, ch.Data) }

func (d *fakeDeriver) Complete(id int64, _ Completion, response []byte, emit EmitFunc) {
	d.log("complete %d %s", id, response)
	d.mu.Lock()
	parent := d.requests[id]
	d.mu.Unlock()
	emit(Event{ParentSeq: &parent, InteractionID: &id, Kind: "model_response", Source: "model", Target: "agent",
		StartedAt: at(1), Status: "ok", Provenance: "recorded", DeriverVersion: 1})
}

func (d *fakeDeriver) log(format string, args ...any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seen = append(d.seen, fmt.Sprintf(format, args...))
}

func newFakeDeriver() *fakeDeriver { return &fakeDeriver{requests: map[int64]int64{}} }

func TestWriterRunsTheDeriverOnStoredRecords(t *testing.T) {
	ctx := context.Background()
	d := newFakeDeriver()
	w, c, _ := newWriter(t, WriterOptions{Deriver: d})

	req := []byte(`{"q":1}`)
	canonical := []byte(`canonical`)
	in := llmRequest(1, req)
	in.CanonicalBlob = ptr(BlobKey(canonical))
	id := w.BeginInteraction(in, canonical, req)
	w.AppendChunk(Chunk{InteractionID: id, Index: 0, Data: []byte("data: x\n\n")})
	resp := []byte(`{"a":2}`)
	w.CompleteInteraction(id, Completion{StatusCode: ptr(200), ResponseBlob: ptr(BlobKey(resp)), DurationMillis: 5}, resp)
	// An event appended by the caller gets the next seq, after the derived ones
	// that were already assigned or before them: seqs stay unique either way.
	queued := w.AppendEvent(Event{Kind: "divergence", Source: "retrace", Target: "agent", StartedAt: at(2),
		Status: "divergence", Provenance: "recorded", DeriverVersion: 1})
	closeWriter(t, w)

	want := []string{`begin 1 {"q":1}`, "chunk 1 data: x\n\n", `complete 1 {"a":2}`}
	if !slices.Equal(d.seen, want) {
		t.Errorf("deriver saw %q, want %q", d.seen, want)
	}
	events, err := c.Events(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	seqs := map[int64]bool{}
	for _, e := range events {
		kinds = append(kinds, e.Kind)
		seqs[e.Seq] = true
	}
	slices.Sort(kinds)
	if !slices.Equal(kinds, []string{"divergence", "model_response", "user_message"}) || !seqs[queued] {
		t.Fatalf("events = %+v, want the two derived ones and the queued one", events)
	}
	for _, e := range events {
		if e.Kind == "model_response" && (e.ParentSeq == nil || !seqs[*e.ParentSeq]) {
			t.Errorf("model_response parent = %v, want the user_message", e.ParentSeq)
		}
	}
	if st := w.Stats(); st.Written != 6 || st.Dropped != 0 {
		t.Errorf("stats = %+v, want 6 records written (4 queued, 2 derived)", st)
	}
}

// A dropped record is not shown to the deriver, and neither is the rest of
// its interaction, until Close completes it as incomplete (see Deriver).
func TestWriterDerivesLostInteractionsOnClose(t *testing.T) {
	ctx := context.Background()
	d := newFakeDeriver()
	hook, stalled, release := stall()
	w, c, _ := newWriter(t, WriterOptions{QueueLen: 2, beforeBatch: hook, Deriver: d})
	defer release()

	first := w.BeginInteraction(llmRequest(1, []byte("a")), []byte("a"))
	<-stalled
	lost := w.BeginInteraction(llmRequest(2, []byte("b")), []byte("b"))
	w.AppendChunk(Chunk{InteractionID: lost, Index: 0, Data: []byte("x")}) // queue full
	w.AppendChunk(Chunk{InteractionID: lost, Index: 1, Data: []byte("y")}) // dropped
	missing := w.BeginInteraction(llmRequest(3, []byte("c")), []byte("c")) // dropped
	release()
	waitFor(t, func() bool { return w.Stats().Written >= 4 })
	w.CompleteInteraction(lost, Completion{StatusCode: ptr(200), DurationMillis: 1}) // blocked by the drop
	w.CompleteInteraction(first, Completion{StatusCode: ptr(200), DurationMillis: 1})
	closeWriter(t, w)

	// The queued chunk x is dropped with the rest of interaction 2. On close,
	// interactions 2 and 3 are completed as incomplete, and 3's request,
	// whose body was not stored, is shown first.
	want := []string{"begin 1 a", "begin 2 b", "complete 1 ", "complete 2 ", "begin 3 ", "complete 3 "}
	if !slices.Equal(d.seen, want) {
		t.Errorf("deriver saw %q, want %q", d.seen, want)
	}
	events, _ := c.Events(ctx)
	perInteraction := map[int64]int{}
	for _, e := range events {
		perInteraction[*e.InteractionID]++
	}
	if len(events) != 6 || perInteraction[first] != 2 || perInteraction[lost] != 2 || perInteraction[missing] != 2 {
		t.Errorf("events per interaction = %v, want 2 each", perInteraction)
	}
}

// A derived event that fails to write is dropped alone, and so are the
// events that reference it. The batch is retried record by record.
func TestWriterDropsDerivedEventsThatFailToWrite(t *testing.T) {
	ctx := context.Background()
	d := newFakeDeriver()
	hook, stalled, release := stall()
	w, c, _ := newWriter(t, WriterOptions{beforeBatch: hook, Deriver: d})

	w.SetMCPServers(nil)
	<-stalled
	bad := w.BeginInteraction(llmRequest(1, []byte("bad")), []byte("bad"))
	w.CompleteInteraction(bad, Completion{StatusCode: ptr(200), DurationMillis: 1})
	good := w.BeginInteraction(llmRequest(2, []byte("good")), []byte("good"))
	w.CompleteInteraction(good, Completion{StatusCode: ptr(200), DurationMillis: 1})
	release()
	closeWriter(t, w)

	events, _ := c.Events(ctx)
	for _, e := range events {
		if *e.InteractionID == bad {
			t.Errorf("event %+v of interaction %d was kept, but its parent failed to write", e, bad)
		}
	}
	if len(events) != 2 {
		t.Errorf("got %d events, want the 2 of interaction %d", len(events), good)
	}
	if in := interactionByID(t, c)[bad]; in.Error != nil || in.DurationMillis == nil {
		t.Errorf("interaction %d = %+v, want it complete: a failed event does not mark it", bad, in)
	}
	if st := w.Stats(); st.DroppedEvents != 2 || st.LostInteractions != 0 {
		t.Errorf("stats = %+v, want 2 dropped events and no lost interaction", st)
	}
}

func TestTxDeleteEventsAndSetDerivation(t *testing.T) {
	ctx := context.Background()
	c, _ := newCassette(t)
	write(t, c, func(tx *Tx) error {
		if err := tx.InsertSession(ctx, testSession()); err != nil {
			return err
		}
		if err := tx.AppendEvent(ctx, Event{Seq: 1, Kind: "user_message", Source: "user", Target: "model",
			StartedAt: at(1), Status: "ok", Provenance: "recorded", DeriverVersion: 1}); err != nil {
			return err
		}
		return tx.AppendEvent(ctx, Event{Seq: 2, ParentSeq: ptr(int64(1)), Kind: "model_response", Source: "model",
			Target: "agent", StartedAt: at(1), Status: "ok", Provenance: "recorded", DeriverVersion: 1})
	})
	write(t, c, func(tx *Tx) error {
		if err := tx.DeleteEvents(ctx); err != nil {
			return err
		}
		return tx.SetDerivation(ctx, 7, "2099-01-01")
	})
	if events, _ := c.Events(ctx); len(events) != 0 {
		t.Errorf("events = %+v, want none", events)
	}
	if s, _ := c.Session(ctx); s.DeriverVersion != 7 || s.PricingVersion != "2099-01-01" {
		t.Errorf("session versions = %d, %q", s.DeriverVersion, s.PricingVersion)
	}
}

// retryDeriver emits a user_message once per distinct request body, as the
// real Deriver does once per distinct user message (spec §7.4), and a
// model_response with a tool_use child for each completion.
type retryDeriver struct {
	users    map[string]int64 // request body → seq of its user_message
	requests map[int64]int64  // interaction id → seq of its user_message
}

func (d *retryDeriver) Begin(in Interaction, request []byte, emit EmitFunc) {
	seq, ok := d.users[string(request)]
	if !ok {
		kind := "user_message"
		if string(request) == "bad" {
			kind = "invalid" // fails the CHECK constraint
		}
		id := in.ID
		seq = emit(Event{InteractionID: &id, Kind: kind, Source: "user", Target: "model",
			StartedAt: in.StartedAt, Status: "ok", Provenance: "recorded", DeriverVersion: 1})
		d.users[string(request)] = seq
	}
	d.requests[in.ID] = seq
}

func (d *retryDeriver) Chunk(Chunk) {}

func (d *retryDeriver) Complete(id int64, _ Completion, _ []byte, emit EmitFunc) {
	parent := d.requests[id]
	resp := emit(Event{ParentSeq: &parent, InteractionID: &id, Kind: "model_response", Source: "model",
		Target: "agent", StartedAt: at(1), Status: "ok", Provenance: "recorded", DeriverVersion: 1})
	emit(Event{ParentSeq: &resp, InteractionID: &id, Kind: "tool_use", Source: "model", Target: "agent",
		StartedAt: at(1), Tool: ptr("search"), Status: "ok", Provenance: "recorded", DeriverVersion: 1})
}

// AC-26: when a user_message fails to write, the later retries of the same
// request still point at its seq, so their events are dropped as children of
// a dropped event, also those derived while closing. The session counts every
// one of them, and the interactions stay complete: reindex rebuilds the events
// (spec §7.6, §7.7).
func TestWriterCountsTheDroppedChildrenOfADroppedUserMessage(t *testing.T) {
	ctx := context.Background()
	d := &retryDeriver{users: map[string]int64{}, requests: map[int64]int64{}}
	w, c, _ := newWriter(t, WriterOptions{Deriver: d})

	req := func(sec int, body string) int64 {
		return w.BeginInteraction(llmRequest(sec, []byte(body)), []byte(body))
	}
	complete := func(id int64) { w.CompleteInteraction(id, Completion{StatusCode: ptr(200), DurationMillis: 1}) }
	first := req(1, "bad") // its user_message (seq 1) fails; seqs 2 and 3 are its children
	complete(first)
	// The retry is derived once the writer knows that seq 1 was dropped.
	waitFor(t, func() bool { return w.Stats().DroppedEvents == 3 })
	retry := req(2, "bad") // seqs 4 and 5, children of seq 1
	complete(retry)
	other := req(3, "good") // seqs 6, 7 and 8 are written
	complete(other)
	waitFor(t, func() bool { return w.Stats().Written >= 9 }) // 6 interaction records and 3 events
	// A retry still in flight whose chunk fails to write: Close completes it,
	// and derives seqs 9 and 10, at the end of the log.
	lost := req(4, "bad")
	w.AppendChunk(Chunk{InteractionID: lost, Index: 0, Data: []byte("x")})
	w.AppendChunk(Chunk{InteractionID: lost, Index: 0, Data: []byte("x")}) // duplicate key
	closeWriter(t, w)

	events, err := c.Events(ctx)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var stored []int64
	for _, e := range events {
		stored = append(stored, e.Seq)
	}
	if want := []int64{6, 7, 8}; fmt.Sprint(stored) != fmt.Sprint(want) {
		t.Errorf("stored seqs %v, want %v", stored, want)
	}
	if s, _ := c.Session(ctx); s.DroppedEvents != 7 {
		t.Errorf("session dropped_events = %d, want 7 (seqs 1-5, 9 and 10)", s.DroppedEvents)
	}
	all := interactionByID(t, c)
	for _, id := range []int64{first, retry, other} {
		if in := all[id]; in.Error != nil || in.DurationMillis == nil {
			t.Errorf("interaction %d = %+v, want it complete: a dropped event does not mark it", id, in)
		}
	}
	if in := all[lost]; !MarksIncomplete(in.Error) {
		t.Errorf("interaction %d = %+v, want it marked as incomplete", lost, in)
	}
}

// ADR 0010: reindex changes dropped_events after the session has ended.
func TestTxSetDroppedEvents(t *testing.T) {
	ctx := context.Background()
	c, _ := newCassette(t)
	write(t, c, func(tx *Tx) error {
		if err := tx.InsertSession(ctx, testSession()); err != nil {
			return err
		}
		return tx.EndSession(ctx, SessionEnd{Status: "completed", EndedAt: at(3), DroppedEvents: 5})
	})
	tests := []struct {
		name    string
		n       int
		want    int // the stored count afterwards
		wantErr bool
	}{
		{"reset", 0, 0, false},
		{"set", 2, 2, false},
		{"negative", -1, 2, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := c.Write(ctx, func(tx *Tx) error { return tx.SetDroppedEvents(ctx, tt.n) })
			if (err != nil) != tt.wantErr {
				t.Fatalf("SetDroppedEvents(%d) error = %v, want error %v", tt.n, err, tt.wantErr)
			}
			if s, err := c.Session(ctx); err != nil || s.DroppedEvents != tt.want {
				t.Errorf("dropped events = %d (err %v), want %d", s.DroppedEvents, err, tt.want)
			}
		})
	}
}
