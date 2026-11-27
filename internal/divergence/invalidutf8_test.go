package divergence

import (
	"encoding/json"
	"testing"
)

// AC-15: the diff entries of invalid UTF-8 are the same bytes on every Go
// version (issue #214): a raw U+FFFD, never the escape � that
// encoding/json writes up to Go 1.26.
func TestCompareInvalidUTF8Bytes(t *testing.T) {
	tests := []struct {
		name               string
		expected, received string
		want               string // the entries, exactly as stored
	}{
		{"body that is not JSON", "a\xffb", "ab",
			"[{\"path\":\"$\",\"expected\":\"a\xef\xbf\xbdb\",\"received\":\"ab\"}]"},
		{"string inside JSON", "{\"k\":\"a\xffb\"}", `{"k":"ab"}`,
			"[{\"path\":\"$.k\",\"expected\":\"a\xef\xbf\xbdb\",\"received\":\"ab\"}]"},
		{"key inside JSON", "{\"a\xffb\":1}", `{}`,
			"[{\"path\":\"$['a\xef\xbf\xbdb']\",\"expected\":1}]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := Compare([]byte(tc.expected), []byte(tc.received))
			got, err := json.Marshal(d.Entries)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("entries = %q, want %q", got, tc.want)
			}
		})
	}
}
