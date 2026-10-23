package redact

import (
	"fmt"
	"slices"
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

// selection is a value a rule selected, and how deep it is in the body.
type selection struct {
	value *jsonValue
	depth int
}

// selectAll returns every value p selects in root, without changing it.
func (p Path) selectAll(root *jsonValue, out []selection) []selection {
	return walk(root, p.steps, 0, out)
}

func walk(v *jsonValue, steps []step, depth int, out []selection) []selection {
	if len(steps) == 0 {
		return append(out, selection{value: v, depth: depth})
	}
	s, rest := steps[0], steps[1:]
	switch s.kind {
	case stepChild:
		if obj, ok := (*v).(*jsonObject); ok {
			for i := range obj.members {
				if obj.members[i].key == s.name {
					out = walk(&obj.members[i].value, rest, depth+1, out)
				}
			}
		}
	case stepIndex:
		if arr, ok := (*v).(jsonArray); ok && s.index < len(arr) {
			out = walk(&arr[s.index], rest, depth+1, out)
		}
	case stepWildcard:
		forEachChild(*v, func(c *jsonValue) { out = walk(c, rest, depth+1, out) })
	case stepRecursive:
		// The name may match here and at any depth below.
		if obj, ok := (*v).(*jsonObject); ok {
			for i := range obj.members {
				if obj.members[i].key == s.name {
					out = walk(&obj.members[i].value, rest, depth+1, out)
				}
			}
		}
		forEachChild(*v, func(c *jsonValue) { out = walk(c, steps, depth+1, out) })
	}
	return out
}

// maskSelections masks each selected value once, outermost first, and returns
// how many it masked. A value inside one already masked has been cut from the
// body, so masking it changes nothing; the outer marker is hashed over the
// original content whatever the rule order (spec §3.5).
func maskSelections(sel []selection) int {
	slices.SortStableFunc(sel, func(a, b selection) int { return a.depth - b.depth })
	done := map[*jsonValue]bool{}
	for _, s := range sel {
		if !done[s.value] {
			done[s.value] = true
			*s.value = masked(*s.value)
		}
	}
	return len(done)
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
