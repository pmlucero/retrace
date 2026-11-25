package cassette

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

// chainEvents returns n events where each one is the child of the previous one.
func chainEvents(n int) []Event {
	events := make([]Event, n)
	for i := range events {
		e := Event{Seq: int64(i + 1), Kind: "user_message", Source: "user", Target: "agent",
			StartedAt: at(0), Status: "ok", Provenance: "recorded", DeriverVersion: 1}
		if i > 0 {
			p := int64(i)
			e.ParentSeq = &p
		}
		events[i] = e
	}
	return events
}

// BenchmarkReplaceAllEvents measures the delete-and-reinsert write that
// reindex performs, on a cassette whose events have parent links.
func BenchmarkReplaceAllEvents(b *testing.B) {
	for _, n := range []int{5000, 10000, 20000} {
		b.Run(fmt.Sprintf("events=%d", n), func(b *testing.B) {
			ctx := context.Background()
			c, err := Create(ctx, filepath.Join(b.TempDir(), "s.cassette"))
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = c.Close() })
			events := chainEvents(n)
			replace := func(tx *Tx) error {
				if err := tx.DeleteEvents(ctx); err != nil {
					return err
				}
				for _, e := range events {
					if err := tx.AppendEvent(ctx, e); err != nil {
						return err
					}
				}
				return nil
			}
			if err := c.Write(ctx, func(tx *Tx) error {
				if err := tx.InsertSession(ctx, testSession()); err != nil {
					return err
				}
				return replace(tx)
			}); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := c.Write(ctx, replace); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
