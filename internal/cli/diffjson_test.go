package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/diff"
)

// diffSchemaPath is the committed schema of `diff --format json` (spec §8.4).
var diffSchemaPath = filepath.Join("..", "..", "docs", "schema", "diff.v1.json")

// AC-21: the JSON output of diff validates against the committed schema,
// and its golden file pins what it holds.
func TestDiffJSON(t *testing.T) {
	schema := loadDiffSchema(t)
	cases := append(diffCases(), diffCase{
		// An untrusted cassette's value that is not JSON is a string.
		name: "arguments that are not JSON",
		seed: func(t *testing.T, dir string) {
			seedCheckout(t, dir, 0)
			events := checkoutEvents()
			events[1] = toolUse(2, 1, "search_orders", `not json`)
			seedSession(t, dir, cassette.Session{ID: "bad", Mode: "exact", Status: "recording", CreatedAt: lsT0}, nil, events...)
		},
		args: []string{"checkout", "bad"}, code: ExitDiffFound, golden: "not-json",
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.seed(t, dir)
			r := runDiffCmd(dir, append([]string{"--format", "json"}, tc.args...)...)
			if r.code != tc.code {
				t.Fatalf("exit %d, want %d\n%s", r.code, tc.code, r.stderr)
			}
			out := r.stdout.String()
			checkGolden(t, "diff/"+tc.golden+".json", out)
			doc := decodeJSON(t, out)
			if err := schema.validate(doc); err != nil {
				t.Fatalf("output does not validate against %s: %v\n%s", diffSchemaPath, err, out)
			}
			if got := doc.(map[string]any)["identical"]; got != (tc.code == ExitOK) {
				t.Errorf("identical = %v with exit %d", got, tc.code)
			}
		})
	}
}

// AC-27: the JSON document carries dropped_events for each session.
func TestDiffJSONDroppedEvents(t *testing.T) {
	dir := t.TempDir()
	seedCheckout(t, dir, 3)
	seedSwap(t, dir, "checkout.swap-model.1")
	r := runDiffCmd(dir, "--format", "json", "checkout", "checkout.swap-model.1")
	var doc struct {
		A, B struct {
			DroppedEvents *int `json:"dropped_events"`
		}
	}
	if err := json.Unmarshal([]byte(r.stdout.String()), &doc); err != nil {
		t.Fatal(err)
	}
	if r.code != ExitDiffFound || doc.A.DroppedEvents == nil || *doc.A.DroppedEvents != 3 || doc.B.DroppedEvents == nil || *doc.B.DroppedEvents != 0 {
		t.Errorf("exit %d, dropped_events %v and %v; want %d, 3 and 0", r.code, doc.A.DroppedEvents, doc.B.DroppedEvents, ExitDiffFound)
	}
}

// Spec §8.4: text_differences is always present, and empty under --text.
func TestDiffJSONTextDifferences(t *testing.T) {
	dir := t.TempDir()
	seedCheckout(t, dir, 0)
	seedSwap(t, dir, "checkout.swap-model.1")
	for _, tc := range []struct {
		args     []string
		compared bool
		want     []int
	}{
		{nil, false, []int{1}},
		{[]string{"--text"}, true, []int{}},
	} {
		r := runDiffCmd(dir, append(tc.args, "--format", "json", "checkout", "checkout.swap-model.1")...)
		var doc struct {
			TextCompared    bool  `json:"text_compared"`
			TextDifferences []int `json:"text_differences"`
		}
		if err := json.Unmarshal([]byte(r.stdout.String()), &doc); err != nil {
			t.Fatal(err)
		}
		if doc.TextCompared != tc.compared || doc.TextDifferences == nil || !slices.Equal(doc.TextDifferences, tc.want) {
			t.Errorf("%v: text_compared %v, text_differences %v; want %v and %v", tc.args, doc.TextCompared, doc.TextDifferences, tc.compared, tc.want)
		}
	}
}

// What a cassette cannot be trusted to hold is null, not invented.
func TestDiffJSONUntrustedValues(t *testing.T) {
	unreadable := cassette.Event{Seq: 7, Kind: "divergence", Result: json.RawMessage(`[]`)}
	if got := sessionJSON(diffSide{diverged: &unreadable, divergedStep: 2}).Divergence; got == nil ||
		*got != (diffStopJSON{Step: 2, Seq: 7}) {
		t.Errorf("divergence = %+v, want step 2, seq 7 and no channel or upstream", got)
	}
	str := func(s string) *string { return &s }
	for _, tc := range []struct {
		stop *diff.Stop
		want *diffMarkerJSON
	}{
		{nil, nil},
		{&diff.Stop{StopReason: "end_turn"}, &diffMarkerJSON{StopReason: str("end_turn")}},
		{&diff.Stop{}, &diffMarkerJSON{}},
		{&diff.Stop{Failed: true}, &diffMarkerJSON{Failed: true}},
		{&diff.Stop{Failed: true, Error: "overloaded"}, &diffMarkerJSON{Failed: true, Error: str("overloaded")}},
	} {
		if got := markerJSON(tc.stop); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("markerJSON(%+v) = %+v, want %+v", tc.stop, got, tc.want)
		}
	}
}

