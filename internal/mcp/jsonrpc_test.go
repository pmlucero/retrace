package mcp

import (
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
		{name: "string id kept as is", in: `{"jsonrpc":"2.0","id":"ab","method":"ping"}`, want: `request "ab" ping`},
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
