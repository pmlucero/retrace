package mcp

import (
	"fmt"
	"strings"
	"testing"
)

// summary describes a message in one line, for comparisons in tests.
func summary(m Message) string {
	switch m.Kind {
	case Request:
		return "request " + string(m.ID) + " " + m.Method
	case Notification:
		return "notification " + m.Method
	case Response:
		if m.Error != nil {
			return "error " + string(m.ID)
		}
		return "response " + string(m.ID)
	}
	return "invalid " + string(m.Raw)
}

func summaries(msgs []Message) string {
	s := make([]string, len(msgs))
	for i, m := range msgs {
		s[i] = summary(m)
	}
	return strings.Join(s, "; ")
}

// Spec §3.4: a POST body is a single JSON-RPC message or a batch.
func TestParseBody(t *testing.T) {
	tests := []struct {
		name, in string
		want     string
		batch    bool
		wantErr  bool
	}{
		{name: "request", in: `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, want: "request 1 tools/list"},
		{name: "string id kept as is", in: `{"jsonrpc":"2.0","id":"a` + escaped('b') + `","method":"ping"}`, want: `request "a` + escaped('b') + `" ping`},
		{name: "number id text kept", in: `{"id":1.0,"method":"ping"}`, want: "request 1.0 ping"},
		{name: "notification", in: `{"jsonrpc":"2.0","method":"notifications/initialized"}`, want: "notification notifications/initialized"},
		{name: "result", in: `{"jsonrpc":"2.0","id":7,"result":{}}`, want: "response 7"},
		{name: "null result", in: `{"id":7,"result":null}`, want: "response 7"},
		{name: "error", in: `{"id":7,"error":{"code":-32601,"message":"no"}}`, want: "error 7"},
		{name: "error with null id", in: `{"id":null,"error":{"code":-32700,"message":"parse"}}`, want: "error null"},
		{name: "surrounding whitespace", in: " \n{\"id\":1,\"method\":\"ping\"}\n", want: "request 1 ping"},
		{
			name:  "batch",
			in:    `[{"id":1,"method":"a"},{"method":"b"},{"id":"x","result":1}]`,
			want:  `request 1 a; notification b; response "x"`,
			batch: true,
		},
		{name: "batch of one", in: ` [{"id":1,"method":"a"}]`, want: "request 1 a", batch: true},
		{
			name:  "invalid batch elements kept in place",
			in:    `[1,{"id":1,"method":"a"},{"id":null,"method":"b"},{"foo":1}]`,
			want:  `invalid 1; request 1 a; invalid {"id":null,"method":"b"}; invalid {"foo":1}`,
			batch: true,
		},
		{name: "empty batch", in: `[]`, wantErr: true},
		{name: "not JSON", in: `hello`, wantErr: true},
		{name: "empty", in: ``, wantErr: true},
		{name: "truncated batch", in: `[{"id":1,"method":"a"}`, wantErr: true},
		{name: "not an object", in: `"ping"`, wantErr: true},
		{name: "null", in: `null`, wantErr: true},
		{name: "method not a string", in: `{"id":1,"method":5}`, wantErr: true},
		{name: "request with null id", in: `{"id":null,"method":"ping"}`, wantErr: true},
		{name: "request with object id", in: `{"id":{},"method":"ping"}`, wantErr: true},
		{name: "method and result", in: `{"id":1,"method":"a","result":1}`, wantErr: true},
		{name: "result and error", in: `{"id":1,"result":1,"error":{}}`, wantErr: true},
		{name: "response without id", in: `{"result":1}`, wantErr: true},
		{name: "response with boolean id", in: `{"id":true,"result":1}`, wantErr: true},
		{name: "member names are case-sensitive", in: `{"ID":1,"Result":{}}`, wantErr: true},
		{name: "upper-case method is not a method", in: `{"Method":"ping"}`, wantErr: true},
		{name: "duplicate member", in: `{"id":1,"result":1,"result":2}`, want: "response 1"},
		{name: "trailing data", in: `{"id":1,"result":1} {}`, wantErr: true},
		{name: "unterminated", in: `{"id":1,"result":1`, wantErr: true},
		{name: "unterminated after a member", in: `{"id":1,"result":1,`, wantErr: true},
		{name: "malformed member name", in: `{"id":1,2:3}`, wantErr: true},
		{name: "malformed member value", in: `{"id":1,"result":tru}`, wantErr: true},
		{name: "malformed nested value", in: `{"id":1,"params":{"a"}}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseBody([]byte(tt.in))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseBody = %s, want an error", summaries(got.Messages))
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseBody: %v", err)
			}
			if s := summaries(got.Messages); s != tt.want {
				t.Errorf("messages = %s, want %s", s, tt.want)
			}
			if got.Batch != tt.batch {
				t.Errorf("Batch = %t, want %t", got.Batch, tt.batch)
			}
		})
	}
}

var escapedI = escaped('i')

// escaped returns the JSON \uXXXX escape of c, for ids and keys that are
// equal by value but not byte for byte.
func escaped(c byte) string { return fmt.Sprintf("%cu%04x", '\\', c) }

// Spec §5 step 3: a replayed response gets the incoming request's id, and
// the rest of the message is kept as recorded.
func TestWithID(t *testing.T) {
	tests := []struct {
		name, in, id, want string
	}{
		{"number to string", `{"jsonrpc":"2.0","id":1,"result":{"id":1}}`, `"x"`, `{"jsonrpc":"2.0","id":"x","result":{"id":1}}`},
		{"spacing kept", "{ \"result\" : [ 1 ] , \"id\" : \"long-id\" }", `7`, "{ \"result\" : [ 1 ] , \"id\" : 7 }"},
		{"escaped key", "{\"" + escapedI + "d\":2,\"result\":{}}", `3`, "{\"" + escapedI + "d\":3,\"result\":{}}"},
		{"other case is another member", `{"ID":"keep","id":1,"result":{}}`, `2`, `{"ID":"keep","id":2,"result":{}}`},
		{"duplicate id uses the last", `{"id":1,"id":2,"result":null}`, `9`, `{"id":1,"id":9,"result":null}`},
		{"null id", `{"id":null,"error":{}}`, `4`, `{"id":4,"error":{}}`},
		{"notification unchanged", `{"method":"n","params":{"id":1}}`, `5`, `{"method":"n","params":{"id":1}}`},
		{"server request unchanged", `{"id":"s1","method":"sampling/createMessage"}`, `5`, `{"id":"s1","method":"sampling/createMessage"}`},
	}
	for _, tt := range tests {
		body, err := ParseBody([]byte(tt.in))
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if got := string(body.Messages[0].WithID([]byte(tt.id))); got != tt.want {
			t.Errorf("%s: WithID = %s, want %s", tt.name, got, tt.want)
		}
	}
}

// A caller may reuse its buffer once ParseBody returns.
func TestParseBodyDoesNotAlias(t *testing.T) {
	buf := []byte(`{"id":1,"result":{}}`)
	body, err := ParseBody(buf)
	if err != nil {
		t.Fatal(err)
	}
	copy(buf, `XXXXXXXXXXXXXXXXXXXX`)
	if got := string(body.Messages[0].Raw); got != `{"id":1,"result":{}}` {
		t.Errorf("Raw = %s after the buffer was reused", got)
	}
}

// Replay rewrites ids and derivation reads params and results, so the
// members are kept as they appear in the body.
func TestParseBodyKeepsMembers(t *testing.T) {
	in := `[{"jsonrpc":"2.0","id":"r-1","method":"tools/call","params":{"name":"x","arguments":{"b":1.50,"a":[]}}},` +
		`{"jsonrpc":"2.0","method":"n"}]`
	body, err := ParseBody([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	m := body.Messages[0]
	if string(m.Raw) != `{"jsonrpc":"2.0","id":"r-1","method":"tools/call","params":{"name":"x","arguments":{"b":1.50,"a":[]}}}` {
		t.Errorf("Raw = %s", m.Raw)
	}
	if string(m.ID) != `"r-1"` || string(m.Params) != `{"name":"x","arguments":{"b":1.50,"a":[]}}` {
		t.Errorf("ID = %s, Params = %s", m.ID, m.Params)
	}
	if n := body.Messages[1]; n.ID != nil || n.Params != nil {
		t.Errorf("notification ID = %s, Params = %s, want both nil", n.ID, n.Params)
	}
}
