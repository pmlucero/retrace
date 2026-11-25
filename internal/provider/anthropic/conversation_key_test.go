package anthropic

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

// referenceConversationKey is conversationKey as of v0.1.0, whose output is
// persisted in cassettes. conversationKey must match it byte for byte.
func referenceConversationKey(messages []json.RawMessage) (string, bool) {
	h := sha256.New()
	for _, m := range messages {
		dec := json.NewDecoder(bytes.NewReader(m))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			return "", false
		}
		c, err := json.Marshal(v)
		if err != nil {
			return "", false
		}
		h.Write(c)
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

// conversationKeySeeds are messages that make the canonical form differ
// easily: HTML characters, U+2028/U+2029, invalid UTF-8, surrogate escapes,
// big and exponent numbers, duplicate keys, nesting, non-ASCII keys and
// input that is not valid JSON.
var conversationKeySeeds = []string{
	`{"role":"user","content":"hi"}`,
	"{ \"b\" : 1 ,\n\t\"a\" : [ 1 , 2 ] }",
	`{"content":"<b>a & b</b> > c"}`,
	`{"content":"x\u003cy\u0026z\u003e \u2028 \u2029 \u007f"}`,
	"{\"content\":\"raw \u2028 and \u2029 and \u00e9 and \U0001F600\"}",
	"{\"content\":\"bad \xf0\x9f \xff \xc3 end\"}",
	`{"content":"\ud83d\ude00 \ud83d \ude00 \udfff"}`,
	`{"content":"\u0000\u001f\b\f\n\r\t\"\\\/"}`,
	`{"content":"\u00e9\u00E9\u0041"}`,
	`{"n":[1,1.0,1.50e3,-0,-0.0,1E+2,12345678901234567890123,1e400,0.1e-7]}`,
	`{"a":1,"a":2,"b":{"x":1,"x":[1],"y":1},"a":{"z":0,"z":1}}`,
	`{"b":1,"a":{"d":1,"c":[{"f":1,"e":2},[{"h":1,"g":2}]]}}`,
	`{"é":1,"e":2,"z":3,"\u00e9":4,"日本":5,"\u65e5\u672c":6}`,
	`{"a\"b":1,"a\\b":2,"a\nb":3,"a<b":4}`,
	`{"":1,"\u0000":2}`,
	`[]`, `{}`, `[[],{},[{}]]`, `"s"`, `1`, `-1.5e-3`, `true`, `false`, `null`,
	`{"a":true,"b":false,"c":null}`,
	`{"a":1} trailing`, `{"a":1}{"b":2}`, `  {"a":1}  `,
	``, `{`, `{"a"`, `{"a":}`, `{"a":1,}`, `[1,]`, `[1 2]`, `{"a" 1}`, `{a:1}`,
	`01`, `1.`, `.5`, `+1`, `1e`, `-`, `tru`, `nul`, `"abc`, `"a\x"`, `"a\u12"`, `"a\u12g4"`,
	"\"tab\there\"", "{\"a\":\"line\nbreak\"}",
}

func rawMessages(ss ...string) []json.RawMessage {
	out := make([]json.RawMessage, len(ss))
	for i, s := range ss {
		out[i] = json.RawMessage(s)
	}
	return out
}

func TestConversationKeyMatchesReference(t *testing.T) {
	for _, seed := range conversationKeySeeds {
		got, gotOK := conversationKey(rawMessages(seed))
		want, wantOK := referenceConversationKey(rawMessages(seed))
		if got != want || gotOK != wantOK {
			t.Errorf("%q: got %s %v, want %s %v", seed, got, gotOK, want, wantOK)
		}
	}
	// Several messages in a row share one canonicalizer.
	var valid []string
	for _, s := range conversationKeySeeds {
		if json.Valid([]byte(s)) {
			valid = append(valid, s)
		}
	}
	all := rawMessages(valid...)
	got, gotOK := conversationKey(all)
	want, wantOK := referenceConversationKey(all)
	if got != want || gotOK != wantOK {
		t.Errorf("all seeds: got %s %v, want %s %v", got, gotOK, want, wantOK)
	}
}

func TestConversationKeyDeepNesting(t *testing.T) {
	for _, depth := range []int{199, 200, 201, 1000, 5000} {
		deep := string(bytes.Repeat([]byte("["), depth)) + string(bytes.Repeat([]byte("]"), depth))
		got, gotOK := conversationKey(rawMessages(deep))
		want, wantOK := referenceConversationKey(rawMessages(deep))
		if got != want || gotOK != wantOK {
			t.Errorf("depth %d: got %s %v, want %s %v", depth, got, gotOK, want, wantOK)
		}
	}
}

// FuzzConversationKey checks that the single-pass canonicalizer gives the
// keys of the encoding/json implementation, for any input.
func FuzzConversationKey(f *testing.F) {
	for _, s := range conversationKeySeeds {
		f.Add([]byte(s))
	}
	f.Add([]byte(`{"role":"user","content":[{"type":"text","text":"a<b"},{"type":"tool_result","content":"\u2028"}]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		msgs := []json.RawMessage{data, json.RawMessage(`{"x":1}`)}
		got, gotOK := conversationKey(msgs)
		want, wantOK := referenceConversationKey(msgs)
		if got != want || gotOK != wantOK {
			t.Fatalf("%q: got %s %v, want %s %v", data, got, gotOK, want, wantOK)
		}
	})
}

// FuzzCanonicalizerOutput compares the canonical bytes themselves, which
// gives a readable failure when the keys differ.
func FuzzCanonicalizerOutput(f *testing.F) {
	for _, s := range conversationKeySeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var c canonicalizer
		got, ok := c.canonical(data)
		if !ok {
			return // declined: conversationKey uses encoding/json
		}
		want, err := stdCanonicalJSON(data)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%q:\n got %s\nwant %s (err %v)", data, got, want, err)
		}
	})
}
