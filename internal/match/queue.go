// Package match matches replayed requests to recorded interactions
// (ADR 0007).
package match

import (
	"sync"

	"github.com/pmlucero/retrace/internal/cassette"
)

// Queues holds one FIFO queue of recorded interactions per fingerprint
// (ADR 0007, spec §5 step 2). It is safe for concurrent use, because the
// replay proxy serves requests concurrently.
type Queues struct {
	mu       sync.Mutex
	queued   []cassette.Interaction // in recorded order
	consumed []bool                 // parallel to queued
	heads    map[string][]int       // fingerprint -> indices into queued, head first
	// incomplete holds, per fingerprint, the first interaction with that
	// fingerprint whose recording is incomplete. It is in no queue.
	incomplete map[string]cassette.Interaction
}

// NewQueues places each interaction of ins in the queue of its fingerprint,
// in the order of ins, which must be recorded order (as returned by
// cassette.Cassette.Interactions). The fingerprint function is called once per
// interaction and returns ok false for interactions that are not matched, such
// as those on a channel the replay mode forwards (ADR 0007 point 3).
//
// An interaction whose recording is incomplete (its error contains
// cassette.RecordingIncomplete) is in no queue, because the cassette does not
// hold the response the agent received: a request that would match it is a
// miss, and it is not reported by Unconsumed (spec §5 step 2, AC-28). Such an
// interaction is still fingerprinted, so that IncompleteFor can explain the
// miss. Later interactions with the same fingerprint move up past it.
//
// An interaction with only a transport error was recorded faithfully, so it
// is queued and served as recorded, error included.
func NewQueues(ins []cassette.Interaction, fingerprint func(cassette.Interaction) (fp string, ok bool)) *Queues {
	q := &Queues{heads: make(map[string][]int), incomplete: make(map[string]cassette.Interaction)}
	for _, in := range ins {
		fp, ok := fingerprint(in)
		if !ok {
			continue
		}
		if in.Incomplete() {
			if _, seen := q.incomplete[fp]; !seen {
				q.incomplete[fp] = in
			}
			continue
		}
		q.heads[fp] = append(q.heads[fp], len(q.queued))
		q.queued = append(q.queued, in)
	}
	q.consumed = make([]bool, len(q.queued))
	return q
}

// Next consumes and returns the head of the queue for fp. It returns false on
// a cassette miss: no interaction was recorded with fp, or all of them were
// already consumed.
func (q *Queues) Next(fp string) (cassette.Interaction, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	queue := q.heads[fp]
	if len(queue) == 0 {
		return cassette.Interaction{}, false
	}
	i := queue[0]
	if len(queue) == 1 {
		delete(q.heads, fp)
	} else {
		q.heads[fp] = queue[1:]
	}
	q.consumed[i] = true
	return q.queued[i], true
}

// IncompleteFor returns the first interaction in recorded order with
// fingerprint fp whose recording is incomplete. After a miss on fp, it tells
// whether the request matched an interaction that is in no queue because its
// recording is incomplete, which the divergence event reports (AC-28). It
// never consumes anything.
func (q *Queues) IncompleteFor(fp string) (cassette.Interaction, bool) {
	in, ok := q.incomplete[fp] // read-only after NewQueues
	return in, ok
}

// NextUnconsumed returns the first queued interaction, in recorded order, that
// was not consumed and satisfies keep, without consuming it. It is the
// reference for the structural diff of a miss (spec §5 step 4), where keep
// selects the channel and server of the missed request. keep runs with q
// locked, so it must not call methods of q.
func (q *Queues) NextUnconsumed(keep func(cassette.Interaction) bool) (cassette.Interaction, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, in := range q.queued {
		if !q.consumed[i] && keep(in) {
			return in, true
		}
	}
	return cassette.Interaction{}, false
}

// Unconsumed returns the queued interactions that were never consumed, in
// recorded order, for the end-of-replay warning (spec §4.4).
func (q *Queues) Unconsumed() []cassette.Interaction {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []cassette.Interaction
	for i, in := range q.queued {
		if !q.consumed[i] {
			out = append(out, in)
		}
	}
	return out
}
