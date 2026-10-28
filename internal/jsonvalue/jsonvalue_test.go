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
		{"escapes", `"\u0041\/\u00e9<\n"`, `"A/é<\n"`},
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

// ADR 0007 §1: canonical JSON sorts keys and keeps number text.
func TestEncodeCanonical(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"sorted keys", `{"b":1,"a":2}`, `{"a":2,"b":1}`},
		{"nested objects in arrays", `[{"z":{"y":1,"x":2}},{"b":0,"a":0}]`, `[{"z":{"x":2,"y":1}},{"a":0,"b":0}]`},
		{"byte order", `{"b":1,"B":2,"é":3,"a":4}`, `{"B":2,"a":4,"b":1,"é":3}`},
		{"duplicates keep their order", `{"k":2,"a":0,"k":1}`, `{"a":0,"k":2,"k":1}`},
		{"number text", `{"n":[1.0,1e2,-0,10E-1]}`, `{"n":[1.0,1e2,-0,10E-1]}`},
		{"whitespace", " {\n\t\"a\" : [ 1 , true , null ] } ", `{"a":[1,true,null]}`},
		{"escapes", `"\u0041\/\u00e9<\n"`, `"A/é<\n"`},
		{"scalar", `"x"`, `"x"`},
	}
	for _, tt := range tests {
		v, err := Decode([]byte(tt.in))
		if err != nil {
			t.Fatalf("%s: Decode: %v", tt.name, err)
		}
		if got := string(EncodeCanonical(v)); got != tt.want {
			t.Errorf("%s: EncodeCanonical = %s, want %s", tt.name, got, tt.want)
		}
	}
}

// Canonical encoding must not reorder the value it is given: redaction
// encodes the same value in member order.
func TestEncodeCanonicalKeepsTheValue(t *testing.T) {
	v, err := Decode([]byte(`{"b":{"d":1,"c":2},"a":3}`))
	if err != nil {
		t.Fatal(err)
	}
	EncodeCanonical(v)
	if got, want := string(Encode(v)), `{"b":{"d":1,"c":2},"a":3}`; got != want {
		t.Errorf("Encode after EncodeCanonical = %s, want %s", got, want)
	}
}
