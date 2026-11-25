package jsonvalue

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// referenceQuote is the quoting EncodeCanonical must reproduce byte for byte:
// encoding/json without HTML escaping, except that each invalid UTF-8 byte is
// the escape \ufffd on every Go version (Go 1.27's encoding/json writes a raw
// U+FFFD instead). Valid runs are quoted by encoding/json.
func referenceQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	start := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteString(jsonQuoteBody(s[start:i]))
			b.WriteString(`\ufffd`)
			i++
			start = i
			continue
		}
		i += size
	}
	b.WriteString(jsonQuoteBody(s[start:]))
	b.WriteByte('"')
	return b.String()
}

// jsonQuoteBody quotes valid UTF-8 with encoding/json, HTML escaping off, and
// strips the surrounding quotes.
func jsonQuoteBody(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	out := strings.TrimSuffix(b.String(), "\n")
	return out[1 : len(out)-1]
}

// quote runs the quoting used by the encoder under test.
func quote(s string) string {
	return string(appendQuoted(nil, s))
}

func quoteSeeds() []string {
	return []string{
		"", "plain", `a"b\c`, "a\nb\tc\rd\be\ff", "\x00\x01\x1f\x7f",
		"<>&", "  ", "café \U0001F600", "\xff\xfe", "ok\xc3",
		"\xed\xa0\x80", "�", "a\xe2\x80", "\x80abc",
	}
}

func TestQuoteMatchesEncodingJSON(t *testing.T) {
	for _, s := range quoteSeeds() {
		if got, want := quote(s), referenceQuote(s); got != want {
			t.Errorf("quote(%q) = %s, want %s", s, got, want)
		}
	}
	for r := rune(0); r < 0x3000; r++ {
		s := "x" + string(r) + "y"
		if got, want := quote(s), referenceQuote(s); got != want {
			t.Fatalf("rune %U: got %s, want %s", r, got, want)
		}
	}
}

func FuzzEncodeCanonicalString(f *testing.F) {
	for _, s := range quoteSeeds() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		want := referenceQuote(s)
		if got := quote(s); got != want {
			t.Fatalf("quote(%q) = %s, want %s", s, got, want)
		}
		if got := string(EncodeCanonical(s)); got != want {
			t.Fatalf("EncodeCanonical(%q) = %s, want %s", s, got, want)
		}
		if !utf8.ValidString(want) {
			t.Fatalf("output not valid UTF-8: %q", want)
		}
	})
}

// anthropicBody builds a request body shaped like an Anthropic Messages call
// with the given number of turns.
func anthropicBody(turns int) []byte {
	var b strings.Builder
	b.WriteString(`{"model":"claude-sonnet-4-5","max_tokens":1024,"system":"You are a careful <assistant> & \"helper\".","tools":[`)
	for i := range 5 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"name":"tool_%d","description":"Does thing %d.\nSecond line.","input_schema":{"type":"object","properties":{"path":{"type":"string"},"n":{"type":"integer"}},"required":["path"]}}`, i, i)
	}
	b.WriteString(`],"messages":[`)
	text := strings.Repeat("The quick brown fox jumps over the lazy dog. é世 ", 12)
	for i := range turns {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"role":"user","content":[{"type":"text","text":"turn %d: %s"}]},`, i, text)
		fmt.Fprintf(&b, `{"role":"assistant","content":[{"type":"text","text":"%s"},{"type":"tool_use","id":"toolu_%d","name":"tool_1","input":{"path":"/tmp/f%d.txt","n":%d}}]},`, text, i, i, i)
		fmt.Fprintf(&b, `{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_%d","content":"%s"}]}`, i, text)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

func BenchmarkEncodeCanonical(b *testing.B) {
	for _, tc := range []struct {
		name  string
		turns int
	}{{"small", 1}, {"250KB", 115}} {
		body := anthropicBody(tc.turns)
		v, err := Decode(body)
		if err != nil {
			b.Fatal(err)
		}
		b.Run(tc.name, func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for b.Loop() {
				EncodeCanonical(v)
			}
		})
	}
}
