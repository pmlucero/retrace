package cassette

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"
)

// Default limits of a Writer's queue. A record holds one chunk, event or
// interaction step, so 4096 records cover a long SSE burst, and 64 MiB caps
// memory when records carry large request or response bodies.
const (
	DefaultQueueLen   = 4096
	DefaultQueueBytes = 64 << 20
	DefaultBatchLen   = 256
)

// recordOverhead approximates the memory of a queued record besides its
// payload, so a flood of tiny records is also bounded by QueueBytes.
const recordOverhead = 256

// WriterOptions configures a Writer. Zero values select the defaults.
type WriterOptions struct {
	QueueLen   int   // maximum queued records
	QueueBytes int64 // maximum queued payload bytes
	BatchLen   int   // maximum records per transaction
	Logger     *slog.Logger

	// beforeBatch runs before each batch is written. Tests use it to stall the
	// writer; it must return when ctx is done.
	beforeBatch func(ctx context.Context)
}

// WriterStats counts what a Writer did with the records it was given.
type WriterStats struct {
	Written          int   // records committed to the cassette
	Dropped          int   // records not written: queue full, write failure, or after Close
	DroppedEvents    int   // dropped records that were events
	LostInteractions int   // interactions marked as incomplete because a record was dropped
	MaxQueueLen      int   // high-water mark of queued records
	MaxQueueBytes    int64 // high-water mark of queued payload bytes
}

// Writer appends a recording to a cassette without ever blocking its caller,
// the proxy's hot path (ADR 0009, AC-03). Records go into a bounded queue, and
// one goroutine writes them in batched transactions.
//
// When the queue is full the record is dropped instead of waiting. Its
// interaction is then marked as lost: later records of it are dropped too, and
// Close completes it with an error that says the recording is incomplete. The
// cassette stays consistent and the gap is visible, which is preferable to
// stalling the agent's stream (docs/plan/mvp.md, Design points).
//
// The Writer assigns interaction ids and event seqs when it accepts a record,
// so callers never wait for the database.
type Writer struct {
	c     *Cassette
	log   *slog.Logger
	opts  WriterOptions
	queue chan record
	ctx   context.Context    // context of every write; Close cancels it when draining runs out of time
	stop  context.CancelFunc // cancels ctx
	done  chan struct{}      // closed when the writing goroutine exits

	// mu guards everything below. Records are queued while holding it, so the
	// queue order matches the order of ids and seqs: an event never reaches the
	// database before the parent event it references.
	mu          sync.Mutex
	closed      bool
	nextID      int64
	nextSeq     int64
	queuedBytes int64
	lost        map[int64]*lostInteraction
	droppedSeqs map[int64]bool
	mcpServers  json.RawMessage // latest value, written again by Close if one was dropped
	mcpDropped  bool
	stats       WriterStats
}

type recordKind int

const (
	recBegin recordKind = iota
	recChunk
	recComplete
	recEvent
	recMCP
)

type record struct {
	kind  recordKind
	id    int64 // interaction id; 0 for events without one and for session records
	size  int64
	begin Interaction
	done  Completion
	chunk Chunk
	event Event
	mcp   json.RawMessage
	blobs [][]byte // stored before the row that references them
}

// lostInteraction remembers what Close needs to finish an interaction whose
// records were not all written.
type lostInteraction struct {
	begin   *Interaction // set when the begin record itself was dropped
	done    *Completion  // set when the completion was dropped
	dropped int
	reason  string // why the first record was dropped
}

