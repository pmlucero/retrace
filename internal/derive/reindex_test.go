package derive

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/provider/anthropic"
)

// rawCassette writes exchanges directly, with no deriver, as an older
// retrace (or one whose writer dropped events) would have left them.
func rawCassette(t *testing.T, status string, deriverVersion int, exchanges []exchange) *cassette.Cassette {
	t.Helper()
	ctx := context.Background()
	c, err := cassette.Create(ctx, filepath.Join(t.TempDir(), "raw.cassette"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	err = c.Write(ctx, func(tx *cassette.Tx) error {
		if err := tx.InsertSession(ctx, cassette.Session{ID: "raw", Mode: "record", Status: status, CreatedAt: t0,
			Command: []string{"agent"}, OnMiss: "strict", DeriverVersion: deriverVersion}); err != nil {
			return err
		}
		for i, x := range exchanges {
			in := x.interaction()
			in.ID = int64(i + 1)
			if x.request != "" {
				if _, err := tx.PutBlob(ctx, []byte(x.request)); err != nil {
					return err
				}
			}
			if err := tx.BeginInteraction(ctx, in); err != nil {
				return err
			}
			for j, ch := range x.chunks {
				if err := tx.AppendChunk(ctx, cassette.Chunk{InteractionID: in.ID, Index: j, Data: []byte(ch)}); err != nil {
					return err
				}
			}
			if x.durationMillis < 0 {
				continue // still in flight
			}
			done, blobs := x.completion()
			for _, b := range blobs {
				if _, err := tx.PutBlob(ctx, b); err != nil {
					return err
				}
			}
			if err := tx.CompleteInteraction(ctx, in.ID, done); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Reindex derives what the recording derived, in the same order (spec §7.6).
func TestReindexReproducesRecordedEvents(t *testing.T) {
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			c := record(t, sc.exchanges)
			ctx := context.Background()
			recorded, _ := c.Events(ctx)
			n, err := Reindex(ctx, c)
			if err != nil {
				t.Fatal(err)
			}
			rebuilt, _ := c.Events(ctx)
			if n != len(rebuilt) || !reflect.DeepEqual(recorded, rebuilt) {
				t.Errorf("reindex wrote %d events:\n%s\nrecording had:\n%s", n, eventsJSONL(t, rebuilt), eventsJSONL(t, recorded))
			}
		})
	}
}

func TestReindexDerivesMissingEventsAndUpdatesTheSession(t *testing.T) {
	ctx := context.Background()
	c := rawCassette(t, "interrupted", 0, []exchange{
		{request: weatherQuestion, status: 200, response: weatherToolUse, durationMillis: 900},
		{startMillis: 1000, request: weatherFollowUp, durationMillis: -1}, // in flight when the recording stopped
	})
	n, err := Reindex(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	events, _ := c.Events(ctx)
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	if n != 3 || strings.Join(kinds, ",") != "user_message,model_response,tool_use" {
		t.Errorf("reindex wrote %d events: %v", n, kinds)
	}
	s, _ := c.Session(ctx)
	if s.DeriverVersion != Version || s.PricingVersion != anthropic.PricingVersion() {
		t.Errorf("session versions = %d, %q; want %d, %q", s.DeriverVersion, s.PricingVersion, Version, anthropic.PricingVersion())
	}

	// Reindexing again is idempotent.
	if _, err := Reindex(ctx, c); err != nil {
		t.Fatal(err)
	}
	again, _ := c.Events(ctx)
	if eventsString(t, again) != eventsString(t, events) {
		t.Error("a second reindex changed the events")
	}
}

func eventsString(t *testing.T, events []cassette.Event) string {
	t.Helper()
	return string(eventsJSONL(t, events))
}

func TestReindexKeepsDivergenceEvents(t *testing.T) {
	ctx := context.Background()
	c := rawCassette(t, "diverged", Version, []exchange{
		{request: weatherQuestion, status: 200, response: weatherToolUse, durationMillis: 900},
		{startMillis: 2000, request: weatherFollowUp, status: 200, response: weatherAnswer, durationMillis: 400},
	})
	divergence := func(seq int64, sec int, parent *int64) cassette.Event {
		return cassette.Event{Seq: seq, ParentSeq: parent, Kind: "divergence", Source: "retrace", Target: "agent",
			StartedAt: t0.Add(time.Duration(sec) * time.Millisecond), Result: []byte(`{"channel":"mcp"}`),
			Status: "divergence", Provenance: "replayed", DeriverVersion: Version}
	}
	err := c.Write(ctx, func(tx *cassette.Tx) error {
		// A stale derived event, which reindex replaces, and two divergences.
		if err := tx.AppendEvent(ctx, cassette.Event{Seq: 1, Kind: "user_message", Source: "user", Target: "model",
			StartedAt: t0, Status: "ok", Provenance: "recorded", DeriverVersion: Version}); err != nil {
			return err
		}
		if err := tx.AppendEvent(ctx, divergence(2, 1500, ptr(int64(1)))); err != nil {
			return err
		}
		return tx.AppendEvent(ctx, divergence(3, 1500, ptr(int64(2))))
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Reindex(ctx, c); err != nil {
		t.Fatal(err)
	}
	events, _ := c.Events(ctx)
	var got []string
	for _, e := range events {
		parent := "-"
		if e.ParentSeq != nil {
			parent = fmt.Sprint(*e.ParentSeq)
		}
		got = append(got, fmt.Sprintf("%d:%s<-%s", e.Seq, e.Kind, parent))
	}
	// The divergences go between the first response (ended at 0.9 s) and the
	// second request (2 s). The first loses its parent, which was replaced;
	// the second keeps its link to the first.
	want := "1:user_message<--,2:model_response<-1,3:tool_use<-2,4:divergence<--,5:divergence<-4,6:model_response<--"
	if strings.Join(got, ",") != want {
		t.Errorf("events = %s\nwant     %s", strings.Join(got, ","), want)
	}
}

func TestReindexRefuses(t *testing.T) {
	tests := []struct {
		name           string
		status         string
		deriverVersion int
		want           error
	}{
		{"a session that may still be recording", "recording", Version, ErrRecording},
		{"events from a newer deriver", "completed", Version + 1, ErrNewerDeriver},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			c := rawCassette(t, tt.status, tt.deriverVersion, []exchange{
				{request: weatherQuestion, status: 200, response: weatherToolUse, durationMillis: 900},
			})
			if _, err := Reindex(ctx, c); !errors.Is(err, tt.want) {
				t.Errorf("Reindex error = %v, want %v", err, tt.want)
			}
			if events, _ := c.Events(ctx); len(events) != 0 {
				t.Errorf("a refused reindex wrote %d events", len(events))
			}
		})
	}
}

func TestRecordingOrder(t *testing.T) {
	in := func(id int64, startMillis int64, durationMillis int64) cassette.Interaction {
		x := cassette.Interaction{ID: id, StartedAt: t0.Add(time.Duration(startMillis) * time.Millisecond)}
		if durationMillis >= 0 {
			x.DurationMillis = &durationMillis
		}
		return x
	}
	tests := []struct {
		name         string
		interactions []cassette.Interaction
		want         string // b<id> for a request, c<id> for a completion
	}{
		{"sequential", []cassette.Interaction{in(1, 0, 10), in(2, 20, 10)}, "b1 c1 b2 c2"},
		{"next request in the same millisecond", []cassette.Interaction{in(1, 0, 10), in(2, 10, 5)}, "b1 c1 b2 c2"},
		{"concurrent", []cassette.Interaction{in(1, 0, 100), in(2, 10, 20), in(3, 50, 10)}, "b1 b2 c2 b3 c3 c1"},
		{"zero duration stays after its request", []cassette.Interaction{in(1, 0, 0)}, "b1 c1"},
		{"ids decide among requests", []cassette.Interaction{in(2, 0, 50), in(1, 5, 50)}, "b1 b2 c2 c1"},
		{"in flight", []cassette.Interaction{in(1, 0, -1), in(2, 10, 5)}, "b1 b2 c2"},
		{"completions tie by id", []cassette.Interaction{in(1, 0, 20), in(2, 10, 10), in(3, 30, 1)}, "b1 b2 c1 c2 b3 c3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, s := range recordingOrder(tt.interactions) {
				kind := "b"
				if s.complete {
					kind = "c"
				}
				got = append(got, fmt.Sprintf("%s%d", kind, s.in.ID))
			}
			if strings.Join(got, " ") != tt.want {
				t.Errorf("order = %s, want %s", strings.Join(got, " "), tt.want)
			}
		})
	}
}