// The validator must be able to fail: otherwise TestDiffJSON proves nothing.
func TestDiffSchemaRejects(t *testing.T) {
	schema := loadDiffSchema(t)
	dir := t.TempDir()
	seedCheckout(t, dir, 0)
	seedSwap(t, dir, "checkout.swap-model.1")
	out := runDiffCmd(dir, "--format", "json", "checkout", "checkout.swap-model.1").stdout.String()
	for _, tc := range []struct {
		name   string
		change func(doc map[string]any)
	}{
		{"missing member", func(doc map[string]any) { delete(doc, "text_differences") }},
		{"extra member", func(doc map[string]any) { doc["extra"] = true }},
		{"wrong const", func(doc map[string]any) { doc["format_version"] = 2 }},
		{"wrong type", func(doc map[string]any) { doc["identical"] = "no" }},
		{"not an integer", func(doc map[string]any) { doc["text_differences"] = []any{1.5} }},
		{"below minimum", func(doc map[string]any) { doc["a"].(map[string]any)["steps"] = -1 }},
		{"not in enum", func(doc map[string]any) { doc["first_divergence"].(map[string]any)["kind"] = "x" }},
		{"no oneOf matches", func(doc map[string]any) { doc["identical_steps"] = map[string]any{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := decodeJSON(t, out).(map[string]any)
			tc.change(doc)
			if err := schema.validate(doc); err == nil {
				t.Error("the schema accepts the changed document")
			}
		})
	}
}

func decodeJSON(t *testing.T, s string) any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, s)
	}
	if dec.More() {
		t.Fatalf("output holds more than one JSON value:\n%s", s)
	}
	return v
}

// jsonSchema is a validator for the subset of JSON Schema (draft 2020-12)
// that docs/schema/diff.v1.json uses. It is a test helper, not a general
// validator: loadDiffSchema fails on any keyword it does not implement, so
// the schema cannot grow a keyword that this test would silently ignore.
//
// It is stricter than the schema in one way: it rejects an object member
// that the schema does not declare. The published schema is open, so that
// adding a member stays backward compatible for consumers, but a member
// added to the output must still be added to the schema.
type jsonSchema struct {
	root map[string]any
}

// schemaKeywords are the keywords jsonSchema implements. The annotations
// ($schema, $id, title, description) have no effect on validation.
var schemaKeywords = map[string]bool{
	"$schema": true, "$id": true, "title": true, "description": true, "$defs": true, "$ref": true,
	"type": true, "properties": true, "required": true,
	"items": true, "enum": true, "const": true, "minimum": true, "oneOf": true,
}

func loadDiffSchema(t *testing.T) jsonSchema {
	t.Helper()
	raw, err := os.ReadFile(diffSchemaPath) //nolint:gosec // G304: the committed schema
	if err != nil {
		t.Fatal(err)
	}
	v := decodeJSON(t, string(raw))
	root, ok := v.(map[string]any)
	if !ok {
		t.Fatal("the schema is not an object")
	}
	if root["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
		t.Fatalf("$schema = %v, want draft 2020-12", root["$schema"])
	}
	if err := checkKeywords("#", root); err != nil {
		t.Fatal(err)
	}
	return jsonSchema{root: root}
}

// checkKeywords reports a keyword of schema s, or of a subschema, that
// jsonSchema does not implement.
func checkKeywords(at string, s map[string]any) error {
	for _, k := range slices.Sorted(maps.Keys(s)) {
		if !schemaKeywords[k] {
			return fmt.Errorf("%s: keyword %q is not supported by the test validator; extend it", at, k)
		}
	}
	var subs []string
	children := map[string]any{}
	for _, k := range []string{"properties", "$defs"} {
		if m, ok := s[k].(map[string]any); ok {
			for name, sub := range m {
				p := at + "/" + k + "/" + name
				children[p] = sub
				subs = append(subs, p)
			}
		}
	}
	if sub, ok := s["items"]; ok {
		children[at+"/items"] = sub
		subs = append(subs, at+"/items")
	}
	if list, ok := s["oneOf"].([]any); ok {
		for i, sub := range list {
			p := fmt.Sprintf("%s/oneOf/%d", at, i)
			children[p] = sub
			subs = append(subs, p)
		}
	}
	slices.Sort(subs)
	for _, p := range subs {
		sub, ok := children[p].(map[string]any)
		if !ok {
			return fmt.Errorf("%s: a subschema must be an object", p)
		}
		if err := checkKeywords(p, sub); err != nil {
			return err
		}
	}
	return nil
}

func (js jsonSchema) validate(doc any) error { return js.check("$", js.root, doc) }