// NewWriter starts a Writer on c, whose session row must already exist. Ids
// and seqs continue after the ones already in the cassette.
func NewWriter(ctx context.Context, c *Cassette, opts WriterOptions) (*Writer, error) {
	if !c.writable {
		return nil, fmt.Errorf("writing %s: %w", c.path, ErrNewerSchema)
	}
	var maxID, maxSeq int64
	if err := c.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM interactions`).Scan(&maxID); err != nil {
		return nil, fmt.Errorf("reading last interaction id of %s: %w", c.path, err)
	}
	if err := c.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM events`).Scan(&maxSeq); err != nil {
		return nil, fmt.Errorf("reading last event seq of %s: %w", c.path, err)
	}
	if opts.QueueLen <= 0 {
		opts.QueueLen = DefaultQueueLen
	}
	if opts.QueueBytes <= 0 {
		opts.QueueBytes = DefaultQueueBytes
	}
	if opts.BatchLen <= 0 {
		opts.BatchLen = DefaultBatchLen
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	// The writer outlives the caller's ctx (often a request or startup
	// context); only Close stops it.
	wctx, stop := context.WithCancel(context.WithoutCancel(ctx))
	w := &Writer{
		c: c, log: log, opts: opts,
		queue: make(chan record, opts.QueueLen),
		ctx:   wctx, stop: stop,
		done:        make(chan struct{}),
		nextID:      maxID,
		nextSeq:     maxSeq,
		lost:        map[int64]*lostInteraction{},
		droppedSeqs: map[int64]bool{},
	}
	go w.run()
	return w, nil
}

// BeginInteraction queues a new in-flight interaction and returns the id it
// assigned, ignoring in.ID. blobs are the bodies that in references by
// BlobKey, such as the request and its canonical form.
func (w *Writer) BeginInteraction(in Interaction, blobs ...[]byte) int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.nextID++
	in.ID = w.nextID
	w.enqueueLocked(record{kind: recBegin, id: in.ID, begin: in, blobs: blobs,
		size: blobsSize(blobs) + int64(len(in.RequestHeaders))})
	return in.ID
}

// AppendChunk queues one SSE event of a streamed response.
func (w *Writer) AppendChunk(ch Chunk) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.enqueueLocked(record{kind: recChunk, id: ch.InteractionID, chunk: ch, size: int64(len(ch.Data))})
}

// CompleteInteraction queues the response of interaction id. blobs are the
// bodies that done references, such as a non-streamed response.
func (w *Writer) CompleteInteraction(id int64, done Completion, blobs ...[]byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.enqueueLocked(record{kind: recComplete, id: id, done: done, blobs: blobs,
		size: blobsSize(blobs) + int64(len(done.ResponseHeaders))})
}

// AppendEvent queues an event and returns the seq it assigned, ignoring e.Seq.
func (w *Writer) AppendEvent(e Event) int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.nextSeq++
	e.Seq = w.nextSeq
	var id int64
	if e.InteractionID != nil {
		id = *e.InteractionID
	}
	w.enqueueLocked(record{kind: recEvent, id: id, event: e, size: int64(len(e.Arguments) + len(e.Result))})
	return e.Seq
}

// SetMCPServers queues new MCP server metadata for the session (spec §3.4).
func (w *Writer) SetMCPServers(servers json.RawMessage) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.mcpServers = servers
	w.enqueueLocked(record{kind: recMCP, mcp: servers, size: int64(len(servers))})
}

// Stats returns the counters so far.
func (w *Writer) Stats() WriterStats {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stats
}

// Close stops accepting records, writes the ones still queued, completes lost
// interactions with an error, and ends the session with end. It does not close
// the cassette.
//
// If ctx is done before the queue is drained, the remaining records are
// dropped (and their interactions marked as lost), the session is still ended,
// and the error wraps ctx.Err(). Either way the cassette is left valid.
func (w *Writer) Close(ctx context.Context, end SessionEnd) error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return errors.New("closing cassette writer: already closed")
	}
	w.closed = true
	close(w.queue)
	w.mu.Unlock()

	var errs []error
	select {
	case <-w.done:
	case <-ctx.Done():
		w.stop()
		<-w.done
		errs = append(errs, fmt.Errorf("draining cassette writer: %w", ctx.Err()))
	}
	w.stop()

	// Finishing is a handful of rows, and skipping it would leave the session
	// "recording", so it runs even when ctx is already done.
	fctx := context.WithoutCancel(ctx)
	if err := w.c.Write(fctx, func(tx *Tx) error { return w.finish(fctx, tx, end) }); err != nil {
		errs = append(errs, fmt.Errorf("finishing cassette %s: %w", w.c.path, err))
	}
	return errors.Join(errs...)
}

