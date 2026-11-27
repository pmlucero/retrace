package cassette

import "bytes"

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
	case w.queuedBytes > 0 && w.queuedBytes+r.size > w.opts.QueueBytes:
		// An empty queue takes any record, so the bound limits how much waits
		// and never rules out one large request.
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
func (w *Writer) blockedLocked(r record) bool {
	switch r.kind {
	case recBegin, recMCP:
		// A begin record is the first of its interaction; MCP metadata is
		// session-wide.
		return false
	case recEvent:
		if r.event.ParentSeq != nil && w.droppedSeqs[*r.event.ParentSeq] {
			return true
		}
		// An event only needs its interaction's row, which exists unless the
		// begin record itself was dropped.
		l := w.lost[r.id]
		return r.id != 0 && l != nil && l.begin != nil
	}
	return r.id != 0 && w.lost[r.id] != nil
}

// dropLocked records that r will not be written.
func (w *Writer) dropLocked(r record, reason string) {
	w.stats.Dropped++
	switch r.kind {
	case recMCP:
		w.mcpDropped = true
		w.warnLocked("cassette writer dropped MCP server metadata; it will be written on close", "reason", reason)
		return
	case recEvent:
		// Events are not marked on their interaction: it may already be
		// complete. The session counts them instead, and their seq is never
		// handed out again, so it stays a gap (spec §7.7).
		w.droppedSeqs[r.event.Seq] = true
		w.stats.DroppedEvents++
		if w.stats.DroppedEvents == 1 {
			w.warnLocked("cassette writer dropped an event; further drops are counted, not logged",
				"seq", r.event.Seq, "reason", reason)
		}
		return
	}
	l := w.lost[r.id]
	if l == nil {
		l = &lostInteraction{reason: reason}
		w.lost[r.id] = l
		w.stats.LostInteractions++
		w.warnLocked("cassette writer dropped a record; the interaction will be marked as incomplete",
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

func (w *Writer) warnLocked(msg string, args ...any) {
	w.warnings = append(w.warnings, warning{msg: msg, args: args})
}

// flushWarnings logs the queued warnings. Only the writing goroutine and
// Close call it, never the proxy.
func (w *Writer) flushWarnings() {
	w.mu.Lock()
	warnings := w.warnings
	w.warnings = nil
	w.mu.Unlock()
	for _, wn := range warnings {
		w.log.Warn(wn.msg, wn.args...)
	}
}

func cloneAll(blobs [][]byte) [][]byte {
	out := make([][]byte, len(blobs))
	for i, b := range blobs {
		out[i] = bytes.Clone(b)
	}
	return out
}

func blobsSize(blobs [][]byte) int64 {
	var n int64
	for _, b := range blobs {
		n += int64(len(b))
	}
	return n
}
