package cassette

import (
	"errors"
	"fmt"
)

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
		w.flushWarnings()
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

	live := w.derive(w.filter(batch))
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

// derive runs the Deriver over live and returns live with the events it
// emits placed right after the record they come from. The derived events are
// written with their record, and a failure to write one is handled like that
// of any queued event.
func (w *Writer) derive(live []record) []record {
	d := w.opts.Deriver
	if d == nil {
		return live
	}
	out := make([]record, 0, len(live))
	emit := func(e Event) int64 {
		var id int64
		if e.InteractionID != nil {
			id = *e.InteractionID
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		w.nextSeq++
		e.Seq = w.nextSeq
		r := record{kind: recEvent, id: id, event: e}
		// A Deriver may reference an event that was dropped after it was
		// emitted, such as the user_message of a retried request. Dropping
		// the child here keeps it from failing the whole batch.
		if w.blockedLocked(r) {
			w.dropLocked(r, "an earlier record it depends on was dropped")
			return e.Seq
		}
		out = append(out, r)
		return e.Seq
	}
	for _, r := range live {
		out = append(out, r)
		switch r.kind {
		case recBegin:
			d.Begin(r.begin, blobByKey(r.blobs, r.begin.RequestBlob), emit)
		case recChunk:
			d.Chunk(r.chunk)
		case recComplete:
			d.Complete(r.id, r.done, blobByKey(r.blobs, r.done.ResponseBlob), emit)
		}
	}
	return out
}

// blobByKey returns the blob of blobs stored under key, or nil.
func blobByKey(blobs [][]byte, key *string) []byte {
	if key == nil {
		return nil
	}
	for _, b := range blobs {
		if BlobKey(b) == *key {
			return b
		}
	}
	return nil
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
			w.mu.Lock()
			w.warnLocked("interaction completed twice; keeping the first completion", "interaction", r.id)
			w.mu.Unlock()
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
