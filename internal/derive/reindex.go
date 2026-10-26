package derive

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/provider/anthropic"
)

var (
	// ErrRecording is returned by Reindex for a session that may still be
	// recording (spec §7.6).
	ErrRecording = errors.New("the session is still recording")
	// ErrNewerDeriver is returned by Reindex for a cassette whose events
	// were derived by a newer version of retrace (spec §7.6).
	ErrNewerDeriver = errors.New("the events were derived by a newer version of retrace")
)

// Reindex rebuilds the events of c from its interactions with the current
// Deriver, and returns how many events it wrote (spec §7.6). It replaces the
// events in one transaction, keeps those that are not derived from
// interactions (divergence), and records the deriver and pricing versions in
// the session. Interactions are never modified.
func Reindex(ctx context.Context, c *cassette.Cassette) (int, error) {
	s, err := c.Session(ctx)
	if err != nil {
		return 0, err
	}
	if s.Status == "recording" {
		return 0, fmt.Errorf("reindexing %s: %w", s.ID, ErrRecording)
	}
	if s.DeriverVersion > Version {
		return 0, fmt.Errorf("reindexing %s (deriver v%d, this retrace has v%d): %w", s.ID, s.DeriverVersion, Version, ErrNewerDeriver)
	}
	derived, err := deriveAll(ctx, c)
	if err != nil {
		return 0, fmt.Errorf("reindexing %s: %w", s.ID, err)
	}
	old, err := c.Events(ctx)
	if err != nil {
		return 0, fmt.Errorf("reindexing %s: %w", s.ID, err)
	}
	events := merge(derived, notDerived(old))

	err = c.Write(ctx, func(tx *cassette.Tx) error {
		if err := tx.DeleteEvents(ctx); err != nil {
			return err
		}
		for _, e := range events {
			if err := tx.AppendEvent(ctx, e); err != nil {
				return err
			}
		}
		return tx.SetDerivation(ctx, Version, anthropic.PricingVersion())
	})
	if err != nil {
		return 0, fmt.Errorf("reindexing %s: %w", s.ID, err)
	}
	return len(events), nil
}

// deriveAll runs a Deriver over the interactions of c in the order the
// recording handled them, and returns the events in seq order from 1.
func deriveAll(ctx context.Context, c *cassette.Cassette) ([]cassette.Event, error) {
	interactions, err := c.Interactions(ctx)
	if err != nil {
		return nil, err
	}
	var events []cassette.Event
	emit := func(e cassette.Event) int64 {
		e.Seq = int64(len(events) + 1)
		events = append(events, e)
		return e.Seq
	}
	d := New()
	for _, step := range recordingOrder(interactions) {
		in := step.in
		if !step.complete {
			request, err := blob(ctx, c, in.RequestBlob)
			if err != nil {
				return nil, fmt.Errorf("interaction %d: %w", in.ID, err)
			}
			d.Begin(*in, request, emit)
			continue
		}
		if in.Streamed {
			chunks, err := c.Chunks(ctx, in.ID)
			if err != nil {
				return nil, err
			}
			for _, ch := range chunks {
				d.Chunk(ch)
			}
		}
		response, err := blob(ctx, c, in.ResponseBlob)
		if err != nil {
			return nil, fmt.Errorf("interaction %d: %w", in.ID, err)
		}
		d.Complete(in.ID, completion(*in), response, emit)
	}
	return events, nil
}

// step is one record the recording handled: an interaction's request, or its
// completion.
type step struct {
	in       *cassette.Interaction
	complete bool
}

// recordingOrder returns the steps of interactions in the order the cassette
// writer saw them (spec §7.6). Requests were queued in id order. A
// completion was queued when its response ended, at started_at plus
// duration_ms, which is known to the millisecond. A completion is placed
// before a request that started at or after it ended, and never before its
// own request.
func recordingOrder(interactions []cassette.Interaction) []step {
	begins := make([]*cassette.Interaction, len(interactions))
	var completes []*cassette.Interaction
	for i := range interactions {
		in := &interactions[i]
		begins[i] = in
		if in.DurationMillis != nil { // in flight when the recording stopped otherwise
			completes = append(completes, in)
		}
	}
	slices.SortFunc(begins, func(a, b *cassette.Interaction) int { return compareInt(a.ID, b.ID) })
	slices.SortStableFunc(completes, func(a, b *cassette.Interaction) int {
		if c := endOf(a).Compare(endOf(b)); c != 0 {
			return c
		}
		return compareInt(a.ID, b.ID)
	})

	out := make([]step, 0, len(begins)+len(completes))
	begun := map[int64]bool{}
	for len(begins) > 0 || len(completes) > 0 {
		if len(completes) > 0 {
			c := completes[0]
			if begun[c.ID] && (len(begins) == 0 || !endOf(c).After(begins[0].StartedAt)) {
				out = append(out, step{in: c, complete: true})
				completes = completes[1:]
				continue
			}
		}
		b := begins[0]
		begins = begins[1:]
		begun[b.ID] = true
		out = append(out, step{in: b})
	}
	return out
}

func endOf(in *cassette.Interaction) time.Time {
	return in.StartedAt.Add(time.Duration(*in.DurationMillis) * time.Millisecond)
}

func compareInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// completion rebuilds what the writer was given when in completed.
func completion(in cassette.Interaction) cassette.Completion {
	return cassette.Completion{
		StatusCode: in.StatusCode, ResponseHeaders: in.ResponseHeaders, ResponseBlob: in.ResponseBlob,
		Streamed: in.Streamed, TTFBMillis: in.TTFBMillis, DurationMillis: *in.DurationMillis, Error: in.Error,
	}
}

func blob(ctx context.Context, c *cassette.Cassette, key *string) ([]byte, error) {
	if key == nil {
		return nil, nil
	}
	return c.Blob(ctx, *key)
}

// notDerived returns the events that are not derived from interactions, and
// so survive a reindex (spec §7.6).
func notDerived(events []cassette.Event) []cassette.Event {
	var out []cassette.Event
	for _, e := range events {
		if e.Kind == "divergence" {
			out = append(out, e)
		}
	}
	return out
}

// merge places kept events among derived ones by started_at, after derived
// events that start at the same time, and renumbers seqs from 1. Parent links
// follow the renumbering. A kept event whose parent was not kept loses it.
func merge(derived, kept []cassette.Event) []cassette.Event {
	if len(kept) == 0 {
		return derived
	}
	out := make([]cassette.Event, 0, len(derived)+len(kept))
	newSeq := map[int64]int64{} // old seq of a kept event → new seq
	derivedSeq := map[int64]int64{}
	i, j := 0, 0
	for i < len(derived) || j < len(kept) {
		takeKept := j < len(kept) && (i == len(derived) || kept[j].StartedAt.Before(derived[i].StartedAt))
		var e cassette.Event
		if takeKept {
			e = kept[j]
			j++
			if e.ParentSeq != nil {
				if p, ok := newSeq[*e.ParentSeq]; ok {
					e.ParentSeq = &p
				} else {
					e.ParentSeq = nil
				}
			}
		} else {
			e = derived[i]
			i++
			if e.ParentSeq != nil {
				p := derivedSeq[*e.ParentSeq]
				e.ParentSeq = &p
			}
		}
		seq := int64(len(out) + 1)
		if takeKept {
			newSeq[e.Seq] = seq
		} else {
			derivedSeq[e.Seq] = seq
		}
		e.Seq = seq
		out = append(out, e)
	}
	return out
}