// enqueueLocked queues r without waiting, or drops it.
func (w *Writer) enqueueLocked(r record) {
	r.size += recordOverhead
	switch {
	case w.closed:
		w.dropLocked(r, "the cassette writer was already closed")
		return
	case w.blockedLocked(r):
		w.dropLocked(r, "an earlier record it depends on was dropped")
		return
	case w.queuedBytes+r.size > w.opts.QueueBytes:
		w.dropLocked(r, "cassette writer queue full (bytes)")
		return
	}
	select {
	case w.queue <- r:
		w.queuedBytes += r.size
		w.stats.MaxQueueLen = max(w.stats.MaxQueueLen, len(w.queue))
		w.stats.MaxQueueBytes = max(w.stats.MaxQueueBytes, w.queuedBytes)
	default:
		w.dropLocked(r, "cassette writer queue full")
	}
}

// blockedLocked reports whether r depends on a record that was dropped.
// Writing it would break a foreign key or leave a gap that looks complete.
// A begin record is never blocked: it is the first record of its interaction.
func (w *Writer) blockedLocked(r record) bool {
	switch r.kind {
	case recBegin, recMCP:
		return false
	case recEvent:
		if r.event.ParentSeq != nil && w.droppedSeqs[*r.event.ParentSeq] {
			return true
		}
	}
	return r.id != 0 && w.lost[r.id] != nil
}

// dropLocked records that r will not be written.
func (w *Writer) dropLocked(r record, reason string) {
	w.stats.Dropped++
	switch r.kind {
	case recMCP:
		w.mcpDropped = true
		w.log.Warn("cassette writer dropped MCP server metadata; it will be written on close", "reason", reason)
		return
	case recEvent:
		// Events are not marked on their interaction: it may already be
		// complete, and `retrace reindex` can rebuild events (spec §7.6).
		w.droppedSeqs[r.event.Seq] = true
		w.stats.DroppedEvents++
		if w.stats.DroppedEvents == 1 {
			w.log.Warn("cassette writer dropped an event; further drops are counted, not logged", "seq", r.event.Seq, "reason", reason)
		}
		return
	}
	l := w.lost[r.id]
	if l == nil {
		l = &lostInteraction{reason: reason}
		w.lost[r.id] = l
		w.stats.LostInteractions++
		w.log.Warn("cassette writer dropped a record; the interaction will be marked as incomplete",
			"interaction", r.id, "reason", reason)
	}
	l.dropped++
	switch r.kind {
	case recBegin:
		// Its blobs were not stored, so the row must not reference them.
		b := r.begin
		b.RequestBlob, b.CanonicalBlob = nil, nil
		l.begin = &b
	case recComplete:
		d := r.done
		d.ResponseBlob = nil
		l.done = &d
	}
}

func (w *Writer) run() {
	defer close(w.done)
	batch := make([]record, 0, w.opts.BatchLen)
	for r := range w.queue {
		batch = append(batch[:0], r)
	fill:
		for len(batch) < cap(batch) {
			select {
			case r, ok := <-w.queue:
				if !ok {
					break fill
				}
				batch = append(batch, r)
			default:
				break fill
			}
		}
		if w.opts.beforeBatch != nil {
			w.opts.beforeBatch(w.ctx)
		}
		w.writeBatch(batch)
	}
}

