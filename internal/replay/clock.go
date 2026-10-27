package replay

import (
	"context"
	"time"
)

// Clock is the time source of realtime replay. It is injected so tests can
// check the schedule of chunks without waiting for it.
type Clock interface {
	Now() time.Time
	// Sleep waits for d. It returns early with ctx.Err() if ctx is done first.
	Sleep(ctx context.Context, d time.Duration) error
}

// systemClock is the wall clock.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
