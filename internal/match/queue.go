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
}

// NewQueues places each interaction of ins in the queue of its fingerprint,
// in the order of ins, which must be recorded order (as returned by
// cassette.Cassette.Interactions). The fingerprint function is called once per
// interaction and returns ok false for interactions that are not matched, such
// as those on a channel the replay mode forwards (ADR 0007 point 3).
//
// An interaction whose recording is incomplete (cassette.Interaction.Incomplete)
// keeps its place in its queue but cannot be served, because the cassette does
// not hold the response the agent received. It blocks the queue: a request
// that reaches it is a miss and it stays at the head, so the divergence shows
// up at the step where it happened. It is never reported as unconsumed
// (spec §5 step 2, AC-28).
//
// An interaction with only a transport error was recorded faithfully, so it
// is queued and served as recorded, error included.
func NewQueues(ins []cassette.Interaction, fingerprint func(cassette.Interaction) (fp string, ok bool)) *Queues {
	q := &Queues{heads: make(map[string][]int)}
	for _, in := range ins {
		fp, ok := fingerprint(in)
		if !ok {
			continue
		}
		q.heads[fp] = append(q.heads[fp], len(q.queued))
		q.queued = append(q.queued, in)
	}
	q.consumed = make([]bool, len(q.queued))
	return q
}

// Next consumes and returns the head of the queue for fp. It returns false on
// a cassette miss: no interaction was recorded with fp, all of them were
// already consumed, or the head is an incomplete interaction (see Blocked).
func (q *Queues) Next(fp string) (cassette.Interaction, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	queue := q.heads[fp]
	if len(queue) == 0 || q.queued[queue[0]].Incomplete() {
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

// Blocked returns the head of the queue for fp when it is an incomplete
// interaction, which makes every request with fp a miss. The divergence event
// of such a miss reports it (AC-28). A blocked head never moves, so Blocked
// agrees with a miss that Next returned, even under concurrent use.
func (q *Queues) Blocked(fp string) (cassette.Interaction, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	queue := q.heads[fp]
	if len(queue) == 0 || !q.queued[queue[0]].Incomplete() {
		return cassette.Interaction{}, false
	}
	return q.queued[queue[0]], true
}

// NextUnconsumed returns the first complete queued interaction, in recorded
// order, that was not consumed and satisfies keep, without consuming it. It is the
// reference for the structural diff of a miss (spec §5 step 4), where keep
// selects the channel and server of the missed request. keep runs with q
// locked, so it must not call methods of q.
func (q *Queues) NextUnconsumed(keep func(cassette.Interaction) bool) (cassette.Interaction, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, in := range q.queued {
		if !q.consumed[i] && !in.Incomplete() && keep(in) {
			return in, true
		}
	}
	return cassette.Interaction{}, false
}

// Unconsumed returns the complete queued interactions that were never
// consumed, in recorded order, for the end-of-replay warning (spec §4.4).
// Interactions behind a blocked head are among them.
func (q *Queues) Unconsumed() []cassette.Interaction {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []cassette.Interaction
	for i, in := range q.queued {
		if !q.consumed[i] && !in.Incomplete() {
			out = append(out, in)
		}
	}
	return out
}
