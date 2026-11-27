package sse

import (
	"fmt"
	"strings"
	"testing"
)

func TestSplitterKeepsFramingAcrossReads(t *testing.T) {
	stream := "event: message_start\ndata: {}\n\n" +
		": ping comment\r\n\r\n" +
		"event: content_block_delta\rdata: {\"text\":\"hi\"}\r\r" +
		"data: unterminated"
	want := []string{
		"event: message_start\ndata: {}\n\n",
		": ping comment\r\n\r\n",
		"event: content_block_delta\rdata: {\"text\":\"hi\"}\r\r",
	}
	// Every way of cutting the stream in two reads gives the same events.
	for cut := 0; cut <= len(stream); cut++ {
		var s Splitter
		events := append(s.Feed([]byte(stream[:cut])), s.Feed([]byte(stream[cut:]))...)
		got := make([]string, len(events))
		for i, e := range events {
			got[i] = string(e)
		}
		if fmt.Sprintf("%q", got) != fmt.Sprintf("%q", want) {
			t.Fatalf("cut at %d: events = %q, want %q", cut, got, want)
		}
		if string(s.Rest()) != "data: unterminated" {
			t.Fatalf("cut at %d: rest = %q", cut, s.Rest())
		}
	}
}

func TestSplitterByteByByte(t *testing.T) {
	stream := strings.Repeat("data: x\r\n\r\n", 3)
	var s Splitter
	var n int
	for i := range len(stream) {
		n += len(s.Feed([]byte{stream[i]}))
	}
	if n != 3 || len(s.Rest()) != 0 {
		t.Errorf("got %d events and rest %q, want 3 and nothing", n, s.Rest())
	}
}

func TestSplitterReturnsCopies(t *testing.T) {
	var s Splitter
	events := s.Feed([]byte("data: a\n\ndata: b\n\n"))
	s.Feed([]byte("data: c\n\n"))
	if string(events[0]) != "data: a\n\n" || string(events[1]) != "data: b\n\n" {
		t.Errorf("events changed after a later Feed: %q", events)
	}
}

func TestLines(t *testing.T) {
	tests := []struct {
		in   string
		want string // text+eol pairs
	}{
		{"", ""},
		{"a\nb\r\nc\rd", `"a"+"\n" "b"+"\r\n" "c"+"\r" "d"+""`},
		{"a\n\n", `"a"+"\n" ""+"\n"`},
		{"a\r\r", `"a"+"\r" ""+"\r"`},
		{"a\r\n\r\n", `"a"+"\r\n" ""+"\r\n"`},
	}
	for _, tt := range tests {
		var parts []string
		var joined string
		for _, l := range Lines([]byte(tt.in)) {
			parts = append(parts, fmt.Sprintf("%q+%q", l.Text, l.EOL))
			joined += string(l.Text) + string(l.EOL)
		}
		if got := strings.Join(parts, " "); got != tt.want {
			t.Errorf("Lines(%q) = %s, want %s", tt.in, got, tt.want)
		}
		if joined != tt.in {
			t.Errorf("Lines(%q) joined to %q", tt.in, joined)
		}
	}
}

func TestData(t *testing.T) {
	tests := []struct {
		name, event, data string
		ok                bool
	}{
		{"one line", "event: x\ndata: {}\n\n", "{}", true},
		{"no space after colon", "data:{}\r\n\r\n", "{}", true},
		{"only one space dropped", "data:  a\n\n", " a", true},
		{"several lines", "data: a\ndata: b\n\n", "a\nb", true},
		{"CR endings", "data: a\rdata: b\r\r", "a\nb", true},
		{"bare field", "data\n\n", "", true},
		{"empty value", "id: 0\ndata:\n\n", "", true},
		{"comment", ": comment\n\n", "", false},
		{"commented data", ": data: x\n\n", "", false},
		{"other field", "event: ping\nid: 3\n\n", "", false},
		{"field name prefix", "database: x\n\n", "", false},
		{"unterminated", "data: unterminated", "unterminated", true},
		{"no field name", "xxxxxxxxxx", "", false},
		{"empty", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, ok := Data([]byte(tt.event))
			if string(data) != tt.data || ok != tt.ok {
				t.Errorf("Data(%q) = %q, %v; want %q, %v", tt.event, data, ok, tt.data, tt.ok)
			}
			data, ok = LinesData(Lines([]byte(tt.event)))
			if string(data) != tt.data || ok != tt.ok {
				t.Errorf("LinesData(%q) = %q, %v; want %q, %v", tt.event, data, ok, tt.data, tt.ok)
			}
		})
	}
}

func TestDataPrefix(t *testing.T) {
	tests := []struct {
		line string
		n    int
		ok   bool
	}{
		{"data: x", 6, true},
		{"data:x", 5, true},
		{"data:  x", 6, true},
		{"data", 4, true},
		{"data:", 5, true},
		{"event: x", 0, false},
		{": data: x", 0, false},
	}
	for _, tt := range tests {
		if n, ok := DataPrefix([]byte(tt.line)); n != tt.n || ok != tt.ok {
			t.Errorf("DataPrefix(%q) = %d, %v; want %d, %v", tt.line, n, ok, tt.n, tt.ok)
		}
	}
}

func FuzzSplitter(f *testing.F) {
	f.Add("event: a\ndata: {}\n\n: c\r\n\r\ndata: x\r\r", uint8(3))
	f.Add("data: a\rdata: b\r\n\r\ndata", uint8(1))
	f.Fuzz(func(t *testing.T, in string, step uint8) {
		var whole Splitter
		wantEvents := whole.Feed([]byte(in))

		// Feeding in pieces of any size gives the same events and rest.
		var pieces Splitter
		var gotEvents [][]byte
		size := int(step)%7 + 1
		for i := 0; i < len(in); i += size {
			gotEvents = append(gotEvents, pieces.Feed([]byte(in[i:min(i+size, len(in))]))...)
		}
		if fmt.Sprintf("%q", gotEvents) != fmt.Sprintf("%q", wantEvents) {
			t.Fatalf("pieces of %d: events = %q, want %q", size, gotEvents, wantEvents)
		}
		if string(pieces.Rest()) != string(whole.Rest()) {
			t.Fatalf("pieces of %d: rest = %q, want %q", size, pieces.Rest(), whole.Rest())
		}

		// Events and rest are the input, byte for byte.
		var joined []byte
		for _, e := range wantEvents {
			joined = append(joined, e...)
			var lines []byte
			for _, l := range Lines(e) {
				lines = append(lines, l.Text...)
				lines = append(lines, l.EOL...)
			}
			if string(lines) != string(e) {
				t.Fatalf("Lines(%q) do not rebuild the event", e)
			}
			d1, ok1 := Data(e)
			d2, ok2 := LinesData(Lines(e))
			if string(d1) != string(d2) || ok1 != ok2 {
				t.Fatalf("Data and LinesData differ on %q", e)
			}
		}
		joined = append(joined, whole.Rest()...)
		if string(joined) != in {
			t.Fatalf("events + rest = %q, want %q", joined, in)
		}
		Data([]byte(in))
	})
}
