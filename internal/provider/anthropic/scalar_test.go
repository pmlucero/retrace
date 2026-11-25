package anthropic

import (
	"encoding/json"
	"testing"
)

func referenceStringValue(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

func referenceBlockIndex(raw json.RawMessage) *int {
	if n := 0; json.Unmarshal(raw, &n) == nil {
		return &n
	}
	return nil
}

var scalarSeeds = []string{
	``, `null`, `0`, `-0`, `1`, `7`, `-1`, `1.0`, `1e0`, `1E2`, `1.5`, `65535`, `65536`, `9223372036854775807`,
	`9223372036854775808`, `99999999999999999999`, `"1"`, `""`, `"text"`, `"a\nb"`, `"a\"b"`, `"é"`,
	"\"café  \"", "\"bad \xff\"", `"\ud800"`, `true`, `[]`, `{}`, `"`,
}

func checkScalars(t *testing.T, raw json.RawMessage) {
	t.Helper()
	if got, want := stringValue(raw), referenceStringValue(raw); got != want {
		t.Errorf("stringValue(%q) = %q, want %q", raw, got, want)
	}
	got, want := blockIndex(raw), referenceBlockIndex(raw)
	if (got == nil) != (want == nil) || got != nil && *got != *want {
		t.Errorf("blockIndex(%q) = %v, want %v", raw, got, want)
	}
}

// stringValue and blockIndex have fast paths that must agree with decoding
// through encoding/json.
func TestScalarFastPathsMatchEncodingJSON(t *testing.T) {
	for _, s := range scalarSeeds {
		checkScalars(t, json.RawMessage(s))
	}
}

func FuzzScalarFastPaths(f *testing.F) {
	for _, s := range scalarSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if json.Valid(data) { // RawMessage fields only ever hold valid JSON
			checkScalars(t, data)
		}
	})
}
