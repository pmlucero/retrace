package redact

import (
	"fmt"
	"strconv"
	"strings"
)

// Path is a parsed body redaction rule: the JSONPath subset of spec §3.5.
type Path struct {
	raw   string
	steps []step
}

type stepKind int

const (
	stepChild     stepKind = iota // .name or ['name']
	stepIndex                     // [n]
	stepWildcard                  // .* or [*]
	stepRecursive                 // ..name
)

type step struct {
	kind  stepKind
	name  string
	index int
}

// String returns the rule as written.
func (p Path) String() string { return p.raw }

// ParsePath parses a body redaction rule.
func ParsePath(raw string) (Path, error) {
	p := Path{raw: raw}
	rest, ok := strings.CutPrefix(raw, "$")
	if !ok {
		return Path{}, fmt.Errorf("JSONPath %q must start with $", raw)
	}
	for rest != "" {
		var (
			s   step
			err error
		)
		switch {
		case strings.HasPrefix(rest, ".."):
			s.kind = stepRecursive
			if s.name, rest, err = parseName(rest[2:]); err != nil {
				return Path{}, fmt.Errorf("JSONPath %q: after '..': %w", raw, err)
			}
		case strings.HasPrefix(rest, ".*"):
			s.kind, rest = stepWildcard, rest[2:]
		case strings.HasPrefix(rest, "."):
			s.kind = stepChild
			if s.name, rest, err = parseName(rest[1:]); err != nil {
				return Path{}, fmt.Errorf("JSONPath %q: after '.': %w", raw, err)
			}
		case strings.HasPrefix(rest, "["):
			if s, rest, err = parseBracket(rest); err != nil {
				return Path{}, fmt.Errorf("JSONPath %q: %w", raw, err)
			}
		default:
			return Path{}, fmt.Errorf("JSONPath %q: unexpected %q", raw, rest)
		}
		p.steps = append(p.steps, s)
	}
	if len(p.steps) == 0 {
		return Path{}, fmt.Errorf("JSONPath %q selects the whole body; name a field", raw)
	}
	return p, nil
}

// parseName reads a member name: letters, digits, '_' and '-', or ['quoted']
// after "..".
func parseName(s string) (name, rest string, err error) {
	if strings.HasPrefix(s, "['") {
		st, rest, err := parseBracket(s)
		if err != nil {
			return "", "", err
		}
		return st.name, rest, nil
	}
	end := 0
	for end < len(s) && isNameByte(s[end]) {
		end++
	}
	if end == 0 {
		return "", "", fmt.Errorf("missing member name (write other names as ['name'])")
	}
	return s[:end], s[end:], nil
}

// isNameByte reports whether c may appear in a dotted member name.
func isNameByte(c byte) bool {
	return c == '_' || c == '-' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// parseBracket reads ['name'], [n] or [*].
func parseBracket(s string) (step, string, error) {
	end := strings.Index(s, "]")
	if end < 0 {
		return step{}, "", fmt.Errorf("unclosed '['")
	}
	inner, rest := s[1:end], s[end+1:]
	switch {
	case inner == "*":
		return step{kind: stepWildcard}, rest, nil
	case len(inner) >= 2 && inner[0] == '\'' && inner[len(inner)-1] == '\'':
		name := inner[1 : len(inner)-1]
		if name == "" || strings.ContainsRune(name, '\'') {
			return step{}, "", fmt.Errorf("invalid quoted name [%s]", inner)
		}
		return step{kind: stepChild, name: name}, rest, nil
	default:
		n, err := strconv.Atoi(inner)
		if err != nil || n < 0 {
			return step{}, "", fmt.Errorf("[%s] is not a name, an index or *", inner)
		}
		return step{kind: stepIndex, index: n}, rest, nil
	}
}

// apply replaces every value that p selects in root with mask(value), and
// returns how many it replaced.
func (p Path) apply(root *jsonValue) int {
	return walk(root, p.steps)
}

func walk(v *jsonValue, steps []step) int {
	if len(steps) == 0 {
		*v = masked(*v)
		return 1
	}
	s, rest := steps[0], steps[1:]
	n := 0
	switch s.kind {
	case stepChild:
		if obj, ok := (*v).(*jsonObject); ok {
			for i := range obj.members {
				if obj.members[i].key == s.name {
					n += walk(&obj.members[i].value, rest)
				}
			}
		}
	case stepIndex:
		if arr, ok := (*v).(jsonArray); ok && s.index < len(arr) {
			n += walk(&arr[s.index], rest)
		}
	case stepWildcard:
		forEachChild(*v, func(c *jsonValue) { n += walk(c, rest) })
	case stepRecursive:
		// The name may match here and at any depth below. A replaced value is
		// a string, so the descent never revisits it.
		if obj, ok := (*v).(*jsonObject); ok {
			for i := range obj.members {
				if obj.members[i].key == s.name {
					n += walk(&obj.members[i].value, rest)
				}
			}
		}
		forEachChild(*v, func(c *jsonValue) { n += walk(c, steps) })
	}
	return n
}

func forEachChild(v jsonValue, fn func(*jsonValue)) {
	switch t := v.(type) {
	case *jsonObject:
		for i := range t.members {
			fn(&t.members[i].value)
		}
	case jsonArray:
		for i := range t {
			fn(&t[i])
		}
	}
}
