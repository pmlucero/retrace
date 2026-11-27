package cassette

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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

// After Close cancels the writes, it waits this long for the writing goroutine
// to stop. A write stuck in the kernel ignores cancellation, and Close must
// still return. finishTimeout bounds the final transaction the same way.
const (
	defaultCloseGrace = 2 * time.Second
	finishTimeout     = 5 * time.Second
)

// ErrWriterStuck is returned by Close when the writing goroutine did not stop,
// for example on a hung disk. The session was not ended: the cassette stays a
// valid prefix of the log, with status recording.
var ErrWriterStuck = errors.New("cassette writer did not stop")

// WriterOptions configures a Writer. Zero values select the defaults.
type WriterOptions struct {
	QueueLen   int   // maximum queued records
	QueueBytes int64 // maximum queued payload bytes; a single larger record is still accepted into an empty queue
	BatchLen   int   // maximum records per transaction
	Logger     *slog.Logger
	// Deriver, if set, derives the semantic events of the recording (spec
	// §7.4) on the writing goroutine.
	Deriver Deriver

	// beforeBatch runs before each batch is written. Tests use it to stall the
	// writer.
	beforeBatch func(ctx context.Context)
	closeGrace  time.Duration
}

// Deriver derives semantic events (spec §7.4) from the records a Writer
// stores. It runs on the writing goroutine, so derivation never delays the
// proxy (ADR 0009), and it sees the records in the order they were queued,
// with the bytes as stored, after redaction.
//
// It sees each record the Writer stored, once. A record that was dropped is
// not shown, and neither is the rest of its interaction, until Close marks
// the interaction as incomplete: the Deriver then sees that completion (and
// its request, if that was dropped too), as stored. So it always sees what
// `retrace reindex` reads (spec §7.6).
type Deriver interface {
	// Begin sees an interaction when its request is recorded. request is the
	// stored request body, or nil.
	Begin(in Interaction, request []byte, emit EmitFunc)
	// Chunk sees one SSE event of a streamed response.
	Chunk(ch Chunk)
	// Complete sees the end of interaction id. response is the stored
	// response body, or nil (always nil when streamed).
	Complete(id int64, done Completion, response []byte, emit EmitFunc)
}

// EmitFunc appends an event derived from the record being handled and
// returns the seq assigned to it, ignoring e.Seq. The event is written in the
// same transaction as that record. If it cannot be written, its seq stays a
// gap, and a later event whose parent it is gets dropped too: both are counted
// in the session (spec §7.7).
type EmitFunc func(e Event) int64

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
// one goroutine writes them in batched transactions. Its methods copy the
// slices they are given, so callers may reuse their buffers right away.
//
// When the queue is full the record is dropped instead of waiting. Its
// interaction is then marked as lost: later records of it are dropped too, and
// Close completes it with an error that says the recording is incomplete. The
// cassette stays consistent and the gap is visible, which is preferable to
// stalling the agent's stream (docs/plan/mvp.md, Design points).
//
// The Writer assigns interaction ids and event seqs when it accepts a record,
// so callers never wait for the database. Callers never log either: warnings
// are emitted by the writing goroutine, so a blocked stderr cannot stall the
// proxy.
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
	// droppedSeqs grows by one entry per dropped event until Close. That is
	// intended: an entry is a few bytes, and it is what keeps a child event
	// from referencing a parent that was never written.
	droppedSeqs map[int64]bool
	mcpServers  json.RawMessage // latest value, written again by Close if one was dropped
	mcpDropped  bool
	warnings    []warning // logged by the writing goroutine, outside mu
	stats       WriterStats
}

type warning struct {
	msg  string
	args []any
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
	begin   *Interaction // set when the begin record itself was dropped: the row does not exist
	done    *Completion  // set when the completion was dropped
	dropped int
	reason  string // why the first record was dropped
}

// NewWriter starts a Writer on c, whose session row must already exist. Ids
// and seqs continue after the ones already in the cassette.
func NewWriter(ctx context.Context, c *Cassette, opts WriterOptions) (*Writer, error) {
	if c.readOnly {
		return nil, fmt.Errorf("writing %s: %w", c.path, ErrReadOnly)
	}
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
	if opts.closeGrace <= 0 {
		opts.closeGrace = defaultCloseGrace
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
	in.RequestHeaders = bytes.Clone(in.RequestHeaders)
	blobs = cloneAll(blobs)
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
	ch.Data = bytes.Clone(ch.Data)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.enqueueLocked(record{kind: recChunk, id: ch.InteractionID, chunk: ch, size: int64(len(ch.Data))})
}

// CompleteInteraction queues the response of interaction id. blobs are the
// bodies that done references, such as a non-streamed response.
func (w *Writer) CompleteInteraction(id int64, done Completion, blobs ...[]byte) {
	done.ResponseHeaders = bytes.Clone(done.ResponseHeaders)
	blobs = cloneAll(blobs)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.enqueueLocked(record{kind: recComplete, id: id, done: done, blobs: blobs,
		size: blobsSize(blobs) + int64(len(done.ResponseHeaders))})
}

// AppendEvent queues an event and returns the seq it assigned, ignoring e.Seq.
func (w *Writer) AppendEvent(e Event) int64 {
	e.Arguments, e.Result = bytes.Clone(e.Arguments), bytes.Clone(e.Result)
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
	servers = bytes.Clone(servers)
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
// The Writer sets end.DroppedEvents itself, to the number of events it
// dropped until the session ended (spec §7.7, ADR 0010), and Close refuses
// a nonzero value. An event given after that is dropped and counted in
// Stats, but not in the session: its row is already final.
//
// If ctx is done before the queue is drained, the remaining records are
// dropped (and their interactions marked as lost), the session is still ended,
// and the error wraps ctx.Err(). If the writing goroutine does not stop even
// then, Close gives up without ending the session and returns ErrWriterStuck.
// In every case the cassette is left valid.
func (w *Writer) Close(ctx context.Context, end SessionEnd) error {
	if end.DroppedEvents != 0 {
		return errors.New("closing cassette writer: dropped events are counted by the writer")
	}
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
		errs = append(errs, fmt.Errorf("draining cassette writer: %w", ctx.Err()))
		select {
		case <-w.done:
		case <-time.After(w.opts.closeGrace):
			errs = append(errs, fmt.Errorf("closing %s: %w", w.c.path, ErrWriterStuck))
			return errors.Join(errs...)
		}
	}
	w.stop()

	// Finishing is a handful of rows, and skipping it would leave the session
	// "recording", so it gets its own deadline even when ctx is already done.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	var finishErrs []error
	err := w.c.Write(fctx, func(tx *Tx) error {
		var err error
		finishErrs, err = w.finish(fctx, tx, end)
		return err
	})
	if err != nil {
		errs = append(errs, fmt.Errorf("finishing cassette %s: %w", w.c.path, err))
	} else {
		errs = append(errs, finishErrs...)
	}
	w.flushWarnings()
	return errors.Join(errs...)
}
