package derive

import (
	"encoding/json"
	"testing"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/mcp"
)

// mcpPost is a POST to the MCP server "orders", as the proxy records it.
func mcpPost(startMillis int64, request string) exchange {
	return exchange{channel: "mcp", upstream: "mcp:orders", path: "/", startMillis: startMillis, request: request}
}

func (x exchange) answer(status int, response string, durationMillis int64) exchange {
	x.status, x.response, x.durationMillis = status, response, durationMillis
	return x
}

func (x exchange) stream(durationMillis int64, chunks ...string) exchange {
	if chunks == nil {
		chunks = []string{} // streamed, with no events
	}
	x.status, x.chunks, x.durationMillis = 200, chunks, durationMillis
	return x
}

func (x exchange) failed(err string) exchange {
	x.err = err
	return x
}

const (
	searchCall = `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search",` +
		"\n  " + `"arguments":{"customer_id":"c_812", "status":"open"}}}`
	searchOK = `{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"2 open orders"}],"structuredContent":{"count":2}}}`
)

// mcpScenarios cover the MCP events (spec §7.4).
var mcpScenarios = []scenario{
	{
		// A session with JSON responses. Every client request and
		// notification gets an event; client responses, GET, DELETE and
		// bodies that are not JSON-RPC get none.
		name: "mcp_json_session",
		exchanges: []exchange{
			mcpPost(0, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`).
				answer(200, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18"}}`, 10),
			mcpPost(20, `{"jsonrpc":"2.0","method":"notifications/initialized"}`).answer(202, "", 2),
			{channel: "mcp", upstream: "mcp:orders", path: "/", method: "GET", startMillis: 30, status: 405, durationMillis: 1},
			mcpPost(40, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`).
				answer(200, `{"jsonrpc":"2.0","id":2,"result":{"tools":[]}}`, 5),
			mcpPost(100, searchCall).answer(200, searchOK, 50),
			mcpPost(200, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"cancel","arguments":{"order":"o_1"}}}`).
				answer(200, `{"jsonrpc":"2.0","id":4,"result":{"content":[{"type":"text","text":"order o_1 is shipped"},{"type":"text","text":"cannot cancel"}],"isError":true}}`, 30),
			mcpPost(300, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"nope","arguments":{}}}`).
				answer(200, `{"jsonrpc":"2.0","id":5,"error":{"code":-32602,"message":"Unknown tool: nope"}}`, 3),
			mcpPost(400, `{"jsonrpc":"2.0","id":"s1","result":{}}`).answer(202, "", 1),
			mcpPost(500, `not json-rpc`).answer(400, `bad request`, 1),
			{channel: "mcp", upstream: "mcp:orders", path: "/", method: "DELETE", startMillis: 600, status: 200, durationMillis: 1},
		},
	},
	{
		// A batch answered over SSE, with the responses out of order and
		// mixed with server messages and a priming event: one event per
		// message, and each tool_result points to its own tool_call.
		name: "mcp_sse_batch",
		exchanges: []exchange{
			mcpPost(0, `[{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","arguments":{"q":"a"}}},`+
				`{"jsonrpc":"2.0","method":"notifications/progress"},`+
				`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search","arguments":{"q":"b"}}},`+
				`{"jsonrpc":"2.0","id":3,"method":"ping"},`+
				`{"not":"a message"}]`).stream(120,
				"id: 0\ndata:\n\n",
				sse("message", `{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info"}}`),
				sse("message", `{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"b"}]}}`),
				sse("message", `{"jsonrpc":"2.0","id":"srv-1","method":"sampling/createMessage","params":{}}`),
				sse("message", `not json`),
				sse("message", `[{"jsonrpc":"2.0","id":3,"result":{}},{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"a"}]}}]`),
			),
		},
	},
	{
		// tools/call responses that are missing or not usable each give an
		// error, mirroring model_response. A response that arrived before
		// the agent closed the stream still counts.
		name: "mcp_incomplete",
		exchanges: []exchange{
			mcpPost(0, searchCall).stream(40, sse("message", `{"jsonrpc":"2.0","method":"notifications/progress"}`)).
				failed("upstream connection reset"),
			mcpPost(100, searchCall).failed("dial tcp: connection refused").answer(0, "", 30000),
			mcpPost(40000, searchCall).answer(200, searchOK, 10).
				failed(cassette.RecordingIncomplete + ": 1 record(s) not written: queue full"),
			mcpPost(41000, searchCall).answer(500, `<html>Internal error</html>`, 10),
			mcpPost(42000, searchCall).answer(200, `{"jsonrpc":"2.0","id":99,"result":{}}`, 10),
			mcpPost(43000, searchCall).stream(10),
			mcpPost(44000, searchCall).stream(10, sse("message", searchOK)).
				failed("the agent closed the connection before the response ended"),
			mcpPost(45000, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":"[REDACTED:1a2b3c4d]"}`).
				answer(200, `{"jsonrpc":"2.0","id":6,"result":"[REDACTED:5e6f7a8b]"}`, 10),
		},
	},
}

// AC-05 (MCP): the status and error of a tool_result (spec §7.4).
func TestToolResult(t *testing.T) {
	ok := cassette.Completion{StatusCode: ptr(200), ResponseBlob: ptr("k"), DurationMillis: 5}
	parse := func(s string) *mcp.Message {
		b, err := mcp.ParseBody([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		return &b.Messages[0]
	}
	incomplete := cassette.RecordingIncomplete + ": queue full"
	tests := []struct {
		name       string
		done       cassette.Completion
		resp       *mcp.Message
		parseErr   error
		wantResult string
		wantErr    string
	}{
		{"ok", ok, parse(`{"id":1,"result":{"content":[]}}`), nil, `{"content":[]}`, ""},
		{"tool error", ok, parse(`{"id":1,"result":{"content":[{"type":"text","text":"boom"}],"isError":true}}`), nil,
			`{"content":[{"type":"text","text":"boom"}],"isError":true}`, "boom"},
		{"tool error without text", ok, parse(`{"id":1,"result":{"isError":true}}`), nil, `{"isError":true}`, "the tool reported an error"},
		{"JSON-RPC error", ok, parse(`{"id":1,"error":{"code":-32603,"message":"Internal error"}}`), nil, "", "-32603: Internal error"},
		{"JSON-RPC error that is not an object", ok, parse(`{"id":1,"error":"boom"}`), nil, "", "unrecognized JSON-RPC error"},
		{"incomplete recording", cassette.Completion{StatusCode: ptr(200), Error: &incomplete}, parse(`{"id":1,"result":{}}`), nil, `{}`, incomplete},
		{"error after the response", cassette.Completion{StatusCode: ptr(200), Streamed: true, Error: ptr("reset")}, parse(`{"id":1,"result":{}}`), nil, `{}`, ""},
		{"transport error", cassette.Completion{Error: ptr("reset")}, nil, nil, "", "reset"},
		{"no response", cassette.Completion{}, nil, nil, "", "no response"},
		{"HTTP status", cassette.Completion{StatusCode: ptr(404), ResponseBlob: ptr("k")}, nil, nil, "", "HTTP 404"},
		{"unparsed body", ok, nil, json.Unmarshal([]byte("x"), &struct{}{}), "", "unrecognized response body"},
		{"stream without the response", cassette.Completion{StatusCode: ptr(200), Streamed: true}, nil, nil, "",
			"the stream ended before the response to this request"},
		{"body not recorded", cassette.Completion{StatusCode: ptr(200)}, nil, nil, "", "the response body was not recorded"},
		{"other id", ok, nil, nil, "", "the response has no message with this request's id"},
	}
	for _, tt := range tests {
		result, errMsg := toolResult(tt.done, tt.resp, tt.parseErr)
		if string(result) != tt.wantResult || errMsg != tt.wantErr {
			t.Errorf("%s: toolResult = %s, %q; want %s, %q", tt.name, result, errMsg, tt.wantResult, tt.wantErr)
		}
	}
}
