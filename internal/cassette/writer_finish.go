package cassette

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

// finish completes the lost interactions with an error and ends the session.
// The writing goroutine has exited, so nothing else touches the cassette. It
// returns the interactions it could not mark separately from err, which fails
// the transaction.
func (w *Writer) finish(ctx context.Context, tx *Tx, end SessionEnd) (unmarked []error, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	ids := make([]int64, 0, len(w.lost))
	for id := range w.lost {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		// A constraint error rolls back only its statement, so one interaction
		// that cannot be marked does not keep the session from ending. Errors
		// that roll back the whole transaction (a full disk, I/O) make the
		// commit fail, and Close reports that.
		if err := w.finishLost(ctx, tx, id, w.lost[id], end.EndedAt); err != nil {
			unmarked = append(unmarked, fmt.Errorf("marking interaction %d as incomplete: %w", id, err))
		}
	}
	if w.mcpDropped {
		if err := tx.SetMCPServers(ctx, w.mcpServers); err != nil {
			return nil, err
		}
	}
	// w.mu is held and the Writer is closed, so no event can be dropped
	// between this count and the end of the session.
	end.DroppedEvents = w.stats.DroppedEvents
	if w.stats.Dropped > 0 {
		w.warnLocked("cassette recording is incomplete",
			"dropped_records", w.stats.Dropped, "dropped_events", w.stats.DroppedEvents,
			"incomplete_interactions", w.stats.LostInteractions)
	}
	return unmarked, tx.EndSession(ctx, end)
}

func (w *Writer) finishLost(ctx context.Context, tx *Tx, id int64, l *lostInteraction, endedAt time.Time) error {
	startedAt, complete, err := tx.interactionState(ctx, id)
	switch {
	case errors.Is(err, ErrNotFound):
		if l.begin == nil {
			return nil // nothing of it was ever accepted
		}
		if err := tx.BeginInteraction(ctx, *l.begin); err != nil {
			return err
		}
		startedAt = l.begin.StartedAt
		if d := w.opts.Deriver; d != nil {
			d.Begin(*l.begin, nil, w.finishEmit(ctx, tx)) // its request blob was not stored
		}
	case err != nil:
		return err
	case complete:
		// Complete rows are never updated (ADR 0009).
		w.warnLocked("records of a completed interaction were dropped", "interaction", id, "reason", l.reason)
		return nil
	}

	msg := fmt.Sprintf("%s: %d record(s) not written: %s", RecordingIncomplete, l.dropped, l.reason)
	done := Completion{}
	if l.done != nil {
		done = *l.done
		if done.Error != nil {
			msg = *done.Error + "; " + msg
		}
	} else {
		// Still in flight when the recording ended: that is when it stopped.
		done.DurationMillis = max(0, endedAt.Sub(startedAt).Milliseconds())
	}
	done.Error = &msg
	if err := tx.CompleteInteraction(ctx, id, done); err != nil {
		return err
	}
	// The deriver has seen what was stored of the interaction; it now sees the
	// completion as stored, so its events match what reindex derives.
	if d := w.opts.Deriver; d != nil {
		d.Complete(id, done, nil, w.finishEmit(ctx, tx))
	}
	return nil
}

// finishEmit returns an EmitFunc for finish, which holds w.mu: it writes each
// event in tx at once, or drops it like any event whose parent was dropped or
// that cannot be written.
func (w *Writer) finishEmit(ctx context.Context, tx *Tx) EmitFunc {
	return func(e Event) int64 {
		w.nextSeq++
		e.Seq = w.nextSeq
		// Not blockedLocked: the interactions finish inserts are still lost,
		// but their rows now exist, so only the parent can be missing.
		r := record{kind: recEvent, event: e}
		if e.ParentSeq != nil && w.droppedSeqs[*e.ParentSeq] {
			w.dropLocked(r, "an earlier record it depends on was dropped")
			return e.Seq
		}
		if err := tx.AppendEvent(ctx, e); err != nil {
			w.dropLocked(r, "writing to the cassette failed: "+err.Error())
			return e.Seq
		}
		w.stats.Written++
		return e.Seq
	}
}
