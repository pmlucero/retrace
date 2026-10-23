package proxy

import (
	"fmt"
	"strings"
	"testing"
)

func TestSSESplitterKeepsFramingAcrossReads(t *testing.T) {
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
		var s sseSplitter
		events := append(s.feed([]byte(stream[:cut])), s.feed([]byte(stream[cut:]))...)
		got := make([]string, len(events))
		for i, e := range events {
			got[i] = string(e)
		}
		if fmt.Sprintf("%q", got) != fmt.Sprintf("%q", want) {
			t.Fatalf("cut at %d: events = %q, want %q", cut, got, want)
		}
		if string(s.rest()) != "data: unterminated" {
			t.Fatalf("cut at %d: rest = %q", cut, s.rest())
		}
	}
}

func TestSSESplitterByteByByte(t *testing.T) {
	stream := strings.Repeat("data: x\r\n\r\n", 3)
	var s sseSplitter
	var n int
	for i := range len(stream) {
		n += len(s.feed([]byte{stream[i]}))
	}
	if n != 3 || len(s.rest()) != 0 {
		t.Errorf("got %d events and rest %q, want 3 and nothing", n, s.rest())
	}
}
