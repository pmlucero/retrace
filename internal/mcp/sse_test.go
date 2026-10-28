package mcp

import "testing"

// Spec §3.4: responses inside an SSE stream are read from the data of each
// stored event.
func TestEventMessages(t *testing.T) {
	tests := []struct {
		name, in string
		want     string
		wantErr  bool
	}{
		{name: "one message", in: "event: message\ndata: {\"id\":1,\"result\":{}}\n\n", want: "response 1"},
		{name: "no space after colon", in: "data:{\"id\":1,\"result\":{}}\n\n", want: "response 1"},
		{name: "CRLF and id field", in: "id: 42\r\ndata: {\"id\":1,\"result\":{}}\r\n\r\n", want: "response 1"},
		{name: "CR line endings", in: "data: {\"id\":1,\r\ndata: \"result\":{}}\r\r", want: "response 1"},
		{
			name: "multi-line data",
			in:   "data: {\"id\":1,\ndata: \"result\":\ndata: {}}\n\n",
			want: "response 1",
		},
		{
			name: "batch",
			in:   "data: [{\"method\":\"notifications/progress\"},{\"id\":2,\"error\":{}}]\n\n",
			want: "notification notifications/progress; error 2",
		},
		{name: "comment only", in: ": keep-alive\n\n", want: ""},
		{name: "no data field", in: "event: ping\nid: 3\n\n", want: ""},
		{name: "priming event with empty data", in: "id: 0\ndata:\n\n", want: ""},
		{name: "bare data field", in: "id: 0\ndata\n\n", want: ""},
		{name: "data is not JSON-RPC", in: "data: hello\n\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := EventMessages([]byte(tt.in))
			if (err != nil) != tt.wantErr {
				t.Fatalf("EventMessages error = %v, want error %t", err, tt.wantErr)
			}
			if s := summaries(got); s != tt.want {
				t.Errorf("messages = %s, want %s", s, tt.want)
			}
		})
	}
}

func TestStreamMessages(t *testing.T) {
	events := [][]byte{
		[]byte("id: 0\ndata:\n\n"),
		[]byte("data: {\"method\":\"notifications/progress\"}\n\n"),
		[]byte("data: not json\n\n"),
		[]byte("data: {\"id\":1,\"result\":{}}\n\n"),
	}
	want := "notification notifications/progress; response 1"
	if s := summaries(StreamMessages(events)); s != want {
		t.Errorf("StreamMessages = %s, want %s", s, want)
	}
}
