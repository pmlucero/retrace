package mcp

import (
	"strings"
	"testing"
)

func mustParse(t *testing.T, body string) []Message {
	t.Helper()
	if body == "" {
		return nil
	}
	b, err := ParseBody([]byte(body))
	if err != nil {
		t.Fatalf("ParseBody(%s): %v", body, err)
	}
	return b.Messages
}

// Spec §3.4: each JSON-RPC request of a POST is matched to its response.
func TestPairMessages(t *testing.T) {
	tests := []struct {
		name               string
		requests, response string
		pairs              string // "request -> response", "-" when unanswered
		unmatched, server  string
	}{
		{
			name:     "single",
			requests: `{"id":1,"method":"tools/list"}`,
			response: `{"id":1,"result":{}}`,
			pairs:    "request 1 tools/list -> response 1",
		},
		{
			name:     "batch answered out of order",
			requests: `[{"id":1,"method":"a"},{"method":"n"},{"id":"two","method":"b"}]`,
			response: `[{"id":"two","error":{}},{"id":1,"result":{}}]`,
			pairs:    `request 1 a -> response 1; request "two" b -> error "two"`,
		},
		{
			name:     "ids compared by value",
			requests: `{"id":"ab","method":"a"}`,
			response: `{"id":"ab","result":{}}`,
			pairs:    `request "ab" a -> response "ab"`,
		},
		{
			name:      "number text differs",
			requests:  `{"id":1,"method":"a"}`,
			response:  `{"id":1.0,"result":{}}`,
			pairs:     "request 1 a -> -",
			unmatched: "response 1.0",
		},
		{
			name:     "string and number ids differ",
			requests: `[{"id":1,"method":"a"},{"id":"1","method":"b"}]`,
			response: `[{"id":"1","result":{}},{"id":1,"result":{}}]`,
			pairs:    `request 1 a -> response 1; request "1" b -> response "1"`,
		},
		{
			name:      "unanswered and unknown",
			requests:  `[{"id":1,"method":"a"},{"id":2,"method":"b"}]`,
			response:  `[{"id":2,"result":{}},{"id":9,"result":{}},{"id":null,"error":{}}]`,
			pairs:     "request 1 a -> -; request 2 b -> response 2",
			unmatched: "response 9; error null",
		},
		{
			name:      "repeated id answered in order",
			requests:  `[{"id":1,"method":"a"},{"id":1,"method":"b"}]`,
			response:  `[{"id":1,"result":1},{"id":1,"result":2},{"id":1,"result":3}]`,
			pairs:     "request 1 a -> response 1; request 1 b -> response 1",
			unmatched: "response 1",
		},
		{
			name:     "server messages in the stream",
			requests: `{"id":1,"method":"tools/call"}`,
			response: `[{"method":"notifications/progress"},{"id":"s1","method":"sampling/createMessage"},{"id":1,"result":{}}]`,
			pairs:    "request 1 tools/call -> response 1",
			server:   `notification notifications/progress; request "s1" sampling/createMessage`,
		},
		{
			name:     "client notifications and responses expect no answer",
			requests: `[{"method":"notifications/initialized"},{"id":"s1","result":{}}]`,
			response: ``,
		},
		{
			name:     "invalid elements ignored",
			requests: `[{"id":1,"method":"a"},{"foo":1}]`,
			response: `[{"bar":1},{"id":1,"result":{}}]`,
			pairs:    "request 1 a -> response 1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PairMessages(mustParse(t, tt.requests), mustParse(t, tt.response))
			pairs := make([]string, len(got.Pairs))
			for i, p := range got.Pairs {
				resp := "-"
				if p.Response != nil {
					resp = summary(*p.Response)
				}
				pairs[i] = summary(p.Request) + " -> " + resp
			}
			if s := strings.Join(pairs, "; "); s != tt.pairs {
				t.Errorf("pairs = %s, want %s", s, tt.pairs)
			}
			if s := summaries(got.Unmatched); s != tt.unmatched {
				t.Errorf("unmatched = %s, want %s", s, tt.unmatched)
			}
			if s := summaries(got.ServerMessages); s != tt.server {
				t.Errorf("server messages = %s, want %s", s, tt.server)
			}
		})
	}
}

// Each pair carries its own response, with the result as sent.
func TestPairMessagesResponses(t *testing.T) {
	got := PairMessages(
		mustParse(t, `[{"id":1,"method":"a"},{"id":2,"method":"b"}]`),
		mustParse(t, `[{"id":2,"result":"two"},{"id":1,"result":"one"}]`),
	)
	if r := string(got.Pairs[0].Response.Result); r != `"one"` {
		t.Errorf("first result = %s, want \"one\"", r)
	}
	if r := string(got.Pairs[1].Response.Result); r != `"two"` {
		t.Errorf("second result = %s, want \"two\"", r)
	}
}
