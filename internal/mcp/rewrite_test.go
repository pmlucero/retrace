package mcp

import "testing"

// rewriter pairs a recorded POST body with an incoming one.
func rewriter(t *testing.T, recorded, incoming string) *IDRewriter {
	t.Helper()
	rec, err := ParseBody([]byte(recorded))
	if err != nil {
		t.Fatal(err)
	}
	in, err := ParseBody([]byte(incoming))
	if err != nil {
		t.Fatal(err)
	}
	return NewIDRewriter(rec.Messages, in.Messages)
}

// AC-11: a replayed response carries the id of the incoming request, and the
// rest of the body is kept as recorded (spec §5 step 3).
func TestIDRewriterBody(t *testing.T) {
	tests := []struct {
		name               string
		recorded, incoming string // request bodies
		body, want         string // response bodies
	}{
		{
			name:     "single response",
			recorded: `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
			incoming: `{"jsonrpc":"2.0","id":"abc","method":"tools/list"}`,
			body:     ` {"result": {"tools":[]}, "jsonrpc":"2.0", "id" : 1}` + "\n",
			want:     ` {"result": {"tools":[]}, "jsonrpc":"2.0", "id" : "abc"}` + "\n",
		},
		{
			name:     "error response",
			recorded: `{"id":1,"method":"tools/call"}`,
			incoming: `{"id":9,"method":"tools/call"}`,
			body:     `{"id":1,"error":{"code":-32602,"message":"bad"}}`,
			want:     `{"id":9,"error":{"code":-32602,"message":"bad"}}`,
		},
		{
			name:     "ids compared by value",
			recorded: `{"id":"ab","method":"ping"}`,
			incoming: `{"id":7,"method":"ping"}`,
			body:     `{"id":"ab","result":{}}`,
			want:     `{"id":7,"result":{}}`,
		},
		{
			name:     "batch answered out of order",
			recorded: `[{"id":1,"method":"a"},{"method":"n"},{"id":2,"method":"b"}]`,
			incoming: `[{"id":10,"method":"a"},{"method":"n"},{"id":20,"method":"b"}]`,
			body:     "[ {\"id\":2,\"result\":\"b\"} ,\n {\"id\":1,\"result\":\"a\"} ]",
			want:     "[ {\"id\":20,\"result\":\"b\"} ,\n {\"id\":10,\"result\":\"a\"} ]",
		},
		{
			name:     "repeated id answers in order",
			recorded: `[{"id":1,"method":"a"},{"id":1,"method":"a"}]`,
			incoming: `[{"id":"x","method":"a"},{"id":"y","method":"a"}]`,
			body:     `[{"id":1,"result":1},{"id":1,"result":2},{"id":1,"result":3}]`,
			want:     `[{"id":"x","result":1},{"id":"y","result":2},{"id":1,"result":3}]`,
		},
		{
			name:     "null id kept",
			recorded: `{"id":1,"method":"a"}`,
			incoming: `{"id":2,"method":"a"}`,
			body:     `{"id":null,"error":{"code":-32700,"message":"parse"}}`,
			want:     `{"id":null,"error":{"code":-32700,"message":"parse"}}`,
		},
		{
			name:     "unknown id kept",
			recorded: `{"id":1,"method":"a"}`,
			incoming: `{"id":2,"method":"a"}`,
			body:     `{"id":5,"result":{}}`,
			want:     `{"id":5,"result":{}}`,
		},
		{
			name:     "server request and invalid element kept",
			recorded: `{"id":1,"method":"a"}`,
			incoming: `{"id":2,"method":"a"}`,
			body:     `[{"id":1,"method":"sampling/createMessage"},{"id":1},{"id":1,"result":{}}]`,
			want:     `[{"id":1,"method":"sampling/createMessage"},{"id":1},{"id":2,"result":{}}]`,
		},
		{
			name:     "not JSON-RPC",
			recorded: `{"id":1,"method":"a"}`,
			incoming: `{"id":2,"method":"a"}`,
			body:     `{"id":1,"result":{}} trailing`,
			want:     `{"id":1,"result":{}} trailing`,
		},
		{
			name:     "empty body",
			recorded: `{"id":1,"method":"a"}`,
			incoming: `{"id":2,"method":"a"}`,
			body:     ``,
			want:     ``,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rewriter(t, tt.recorded, tt.incoming).Body([]byte(tt.body))
			if string(got) != tt.want {
				t.Errorf("Body = %s, want %s", got, tt.want)
			}
		})
	}
}

// AC-10, AC-11: in an SSE response, only the ids change; the framing of each
// event is kept.
func TestIDRewriterEvent(t *testing.T) {
	tests := []struct {
		name, event, want string
	}{
		{
			name:  "one line",
			event: "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n",
			want:  "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":\"new\",\"result\":{}}\n\n",
		},
		{
			name:  "no space after colon and CRLF",
			event: "id: 4\r\ndata:{\"id\":1,\"result\":{}}\r\n\r\n",
			want:  "id: 4\r\ndata:{\"id\":\"new\",\"result\":{}}\r\n\r\n",
		},
		{
			name:  "multi-line data keeps its lines",
			event: "data: {\"result\":{},\rdata:  \"id\":1\rdata: }\r\r",
			want:  "data: {\"result\":{},\rdata:  \"id\":\"new\"\rdata: }\r\r",
		},
		{
			name:  "no final line ending",
			event: "data: {\"id\":1,\"result\":{}}",
			want:  "data: {\"id\":\"new\",\"result\":{}}",
		},
		{name: "notification kept", event: "data: {\"method\":\"notifications/progress\"}\n\n", want: "data: {\"method\":\"notifications/progress\"}\n\n"},
		{name: "priming event kept", event: "id: 0\ndata:\n\n", want: "id: 0\ndata:\n\n"},
		{name: "comment kept", event: ": keep-alive\n\n", want: ": keep-alive\n\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rewriter(t, `{"id":1,"method":"a"}`, `{"id":"new","method":"a"}`).Event([]byte(tt.event))
			if string(got) != tt.want {
				t.Errorf("Event = %q, want %q", got, tt.want)
			}
		})
	}
}

// A request is answered once, however many events of the stream carry its id.
func TestIDRewriterStream(t *testing.T) {
	r := rewriter(t, `[{"id":1,"method":"a"},{"id":2,"method":"b"}]`, `[{"id":"x","method":"a"},{"id":"y","method":"b"}]`)
	events := []string{
		"data: {\"id\":2,\"result\":{}}\n\n",
		"data: {\"id\":1,\"result\":{}}\n\n",
		"data: {\"id\":1,\"result\":{}}\n\n",
	}
	want := []string{
		"data: {\"id\":\"y\",\"result\":{}}\n\n",
		"data: {\"id\":\"x\",\"result\":{}}\n\n",
		"data: {\"id\":1,\"result\":{}}\n\n",
	}
	for i, ev := range events {
		if got := r.Event([]byte(ev)); string(got) != want[i] {
			t.Errorf("event %d = %q, want %q", i, got, want[i])
		}
	}
}
