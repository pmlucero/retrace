package anthropic

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
)

// A conversation grows turn by turn, so each key after the first restores a
// cached prefix. Every key must still equal the reference, whatever the cache
// holds, including a cache too small to keep the prefixes.
func TestConversationKeyCachedGrowingHistory(t *testing.T) {
	var valid []string
	for _, s := range conversationKeySeeds {
		if json.Valid([]byte(s)) {
			valid = append(valid, s)
		}
	}
	for _, capacity := range []int{1, 2, 32} {
		cache := newPrefixCache(capacity)
		for n := 1; n <= len(valid); n++ {
			msgs := rawMessages(valid[:n]...)
			got, gotOK := conversationKeyCached(cache, msgs)
			want, wantOK := referenceConversationKey(msgs)
			if got != want || gotOK != wantOK {
				t.Fatalf("capacity %d, %d messages: got %s %v, want %s %v", capacity, n, got, gotOK, want, wantOK)
			}
		}
	}
}

// The length of each message is part of the prefix hash, and order matters,
// so different histories never share a cached state.
func TestConversationKeyCachedPrefixIdentity(t *testing.T) {
	cache := newPrefixCache(8)
	histories := [][]json.RawMessage{
		rawMessages(`{"a":1}`, `{"b":2}`),
		rawMessages(`{"a":1}`, `{"b":3}`),
		rawMessages(`{"b":2}`, `{"a":1}`),
		rawMessages(`{"a":1}`),
		rawMessages(`{"a":1}`, `{"b":2}`, `{"c":3}`),
	}
	for round := 0; round < 2; round++ {
		for _, msgs := range histories {
			got, gotOK := conversationKeyCached(cache, msgs)
			want, wantOK := referenceConversationKey(msgs)
			if got != want || gotOK != wantOK {
				t.Fatalf("%s: got %s %v, want %s %v", msgs, got, gotOK, want, wantOK)
			}
		}
	}
}

func TestConversationKeyCachedInvalidAfterValidPrefix(t *testing.T) {
	cache := newPrefixCache(8)
	conversationKeyCached(cache, rawMessages(`{"a":1}`))
	got, gotOK := conversationKeyCached(cache, rawMessages(`{"a":1}`, `{bad`))
	if gotOK || got != "" {
		t.Fatalf("got %q %v, want a failure", got, gotOK)
	}
	if n := len(cache.m); n != 1 {
		t.Errorf("a failed key stored a prefix: %d entries", n)
	}
}

func TestPrefixCacheBounded(t *testing.T) {
	cache := newPrefixCache(4)
	for i := 0; i < 100; i++ {
		conversationKeyCached(cache, rawMessages(fmt.Sprintf(`{"i":%d}`, i)))
		if len(cache.m) > 4 || len(cache.order) > 4 {
			t.Fatalf("after %d keys: %d entries, %d in order", i+1, len(cache.m), len(cache.order))
		}
	}
	if len(cache.m) != 4 {
		t.Errorf("%d entries, want 4", len(cache.m))
	}
}

func TestConversationKeyCachedConcurrent(t *testing.T) {
	cache := newPrefixCache(4)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var msgs []json.RawMessage
			for i := 0; i < 40; i++ {
				msgs = append(msgs, json.RawMessage(fmt.Sprintf(`{"g":%d,"i":%d}`, g%3, i)))
				got, gotOK := conversationKeyCached(cache, msgs)
				want, wantOK := referenceConversationKey(msgs)
				if got != want || gotOK != wantOK {
					t.Errorf("goroutine %d, %d messages: got %s %v, want %s %v", g, len(msgs), got, gotOK, want, wantOK)
					return
				}
			}
		}()
	}
	wg.Wait()
}
