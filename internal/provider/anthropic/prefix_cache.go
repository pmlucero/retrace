package anthropic

import "sync"

// prefixCacheSize is how many conversation prefixes prefixStates remembers.
// Each entry is a SHA-256 state of about 100 bytes, so memory is fixed.
const prefixCacheSize = 32

// prefixStates is shared by every conversationKey call in the process.
var prefixStates = newPrefixCache(prefixCacheSize)

// prefixCache maps the hash of a message prefix to the marshaled state of the
// conversation hash after that prefix. It holds a fixed number of entries and
// drops the oldest first. It is safe for concurrent use.
type prefixCache struct {
	mu    sync.Mutex
	m     map[[32]byte][]byte
	order [][32]byte // insertion order, a ring of at most cap entries
	next  int        // ring slot to overwrite once full
	cap   int
}

func newPrefixCache(capacity int) *prefixCache {
	return &prefixCache{m: make(map[[32]byte][]byte, capacity), cap: capacity}
}

func (c *prefixCache) get(key [32]byte) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.m[key]
	return s, ok
}

func (c *prefixCache) put(key [32]byte, state []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[key]; ok {
		c.m[key] = state
		return
	}
	if len(c.order) < c.cap {
		c.order = append(c.order, key)
	} else {
		delete(c.m, c.order[c.next])
		c.order[c.next] = key
		c.next = (c.next + 1) % c.cap
	}
	c.m[key] = state
}
