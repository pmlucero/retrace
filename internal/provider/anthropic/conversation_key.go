package anthropic

import (
	"bytes"
	"crypto/sha256"
	"encoding"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"slices"
	"unicode/utf8"
)

// conversationKey hashes the canonical JSON of messages, so the same
// conversation gets the same key however its JSON was formatted.
// ok is false when a message is not valid JSON.
func conversationKey(messages []json.RawMessage) (key string, ok bool) {
	return conversationKeyCached(prefixStates, messages)
}

// conversationKeyCached is conversationKey with an explicit cache, which may
// be nil. A turn repeats the history of the previous one, so the hash state
// after a stored prefix is restored instead of canonicalizing it again. The
// prefix is identified by the SHA-256 of its raw messages, so a hit implies
// the same canonical bytes and the key never depends on the cache.
func conversationKeyCached(cache *prefixCache, messages []json.RawMessage) (key string, ok bool) {
	h := sha256.New()
	start := 0
	var last [sha256.Size]byte
	if cache != nil && len(messages) > 0 {
		prefixes := rawPrefixHashes(messages)
		last = prefixes[len(prefixes)-1]
		for i := len(prefixes) - 1; i >= 0; i-- {
			if state, hit := cache.get(prefixes[i]); hit {
				// crypto/sha256 digests implement BinaryUnmarshaler and BinaryMarshaler.
				if h.(encoding.BinaryUnmarshaler).UnmarshalBinary(state) == nil {
					start = i + 1
				} else {
					h.Reset()
				}
				break
			}
		}
	}
	var c canonicalizer
	for _, m := range messages[start:] {
		out, good := c.canonical(m)
		if !good {
			// Anything the scanner declines, such as malformed JSON, goes
			// through encoding/json, which defines the form.
			var err error
			if out, err = stdCanonicalJSON(m); err != nil {
				return "", false
			}
		}
		h.Write(out)
		h.Write([]byte{'\n'})
	}
	if cache != nil && start < len(messages) {
		if state, err := h.(encoding.BinaryMarshaler).MarshalBinary(); err == nil {
			cache.put(last, state)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

// rawPrefixHashes returns, for each i, the SHA-256 of the length-prefixed
// raw bytes of messages[:i+1].
func rawPrefixHashes(messages []json.RawMessage) [][sha256.Size]byte {
	h := sha256.New()
	out := make([][sha256.Size]byte, len(messages))
	var n [8]byte
	for i, m := range messages {
		binary.BigEndian.PutUint64(n[:], uint64(len(m)))
		h.Write(n[:])
		h.Write(m)
		h.Sum(out[i][:0])
	}
	return out
}

// stdCanonicalJSON re-encodes raw with object keys sorted and no
// insignificant whitespace. Numbers keep their text.
//
// Its output defines the canonical form: conversation keys are persisted in
// cassettes, so every byte of it must stay as it is, including the escaping
// of <, > & and U+2028/U+2029, and the last value of a duplicate key winning.
func stdCanonicalJSON(raw json.RawMessage) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(v) // encoding/json sorts map keys
}

// maxScanDepth bounds the recursion of canonicalizer. A deeper value goes
// through encoding/json, which nests up to 10000 levels.
const maxScanDepth = 200

// canonicalizer writes the same bytes as stdCanonicalJSON in a single pass
// over the input, without building a value tree or reflecting. It declines
// (ok false) whatever it does not handle exactly: malformed JSON, trailing
// data, deep nesting, invalid UTF-8 and surrogate escapes. The caller then
// uses stdCanonicalJSON. FuzzConversationKey checks the two for equality.
//
// jsonvalue.EncodeCanonical is not used: its form differs from the persisted
// one (no HTML escaping, and duplicate keys are kept).
type canonicalizer struct {
	src     []byte
	pos     int
	out     []byte
	members []member
	bufs    [][]byte // one assembly buffer per nesting depth
	scratch []byte   // decoded string values, which are copied to out at once, so one buffer serves them all
}

type member struct {
	key        []byte // decoded
	start, end int    // the canonical value in out
}

// canonical returns the canonical form of raw. The result is valid until the
// next call.
func (c *canonicalizer) canonical(raw []byte) (out []byte, ok bool) {
	c.src, c.pos, c.out, c.members = raw, 0, c.out[:0], c.members[:0]
	if !c.value(0) {
		return nil, false
	}
	c.skipSpace()
	if c.pos != len(c.src) {
		return nil, false
	}
	return c.out, true
}

func (c *canonicalizer) skipSpace() {
	for c.pos < len(c.src) {
		switch c.src[c.pos] {
		case ' ', '\t', '\r', '\n':
			c.pos++
		default:
			return
		}
	}
}

func (c *canonicalizer) value(depth int) bool {
	c.skipSpace()
	if c.pos >= len(c.src) {
		return false
	}
	switch ch := c.src[c.pos]; {
	case ch == '{':
		return depth < maxScanDepth && c.object(depth)
	case ch == '[':
		return depth < maxScanDepth && c.array(depth)
	case ch == '"':
		// The decoded value is written to out before scratch is used again.
		s, ok := c.str(true)
		if ok {
			c.out = appendString(c.out, s)
		}
		return ok
	case ch == '-' || ch >= '0' && ch <= '9':
		return c.number()
	default:
		for _, lit := range [...]string{"true", "false", "null"} {
			if bytes.HasPrefix(c.src[c.pos:], []byte(lit)) {
				c.out = append(c.out, lit...)
				c.pos += len(lit)
				return true
			}
		}
		return false
	}
}

func (c *canonicalizer) array(depth int) bool {
	c.pos++ // [
	c.out = append(c.out, '[')
	c.skipSpace()
	if c.pos < len(c.src) && c.src[c.pos] == ']' {
		c.pos++
		c.out = append(c.out, ']')
		return true
	}
	for {
		if !c.value(depth + 1) {
			return false
		}
		c.skipSpace()
		if c.pos >= len(c.src) {
			return false
		}
		switch c.src[c.pos] {
		case ',':
			c.pos++
			c.out = append(c.out, ',')
		case ']':
			c.pos++
			c.out = append(c.out, ']')
			return true
		default:
			return false
		}
	}
}

func (c *canonicalizer) object(depth int) bool {
	c.pos++ // {
	base, objStart := len(c.members), len(c.out)
	c.skipSpace()
	if c.pos < len(c.src) && c.src[c.pos] == '}' {
		c.pos++
		c.out = append(c.out, '{', '}')
		return true
	}
	for {
		c.skipSpace()
		if c.pos >= len(c.src) || c.src[c.pos] != '"' {
			return false
		}
		key, ok := c.str(false) // members keep the key, so it needs its own bytes
		if !ok {
			return false
		}
		c.skipSpace()
		if c.pos >= len(c.src) || c.src[c.pos] != ':' {
			return false
		}
		c.pos++
		start := len(c.out)
		if !c.value(depth + 1) {
			return false
		}
		c.members = append(c.members, member{key, start, len(c.out)})
		c.skipSpace()
		if c.pos >= len(c.src) {
			return false
		}
		if c.src[c.pos] == ',' {
			c.pos++
			continue
		}
		if c.src[c.pos] != '}' {
			return false
		}
		c.pos++
		break
	}
	// The values are in out in source order. Reassemble them sorted by key,
	// keeping the last of a duplicated key, as decoding into a map does.
	ms := c.members[base:]
	slices.SortStableFunc(ms, func(a, b member) int { return bytes.Compare(a.key, b.key) })
	for len(c.bufs) <= depth {
		c.bufs = append(c.bufs, nil)
	}
	res := c.bufs[depth][:0]
	res = append(res, '{')
	for i, m := range ms {
		if i+1 < len(ms) && bytes.Equal(ms[i+1].key, m.key) {
			continue
		}
		if len(res) > 1 {
			res = append(res, ',')
		}
		res = appendString(res, m.key)
		res = append(res, ':')
		res = append(res, c.out[m.start:m.end]...)
	}
	res = append(res, '}')
	c.bufs[depth] = res
	c.out = append(c.out[:objStart], res...)
	c.members = c.members[:base]
	return true
}

// number copies a number as written, if it follows the JSON grammar.
func (c *canonicalizer) number() bool {
	s, i := c.src, c.pos
	digits := func() int {
		n := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
			n++
		}
		return n
	}
	if s[i] == '-' {
		i++
	}
	switch {
	case i < len(s) && s[i] == '0':
		i++
	case digits() == 0:
		return false
	}
	if i < len(s) && s[i] == '.' {
		i++
		if digits() == 0 {
			return false
		}
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		if digits() == 0 {
			return false
		}
	}
	c.out = append(c.out, s[c.pos:i]...)
	c.pos = i
	return true
}

// str reads the string at pos and returns its decoded bytes: a slice of the
// input, or, when the string has escapes, a new slice. With useScratch the
// new slice is the scratch buffer, so the caller must be done with the result
// before the next call that uses it.
func (c *canonicalizer) str(useScratch bool) ([]byte, bool) {
	s := c.src
	i := c.pos + 1
	start := i
	escaped := false
	for ; i < len(s) && s[i] != '"'; i++ {
		switch {
		case s[i] < ' ':
			return nil, false
		case s[i] == '\\':
			escaped = true
			i++ // the escaped byte cannot end the string
		}
	}
	if i >= len(s) {
		return nil, false
	}
	body := s[start:i]
	c.pos = i + 1
	if !utf8.Valid(body) {
		return nil, false
	}
	if !escaped {
		return body, true
	}
	var dec []byte
	if useScratch {
		dec = c.scratch[:0]
		defer func() { c.scratch = dec[:0] }() // keep the grown buffer
	} else {
		dec = make([]byte, 0, len(body))
	}
	for j := 0; j < len(body); j++ {
		if body[j] != '\\' {
			dec = append(dec, body[j])
			continue
		}
		j++
		if j >= len(body) {
			return nil, false
		}
		switch body[j] {
		case '"', '\\', '/':
			dec = append(dec, body[j])
		case 'b':
			dec = append(dec, '\b')
		case 'f':
			dec = append(dec, '\f')
		case 'n':
			dec = append(dec, '\n')
		case 'r':
			dec = append(dec, '\r')
		case 't':
			dec = append(dec, '\t')
		case 'u':
			if j+4 >= len(body) {
				return nil, false
			}
			var r rune
			for _, h := range body[j+1 : j+5] {
				switch {
				case h >= '0' && h <= '9':
					r = r<<4 | rune(h-'0')
				case h >= 'a' && h <= 'f':
					r = r<<4 | rune(h-'a'+10)
				case h >= 'A' && h <= 'F':
					r = r<<4 | rune(h-'A'+10)
				default:
					return nil, false
				}
			}
			if r >= 0xD800 && r < 0xE000 {
				return nil, false // surrogates are left to encoding/json
			}
			dec = utf8.AppendRune(dec, r)
			j += 4
		default:
			return nil, false
		}
	}
	return dec, true
}

const hexDigits = "0123456789abcdef"

// plain marks the bytes that appendString copies as they are.
var plain = func() (t [256]bool) {
	for i := ' '; i < 256; i++ {
		t[i] = i != '"' && i != '\\' && i != '<' && i != '>' && i != '&' && i != 0xE2
	}
	return t
}()

// appendString writes the decoded, valid UTF-8 string s as encoding/json
// does by default: HTML escaping on, U+2028 and U+2029 escaped.
func appendString(b, s []byte) []byte {
	b = append(b, '"')
	start := 0
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if plain[ch] {
			continue
		}
		if ch == 0xE2 {
			// U+2028 and U+2029 are E2 80 A8 and E2 80 A9.
			if i+2 < len(s) && s[i+1] == 0x80 && s[i+2]&^1 == 0xA8 {
				b = append(b, s[start:i]...)
				b = append(b, `\u202`...)
				b = append(b, hexDigits[s[i+2]&0xf])
				i += 2
				start = i + 1
			}
			continue
		}
		b = append(b, s[start:i]...)
		switch ch {
		case '"', '\\':
			b = append(b, '\\', ch)
		case '\b':
			b = append(b, '\\', 'b')
		case '\f':
			b = append(b, '\\', 'f')
		case '\n':
			b = append(b, '\\', 'n')
		case '\r':
			b = append(b, '\\', 'r')
		case '\t':
			b = append(b, '\\', 't')
		default: // other control characters, <, > and &
			b = append(b, '\\', 'u', '0', '0', hexDigits[ch>>4], hexDigits[ch&0xf])
		}
		start = i + 1
	}
	b = append(b, s[start:]...)
	return append(b, '"')
}
