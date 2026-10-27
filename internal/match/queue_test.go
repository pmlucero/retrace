package match

import (
	"slices"
	"sync"
	"testing"

	"github.com/pmlucero/retrace/internal/cassette"
)

// rec is a recorded interaction for the tests. Its fingerprint is stored in
// the interaction, so the tests do not depend on how fingerprints are
// computed.
func rec(id int64, channel, fp string) cassette.Interaction {
	return cassette.Interaction{ID: id, Channel: channel, Fingerprint: &fp}
}

func withError(in cassette.Interaction, msg string) cassette.Interaction {
	in.Error = &msg
	return in
}

// storedFingerprint matches only the llm channel, like exact replay in M3.
func storedFingerprint(in cassette.Interaction) (string, bool) {
	if in.Channel != "llm" || in.Fingerprint == nil {
		return "", false
	}
	return *in.Fingerprint, true
}

func ids(ins []cassette.Interaction) []int64 {
	out := []int64{}
	for _, in := range ins {
		out = append(out, in.ID)
	}
	return out
}

func TestQueues(t *testing.T) {
	tests := []struct {
		name     string
		recorded []cassette.Interaction
		requests []string // fingerprints, in arrival order
		served   []int64  // interaction served per request, 0 for a miss
		left     []int64  // Unconsumed after the requests
	}{
		{
			// AC-12
			name:     "identical requests are served the recorded responses in order",
			recorded: []cassette.Interaction{rec(1, "llm", "a"), rec(2, "llm", "a"), rec(3, "llm", "a")},
			requests: []string{"a", "a", "a"},
			served:   []int64{1, 2, 3},
			left:     []int64{},
		},
		{
			name:     "queues are independent per fingerprint",
			recorded: []cassette.Interaction{rec(1, "llm", "a"), rec(2, "llm", "b"), rec(3, "llm", "a")},
			requests: []string{"b", "a", "a"},
			served:   []int64{2, 1, 3},
			left:     []int64{},
		},
		{
			name:     "unknown fingerprint is a miss",
			recorded: []cassette.Interaction{rec(1, "llm", "a")},
			requests: []string{"x", "a"},
			served:   []int64{0, 1},
			left:     []int64{},
		},
		{
			name:     "exhausted queue is a miss",
			recorded: []cassette.Interaction{rec(1, "llm", "a"), rec(2, "llm", "a")},
			requests: []string{"a", "a", "a"},
			served:   []int64{1, 2, 0},
			left:     []int64{},
		},
		{
			name: "incomplete recording is in no queue and not unconsumed",
			recorded: []cassette.Interaction{
				rec(1, "llm", "a"),
				withError(rec(2, "llm", "a"), cassette.RecordingIncomplete+": 3 record(s) not written: queue full"),
				withError(rec(3, "llm", "b"), cassette.RecordingIncomplete),
			},
			requests: []string{"a", "a", "b"},
			served:   []int64{1, 0, 0},
			left:     []int64{},
		},
		{
			name:     "transport error with a complete recording is served as recorded",
			recorded: []cassette.Interaction{withError(rec(1, "llm", "a"), "reading upstream body: connection reset")},
			requests: []string{"a"},
			served:   []int64{1},
			left:     []int64{},
		},
		{
			name:     "unmatched channel is in no queue and not unconsumed",
			recorded: []cassette.Interaction{rec(1, "mcp", "a"), rec(2, "llm", "a")},
			requests: []string{"a", "a"},
			served:   []int64{2, 0},
			left:     []int64{},
		},
		{
			name: "unconsumed interactions are in recorded order",
			recorded: []cassette.Interaction{
				rec(1, "llm", "c"), rec(2, "llm", "a"), rec(3, "llm", "b"), rec(4, "llm", "a"), rec(5, "llm", "c"),
			},
			requests: []string{"a", "c"},
			served:   []int64{2, 1},
			left:     []int64{3, 4, 5},
		},
		{
			name:     "empty cassette",
			requests: []string{"a"},
			served:   []int64{0},
			left:     []int64{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := NewQueues(tt.recorded, storedFingerprint)
			served := []int64{}
			for _, fp := range tt.requests {
				in, ok := q.Next(fp)
				if !ok {
					served = append(served, 0)
					continue
				}
				if *in.Fingerprint != fp {
					t.Fatalf("Next(%q) served interaction %d with fingerprint %q", fp, in.ID, *in.Fingerprint)
				}
				served = append(served, in.ID)
			}
			if !slices.Equal(served, tt.served) {
				t.Errorf("served %v, want %v", served, tt.served)
			}
			if got := ids(q.Unconsumed()); !slices.Equal(got, tt.left) {
				t.Errorf("Unconsumed() = %v, want %v", got, tt.left)
			}
		})
	}
}

