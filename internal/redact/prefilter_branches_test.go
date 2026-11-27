package redact

import (
	"strings"
	"testing"

	"github.com/pmlucero/retrace/internal/jsonvalue"
)

// The prefilter's defensive branches, called directly. The differential test
// in prefilter_test.go checks the end result; these pin each branch.

func TestPlainName(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   string
		want bool
	}{
		{"ascii", "email", true},
		{"slash", "a/b", true},
		{"non-ASCII", "clé", true},
		{"empty", "", false},
		{"invalid UTF-8", "a\xffb", false},
		{"replacement char", "a�b", false},
		{"control char", "a\x01b", false},
		{"quote", `a"b`, false},
		{"backslash", `a\b`, false},
	} {
		if got := plainName(tt.in); got != tt.want {
			t.Errorf("%s: plainName(%q) = %t, want %t", tt.name, tt.in, got, tt.want)
		}
	}
}

func TestMayEscape(t *testing.T) {
	plain := [][]byte{[]byte("email")}
	slash := [][]byte{[]byte("email"), []byte("a/b")}
	for _, tt := range []struct {
		name string
		data string
		lits [][]byte
		want bool
	}{
		{"no escape", `{"email":1}`, plain, false},
		{"unicode escape", `{"\u0065mail":1}`, plain, true},
		{"slash escape, no slash name", `{"a\/b":1}`, plain, false},
		{"slash escape, slash name", `{"a\/b":1}`, slash, true},
		{"slash name, no slash escape", `{"a/b":1}`, slash, false},
		{"other escapes", `{"a":"\n\t\"\\"}`, slash, false},
	} {
		if got := mayEscape([]byte(tt.data), tt.lits); got != tt.want {
			t.Errorf("%s: mayEscape(%s) = %t, want %t", tt.name, tt.data, got, tt.want)
		}
	}
}

func TestEndOfString(t *testing.T) {
	for _, tt := range []struct {
		name string
		data string
		want int
	}{
		{"plain", `"abc" x`, 4},
		{"empty string", `""`, 1},
		{"escaped quote", `"a\"b" x`, 5},
		{"escaped backslash then quote", `"a\\" x`, 4},
		{"three backslashes then quote", `"a\\\"b" x`, 7},
		{"four backslashes then quote", `"a\\\\" x`, 6},
		{"unclosed", `"abc`, 4},
		{"unclosed after escaped quote", `"abc\"`, 6},
		{"no content", `"`, 1},
	} {
		if got := endOfString([]byte(tt.data), 1); got != tt.want {
			t.Errorf("%s: endOfString(%q, 1) = %d, want %d", tt.name, tt.data, got, tt.want)
		}
	}
}

func TestMayBeTooDeep(t *testing.T) {
	over := jsonvalue.MaxDepth + 1
	for _, tt := range []struct {
		name string
		data string
		want bool
	}{
		{"few brackets", nest(jsonvalue.MaxDepth, "1"), false},
		{"real nesting", nest(over, "1"), true},
		{"opens only inside a string", `["` + strings.Repeat("[", over+5) + `"]`, false},
		{"opens inside an escaped-quote string", `["a\"` + strings.Repeat("[", over+5) + `"]`, false},
		{"unclosed string hides the opens", `["abc` + strings.Repeat("[", over+5), false},
		{"many wide, none deep", "[" + strings.Repeat("[1],", over+5) + "1]", false},
		{"closed nesting then deep", "[" + strings.Repeat("[1],", over) + nest(over, "1") + "]", true},
	} {
		if got := mayBeTooDeep([]byte(tt.data)); got != tt.want {
			t.Errorf("%s: mayBeTooDeep = %t, want %t", tt.name, got, tt.want)
		}
	}
}

func TestCannotMatchEvent(t *testing.T) {
	r := mustRules(t, "$..email")
	few := "event: x\ndata: " + nest(jsonvalue.MaxDepth, "1") + "\n\n"
	many := "event: x\ndata: " + nest(jsonvalue.MaxDepth+1, "1") + "\n\n"
	if !r.cannotMatchEvent([]byte(few)) {
		t.Error("event with MaxDepth brackets and no name: cannotMatchEvent = false, want true")
	}
	if r.cannotMatchEvent([]byte(many)) {
		t.Error("event with more than MaxDepth brackets: cannotMatchEvent = true, want false")
	}
	if r.cannotMatchEvent([]byte("data: {\"email\":1}\n\n")) {
		t.Error("event with the name: cannotMatchEvent = true, want false")
	}
	if withoutPrefilter(r).cannotMatchEvent([]byte(few)) {
		t.Error("prefilter off: cannotMatchEvent = true, want false")
	}
}