// writeBatch writes batch in one transaction. If that fails, it writes each
// record in its own transaction, so one bad record does not lose the others.
func (w *Writer) writeBatch(batch []record) {
	defer func() {
		w.mu.Lock()
		for _, r := range batch {
			w.queuedBytes -= r.size
		}
		w.mu.Unlock()
	}()

	live := w.filter(batch)
	if len(live) == 0 {
		return
	}
	err := w.c.Write(w.ctx, func(tx *Tx) error {
		for _, r := range live {
			if err := w.apply(tx, r); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		w.mu.Lock()
		w.stats.Written += len(live)
		w.mu.Unlock()
		return
	}
	for _, r := range live {
		if w.filter([]record{r}) == nil {
			continue // an earlier record of this batch failed and took r with it
		}
		err := w.c.Write(w.ctx, func(tx *Tx) error { return w.apply(tx, r) })
		w.mu.Lock()
		if err != nil {
			reason := "writing to the cassette failed: " + err.Error()
			if w.ctx.Err() != nil {
				reason = "the recording ended before it was written"
			}
			w.dropLocked(r, reason)
		} else {
			w.stats.Written++
		}
		w.mu.Unlock()
	}
}

// filter drops the records of batch that depend on a dropped record. A record
// can become blocked after it was queued, when an earlier one fails to write.
func (w *Writer) filter(batch []record) []record {
	w.mu.Lock()
	defer w.mu.Unlock()
	var live []record
	for _, r := range batch {
		if w.blockedLocked(r) {
			w.dropLocked(r, "an earlier record it depends on was dropped")
			continue
		}
		live = append(live, r)
	}
	return live
}

func (w *Writer) apply(tx *Tx, r record) error {
	for _, b := range r.blobs {
		if _, err := tx.PutBlob(w.ctx, b); err != nil {
			return err
		}
	}
	switch r.kind {
	case recBegin:
		return tx.BeginInteraction(w.ctx, r.begin)
	case recChunk:
		return tx.AppendChunk(w.ctx, r.chunk)
	case recComplete:
		err := tx.CompleteInteraction(w.ctx, r.id, r.done)
		if errors.Is(err, ErrAlreadyCompleted) {
			// The first completion stands and the row is unchanged (ADR 0009).
			w.log.Warn("interaction completed twice; keeping the first completion", "interaction", r.id)
			return nil
		}
		return err
	case recEvent:
		return tx.AppendEvent(w.ctx, r.event)
	case recMCP:
		return tx.SetMCPServers(w.ctx, r.mcp)
	}
	return fmt.Errorf("unknown record kind %d", r.kind)
}

// finish completes the lost interactions with an error and ends the session.
// The writing goroutine has exited, so nothing else touches the cassette.
func (w *Writer) finish(ctx context.Context, tx *Tx, end SessionEnd) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	ids := make([]int64, 0, len(w.lost))
	for id := range w.lost {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		// A failed statement does not abort the transaction, so one interaction
		// that cannot be finished does not keep the session from ending.
		if err := w.finishLost(ctx, tx, id, w.lost[id], end.EndedAt); err != nil {
			w.log.Warn("could not mark an incomplete interaction", "interaction", id, "error", err)
		}
	}
	if w.mcpDropped {
		if err := tx.SetMCPServers(ctx, w.mcpServers); err != nil {
			return err
		}
	}
	if w.stats.Dropped > 0 {
		w.log.Warn("cassette recording is incomplete",
			"dropped_records", w.stats.Dropped, "dropped_events", w.stats.DroppedEvents,
			"incomplete_interactions", w.stats.LostInteractions)
	}
	return tx.EndSession(ctx, end)
}

func (w *Writer) finishLost(ctx context.Context, tx *Tx, id int64, l *lostInteraction, endedAt time.Time) error {
	var (
		startedAt string
		duration  sql.NullInt64
	)
	err := tx.tx.QueryRowContext(ctx, `SELECT started_at, duration_ms FROM interactions WHERE id = ?`, id).
		Scan(&startedAt, &duration)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if l.begin == nil {
			return nil // nothing of it was ever accepted
		}
		if err := tx.BeginInteraction(ctx, *l.begin); err != nil {
			return err
		}
		startedAt = FormatTime(l.begin.StartedAt)
	case err != nil:
		return fmt.Errorf("reading interaction %d: %w", id, err)
	case duration.Valid:
		// Already complete, and complete rows are never updated (ADR 0009).
		w.log.Warn("records of a completed interaction were dropped", "interaction", id, "reason", l.reason)
		return nil
	}

	msg := fmt.Sprintf("recording incomplete: %d record(s) not written: %s", l.dropped, l.reason)
	done := Completion{}
	if l.done != nil {
		done = *l.done
		if done.Error != nil {
			msg = *done.Error + "; " + msg
		}
	} else {
		// Still in flight when the recording ended: that is when it stopped.
		started, err := parseTime(startedAt)
		if err != nil {
			return fmt.Errorf("reading interaction %d: started_at: %w", id, err)
		}
		done.DurationMillis = max(0, endedAt.Sub(started).Milliseconds())
	}
	done.Error = &msg
	return tx.CompleteInteraction(ctx, id, done)
}

func blobsSize(blobs [][]byte) int64 {
	var n int64
	for _, b := range blobs {
		n += int64(len(b))
	}
	return n
}
