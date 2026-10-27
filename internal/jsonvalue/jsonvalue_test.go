package jsonvalue

import (
	"errors"
	"strings"
	"testing"
)

// The decoder shared by redaction (spec §3.5) and matching (ADR 0007).
func TestDecode(t *testing.T) {
	nest := func(n int) string { return strings.Repeat("[", n) + strings.Repeat("]", n) }
	tests := []struct {
		name, in string
		wantErr  bool
		tooDeep  bool
	}{
		{"object", `{"a":[1,"x",true,null]}`, false, false},
		{"at the depth limit", nest(MaxDepth), false, false},
		{"empty", ``, true, false},
		{"truncated", `{"a":[`, true, false},
		{"trailing data", `{"a":1} {}`, true, false},
		{"not JSON", `a=1`, true, false},
		{"past the depth limit", nest(MaxDepth + 1), true, true},
		{"past encoding/json's limit", nest(20000), true, true},
	}
	for _, tt := range tests {
		_, err := Decode([]byte(tt.in))
		if (err != nil) != tt.wantErr || errors.Is(err, ErrTooDeep) != tt.tooDeep {
			t.Errorf("%s: Decode error = %v, want error %t, ErrTooDeep %t", tt.name, err, tt.wantErr, tt.tooDeep)
		}
	}
}

// Encode keeps member order, duplicates and number text, and writes no
// insignificant whitespace or HTML escapes.
func TestEncodeRoundTrip(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"member order and duplicates", `{"b":1,"a":2,"b":3}`, `{"b":1,"a":2,"b":3}`},
		{"number text", `[1.0,1e2,-0,10E-1]`, `[1.0,1e2,-0,10E-1]`},
		{"whitespace", " {\n\t\"a\" : [ 1 , true , null ] } ", `{"a":[1,true,null]}`},
		{"escapes", `"A\/é<\n"`, `"A/é<\n"`},
	}
	for _, tt := range tests {
		v, err := Decode([]byte(tt.in))
		if err != nil {
			t.Fatalf("%s: Decode: %v", tt.name, err)
		}
		if got := string(Encode(v)); got != tt.want {
			t.Errorf("%s: Encode = %s, want %s", tt.name, got, tt.want)
		}
	}
}