func (js jsonSchema) check(at string, s map[string]any, v any) error {
	if ref, ok := s["$ref"].(string); ok {
		name, ok := strings.CutPrefix(ref, "#/$defs/")
		def, found := js.root["$defs"].(map[string]any)[name].(map[string]any)
		if !ok || !found {
			return fmt.Errorf("%s: unsupported $ref %q", at, ref)
		}
		if err := js.check(at, def, v); err != nil {
			return err
		}
	}
	if typ, ok := s["type"]; ok && !matchesType(typ, v) {
		return fmt.Errorf("%s: %v is not of type %v", at, v, typ)
	}
	if c, ok := s["const"]; ok && !jsonEqual(c, v) {
		return fmt.Errorf("%s: %v is not %v", at, v, c)
	}
	if e, ok := s["enum"].([]any); ok && !slices.ContainsFunc(e, func(x any) bool { return jsonEqual(x, v) }) {
		return fmt.Errorf("%s: %v is not one of %v", at, v, e)
	}
	if m, ok := s["minimum"].(json.Number); ok {
		if n, isNum := v.(json.Number); isNum {
			x, _ := n.Float64()
			if lim, _ := m.Float64(); x < lim {
				return fmt.Errorf("%s: %v is below the minimum %v", at, v, m)
			}
		}
	}
	if list, ok := s["oneOf"].([]any); ok {
		matched := 0
		for _, sub := range list {
			if js.check(at, sub.(map[string]any), v) == nil {
				matched++
			}
		}
		if matched != 1 {
			return fmt.Errorf("%s: %v matches %d subschemas of oneOf, want 1", at, v, matched)
		}
	}
	if obj, ok := v.(map[string]any); ok {
		if err := js.checkObject(at, s, obj); err != nil {
			return err
		}
	}
	if arr, ok := v.([]any); ok {
		if items, ok := s["items"].(map[string]any); ok {
			for i, x := range arr {
				if err := js.check(fmt.Sprintf("%s[%d]", at, i), items, x); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (js jsonSchema) checkObject(at string, s map[string]any, obj map[string]any) error {
	props, _ := s["properties"].(map[string]any)
	if req, ok := s["required"].([]any); ok {
		for _, name := range req {
			if _, ok := obj[name.(string)]; !ok {
				return fmt.Errorf("%s: required member %q is missing", at, name)
			}
		}
	}
	if props == nil {
		return nil // any object, such as stored arguments
	}
	for _, name := range slices.Sorted(maps.Keys(obj)) {
		sub, ok := props[name].(map[string]any)
		if !ok {
			return fmt.Errorf("%s: member %q is not declared in the schema", at, name)
		}
		if err := js.check(at+"."+name, sub, obj[name]); err != nil {
			return err
		}
	}
	return nil
}

// matchesType reports whether v is of the JSON Schema type typ: a name or a
// list of names.
func matchesType(typ, v any) bool {
	names, ok := typ.([]any)
	if !ok {
		names = []any{typ}
	}
	for _, name := range names {
		switch name {
		case "null":
			if v == nil {
				return true
			}
		case "boolean":
			if _, ok := v.(bool); ok {
				return true
			}
		case "string":
			if _, ok := v.(string); ok {
				return true
			}
		case "number":
			if _, ok := v.(json.Number); ok {
				return true
			}
		case "integer":
			// Draft 2020-12: a number with a zero fractional part, such
			// as 1.0, is an integer.
			if n, ok := v.(json.Number); ok {
				if f, err := n.Float64(); err == nil && f == float64(int64(f)) {
					return true
				}
			}
		case "array":
			if _, ok := v.([]any); ok {
				return true
			}
		case "object":
			if _, ok := v.(map[string]any); ok {
				return true
			}
		}
	}
	return false
}

// jsonEqual compares two decoded JSON values, numbers by value.
func jsonEqual(x, y any) bool {
	xn, xok := x.(json.Number)
	yn, yok := y.(json.Number)
	if xok && yok {
		a, _ := xn.Float64()
		b, _ := yn.Float64()
		return a == b
	}
	if xok || yok {
		return false
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return bytes.Equal(xb, yb)
}

// The validator honors the draft's integer rule and numeric equality, which
// the schema relies on for format_version and the counts.
func TestJSONSchemaNumbers(t *testing.T) {
	for _, tc := range []struct {
		typ  any
		v    any
		want bool
	}{
		{"integer", json.Number("1"), true},
		{"integer", json.Number("1.0"), true},
		{"integer", json.Number("1.5"), false},
		{[]any{"integer", "null"}, nil, true},
		{"number", "1", false},
	} {
		if got := matchesType(tc.typ, tc.v); got != tc.want {
			t.Errorf("matchesType(%v, %v) = %v, want %v", tc.typ, tc.v, got, tc.want)
		}
	}
	if !jsonEqual(json.Number("1"), json.Number("1.0")) || jsonEqual(json.Number("1"), "1") {
		t.Error("jsonEqual does not compare numbers by value")
	}
}
