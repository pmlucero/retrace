package divergence

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// AC-15: a cassette miss carries a JSON-path diff (spec §5 step 4).
func TestCompare(t *testing.T) {
	tests := []struct {
		name               string
		expected, received string
		want               string // the entries as JSON
	}{
		{"equal", `{"a":[1,{"b":"x"}]}`, `{"a":[1,{"b":"x"}]}`, `[]`},
		{"changed scalar", `{"params":{"arguments":{"status":"open"}}}`, `{"params":{"arguments":{"status":"closed"}}}`,
			`[{"path":"$.params.arguments.status","expected":"open","received":"closed"}]`},
		{"member only recorded", `{"a":1,"status":"open"}`, `{"a":1}`,
			`[{"path":"$.status","expected":"open"}]`},
		{"member only received", `{"a":1}`, `{"a":1,"z":true}`,
			`[{"path":"$.z","received":true}]`},
		{"recorded members first, then received-only ones", `{"b":1,"c":2}`, `{"a":0,"c":3}`,
			`[{"path":"$.b","expected":1},{"path":"$.c","expected":2,"received":3},{"path":"$.a","received":0}]`},
		{"different types", `{"a":{"b":1}}`, `{"a":[1]}`,
			`[{"path":"$.a","expected":{"b":1},"received":[1]}]`},
		{"bool", `{"a":true,"b":false}`, `{"a":true,"b":true}`,
			`[{"path":"$.b","expected":false,"received":true}]`},
		{"duplicate keys: the first one is compared", `{"a":1,"a":2}`, `{"a":1,"a":3}`, `[]`},
		{"null against a value", `{"a":null}`, `{"a":0}`,
			`[{"path":"$.a","expected":null,"received":0}]`},
		{"number text", `{"a":1.0}`, `{"a":1}`,
			`[{"path":"$.a","expected":1.0,"received":1}]`},
		{"array gains an element", `{"messages":[{"r":"user"}]}`, `{"messages":[{"r":"user"},{"r":"assistant"}]}`,
			`[{"path":"$.messages[1]","received":{"r":"assistant"}}]`},
		{"array loses an element", `[1,2,3]`, `[1]`,
			`[{"path":"$[1]","expected":2},{"path":"$[2]","expected":3}]`},
		{"top-level type", `{"kind":"request"}`, `[{"kind":"request"}]`,
			`[{"path":"$","expected":{"kind":"request"},"received":[{"kind":"request"}]}]`},
		{"bracketed names", `{"a b":1,"it's":1,"":1,"é":1}`, `{"a b":2,"it's":2,"":2,"é":2}`,
			`[{"path":"$['a b']","expected":1,"received":2},{"path":"$['it\\'s']","expected":1,"received":2},` +
				`{"path":"$['']","expected":1,"received":2},{"path":"$['é']","expected":1,"received":2}]`},
		{"not JSON", "not json", `{"a":1}`,
			`[{"path":"$","expected":"not json","received":"{\"a\":1}"}]`},
		{"not JSON, equal", "same", "same", `[]`},
		{"invalid UTF-8", "a\xffb", "ab",
			`[{"path":"$","expected":"a�b","received":"ab"}]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := Compare([]byte(tt.expected), []byte(tt.received))
			got, err := json.Marshal(d.Entries)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("Compare entries =\n%s\nwant\n%s", got, tt.want)
			}
			if d.Truncated {
				t.Error("Truncated = true, want false")
			}
		})
	}
}

func TestCompareCapsEntries(t *testing.T) {
	build := func(n int, v string) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = fmt.Sprintf(`"k%03d":%s`, i, v)
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	for _, n := range []int{MaxEntries, MaxEntries + 1} {
		d := Compare([]byte(build(n, "1")), []byte(build(n, "2")))
		if len(d.Entries) != MaxEntries {
			t.Errorf("%d differences: %d entries, want %d", n, len(d.Entries), MaxEntries)
		}
		if want := n > MaxEntries; d.Truncated != want {
			t.Errorf("%d differences: Truncated = %v, want %v", n, d.Truncated, want)
		}
		if d.Entries[0].Path != "$.k000" || d.Entries[MaxEntries-1].Path != fmt.Sprintf("$.k%03d", MaxEntries-1) {
			t.Errorf("%d differences: entries from %s to %s, want the first %d in order",
				n, d.Entries[0].Path, d.Entries[len(d.Entries)-1].Path, MaxEntries)
		}
	}
}

// A large differing array stops being walked once the cap is reached.
func TestCompareCapsNestedEntries(t *testing.T) {
	elems := func(n int, v string) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = `{"a":` + v + `}`
		}
		return "[" + strings.Join(parts, ",") + "]"
	}
	d := Compare([]byte(elems(60, "1")), []byte(elems(60, "2")))
	if len(d.Entries) != MaxEntries || !d.Truncated {
		t.Fatalf("%d entries, Truncated %v; want %d and true", len(d.Entries), d.Truncated, MaxEntries)
	}
	if last := d.Entries[MaxEntries-1].Path; last != fmt.Sprintf("$[%d].a", MaxEntries-1) {
		t.Errorf("last path %s, want $[%d].a", last, MaxEntries-1)
	}
}

func TestCompareElidesLargeValues(t *testing.T) {
	// The compact JSON of a string is its length plus two quotes.
	fits := strings.Repeat("x", MaxValueBytes-2)
	long := strings.Repeat("y", MaxValueBytes-1)
	d := Compare([]byte(`{"a":"`+fits+`"}`), []byte(`{"a":"`+long+`"}`))
	if got := string(d.Entries[0].Expected); got != `"`+fits+`"` {
		t.Errorf("expected = %.40s…, want the value as it is", got)
	}
	want := fmt.Sprintf(`"[retrace: %d bytes elided]"`, MaxValueBytes+1)
	if got := string(d.Entries[0].Received); got != want {
		t.Errorf("received = %.40s…, want %s", got, want)
	}
	if !d.Truncated {
		t.Error("Truncated = false, want true after an elided value")
	}
}