func TestQueuesNextUnconsumed(t *testing.T) {
	recorded := []cassette.Interaction{
		rec(1, "llm", "a"),
		withError(rec(2, "llm", "b"), cassette.RecordingIncomplete),
		rec(3, "llm", "c"),
		rec(4, "llm", "d"),
	}
	isLLM := func(in cassette.Interaction) bool { return in.Channel == "llm" }
	notC := func(in cassette.Interaction) bool { return *in.Fingerprint != "c" }

	q := NewQueues(recorded, storedFingerprint)
	steps := []struct {
		consume string // fingerprint consumed before the lookup, if any
		keep    func(cassette.Interaction) bool
		want    int64 // 0 when nothing is left
	}{
		{keep: isLLM, want: 1},
		{keep: isLLM, want: 1}, // looking does not consume
		{consume: "a", keep: isLLM, want: 3},
		{keep: notC, want: 4},
		{consume: "d", keep: notC, want: 0},
		{consume: "c", keep: isLLM, want: 0},
	}
	for i, s := range steps {
		if s.consume != "" {
			if _, ok := q.Next(s.consume); !ok {
				t.Fatalf("step %d: Next(%q) missed", i, s.consume)
			}
		}
		in, ok := q.NextUnconsumed(s.keep)
		if !ok {
			in.ID = 0
		}
		if in.ID != s.want {
			t.Errorf("step %d: NextUnconsumed() = %d, want %d", i, in.ID, s.want)
		}
	}
}

func TestQueuesConcurrentUse(t *testing.T) {
	const fingerprints, perFingerprint = 8, 50
	var recorded []cassette.Interaction
	var id int64
	for range perFingerprint {
		for f := range fingerprints {
			id++
			recorded = append(recorded, rec(id, "llm", string(rune('a'+f))))
		}
	}
	q := NewQueues(recorded, storedFingerprint)

	// Each fingerprint gets one more request than was recorded, from several
	// goroutines at once, while others read the diagnostics.
	served := make([][]int64, fingerprints)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for f := range fingerprints {
		fp := string(rune('a' + f))
		for range perFingerprint + 1 {
			wg.Go(func() {
				if in, ok := q.Next(fp); ok {
					mu.Lock()
					served[f] = append(served[f], in.ID)
					mu.Unlock()
				}
			})
		}
		wg.Go(func() {
			q.Unconsumed()
			q.NextUnconsumed(func(cassette.Interaction) bool { return true })
		})
	}
	wg.Wait()

	for f, got := range served {
		// Every recorded interaction of the fingerprint is served exactly once.
		slices.Sort(got)
		want := []int64{}
		for i := range perFingerprint {
			want = append(want, int64(i*fingerprints+f+1))
		}
		if !slices.Equal(got, want) {
			t.Errorf("fingerprint %c served %v, want %v", 'a'+f, got, want)
		}
	}
	if left := q.Unconsumed(); len(left) != 0 {
		t.Errorf("Unconsumed() = %v, want none", ids(left))
	}
}
