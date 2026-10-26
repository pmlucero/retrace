package derive

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/provider/anthropic"
)

func TestResponseError(t *testing.T) {
	blob := ptr("k")
	tests := []struct {
		name     string
		done     cassette.Completion
		msg      anthropic.Message
		parseErr error
		want     string
	}{
		{"ok", cassette.Completion{StatusCode: ptr(200), ResponseBlob: blob}, anthropic.Message{Complete: true}, nil, ""},
		{"api error first", cassette.Completion{StatusCode: ptr(500), Error: ptr("x")}, anthropic.Message{Error: "api_error: boom"}, nil, "api_error: boom"},
		{"interaction error", cassette.Completion{StatusCode: ptr(200), Error: ptr("recording incomplete")}, anthropic.Message{Complete: true}, nil, "recording incomplete"},
		{"no response", cassette.Completion{}, anthropic.Message{}, nil, "no response"},
		{"HTTP status", cassette.Completion{StatusCode: ptr(404), ResponseBlob: blob}, anthropic.Message{}, anthropic.ErrNotMessage, "HTTP 404"},
		{"unparsed 2xx body", cassette.Completion{StatusCode: ptr(200), ResponseBlob: blob}, anthropic.Message{}, anthropic.ErrNotMessage, "unrecognized response body"},
		{"stream without message_stop", cassette.Completion{StatusCode: ptr(200), Streamed: true}, anthropic.Message{}, nil, "the stream ended before message_stop"},
		{"body not recorded", cassette.Completion{StatusCode: ptr(200)}, anthropic.Message{}, nil, "the response body was not recorded"},
	}
	for _, tt := range tests {
		if got := responseError(tt.done, tt.msg, tt.parseErr); got != tt.want {
			t.Errorf("%s: responseError = %q, want %q", tt.name, got, tt.want)
		}
	}
}

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

var t0 = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

// exchange is one interaction of a test scenario, written through the
// cassette writer as the proxy would.
type exchange struct {
	channel, upstream, method, path string // default llm, anthropic, POST, /v1/messages
	startMillis                     int64
	request                         string
	status                          int // 0: no response
	response                        string
	chunks                          []string // set for a streamed response
	durationMillis                  int64
	err                             string
}

func (x exchange) interaction() cassette.Interaction {
	in := cassette.Interaction{
		Channel: or(x.channel, "llm"), Upstream: or(x.upstream, "anthropic"),
		HTTPMethod: or(x.method, "POST"), Path: or(x.path, "/v1/messages"),
		StartedAt: t0.Add(time.Duration(x.startMillis) * time.Millisecond), Provenance: "recorded",
	}
	if x.request != "" {
		in.RequestBlob = ptr(cassette.BlobKey([]byte(x.request)))
	}
	return in
}

func (x exchange) completion() (cassette.Completion, [][]byte) {
	done := cassette.Completion{DurationMillis: x.durationMillis, Streamed: x.chunks != nil}
	if x.status != 0 {
		done.StatusCode = ptr(x.status)
		done.TTFBMillis = ptr(int64(1))
	}
	if x.err != "" {
		done.Error = ptr(x.err)
	}
	var blobs [][]byte
	if x.status != 0 && x.chunks == nil {
		done.ResponseBlob = ptr(cassette.BlobKey([]byte(x.response)))
		blobs = append(blobs, []byte(x.response))
	}
	return done, blobs
}

func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// record writes the exchanges, one after the other, through a writer that
// derives events, and returns the closed-and-reopened cassette.
func record(t *testing.T, exchanges []exchange) *cassette.Cassette {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "s.cassette")
	c, err := cassette.Create(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	err = c.Write(ctx, func(tx *cassette.Tx) error {
		return tx.InsertSession(ctx, cassette.Session{ID: "s", Mode: "record", Status: "recording",
			CreatedAt: t0, Command: []string{"agent"}, OnMiss: "strict", DeriverVersion: Version})
	})
	if err != nil {
		t.Fatal(err)
	}
	w, err := cassette.NewWriter(ctx, c, cassette.WriterOptions{Deriver: New()})
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range exchanges {
		var reqBlobs [][]byte
		if x.request != "" {
			reqBlobs = append(reqBlobs, []byte(x.request))
		}
		id := w.BeginInteraction(x.interaction(), reqBlobs...)
		for i, ch := range x.chunks {
			w.AppendChunk(cassette.Chunk{InteractionID: id, Index: i, OffsetMillis: int64(i), Data: []byte(ch)})
		}
		done, respBlobs := x.completion()
		w.CompleteInteraction(id, done, respBlobs...)
	}
	if err := w.Close(ctx, cassette.SessionEnd{Status: "completed", EndedAt: t0.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if st := w.Stats(); st.Dropped != 0 {
		t.Fatalf("writer dropped records: %+v", st)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = cassette.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// sse builds the data line of one SSE event.
func sse(typ, data string) string {
	return "event: " + typ + "\ndata: " + data + "\n\n"
}

const (
	weatherQuestion = `{"model":"claude-sonnet-5-5","max_tokens":1024,"tools":[{"name":"get_weather","input_schema":{"type":"object"}}],` +
		`"messages":[{"role":"user","content":"What's the weather in Paris?"}]}`
	weatherToolUse = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5-5",` +
		`"content":[{"type":"text","text":"Let me check."},{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Paris"}}],` +
		`"stop_reason":"tool_use","usage":{"input_tokens":1200,"output_tokens":80,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`
	weatherFollowUp = `{"model":"claude-sonnet-5-5","max_tokens":1024,"messages":[` +
		`{"role":"user","content":"What's the weather in Paris?"},` +
		`{"role":"assistant","content":[{"type":"text","text":"Let me check."},{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Paris"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"18°C, sunny"}]}]}`
	weatherAnswer = `{"id":"msg_2","type":"message","role":"assistant","model":"claude-sonnet-5-5",` +
		`"content":[{"type":"text","text":"It is 18°C and sunny in Paris."}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":1400,"output_tokens":20,"cache_read_input_tokens":1000,"cache_creation_input_tokens":0}}`
)

func streamedToolUse() []string {
	return []string{
		sse("message_start", `{"type":"message_start","message":{"id":"msg_3","model":"claude-opus-5-5","usage":{"input_tokens":50,"output_tokens":1,"cache_creation_input_tokens":2000,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":2000}}}}`),
		sse("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		sse("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Searching <orders> & more"}}`),
		sse("content_block_stop", `{"type":"content_block_stop","index":0}`),
		sse("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_2","name":"orders__search","input":{}}}`),
		sse("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"customer_id\": \"c_812\","}}`),
		sse("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":" \"status\": \"open\"}"}}`),
		sse("content_block_stop", `{"type":"content_block_stop","index":1}`),
		sse("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":64}}`),
		sse("message_stop", `{"type":"message_stop"}`),
	}
}

var scenarios = []struct {
	name      string
	exchanges []exchange
}{
	{
		// A tool loop with JSON responses: one user_message, two
		// model_responses, one tool_use.
		name: "json_tool_loop",
		exchanges: []exchange{
			{request: weatherQuestion, status: 200, response: weatherToolUse, durationMillis: 900},
			{startMillis: 1000, request: weatherFollowUp, status: 200, response: weatherAnswer, durationMillis: 400},
		},
	},
	{
		name: "sse_tool_use",
		exchanges: []exchange{
			{path: "/v1/messages?beta=true", request: `{"model":"claude-opus-5-5","stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"Find open orders"},{"type":"text","text":"for c_812"}]}]}`,
				status: 200, chunks: streamedToolUse(), durationMillis: 1500},
		},
	},
	{
		// A retried request is the same user message: it gets one
		// user_message, and both responses point to it.
		name: "retry_and_errors",
		exchanges: []exchange{
			{request: weatherQuestion, status: 529, response: `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, durationMillis: 100},
			{startMillis: 2000, request: weatherQuestion, durationMillis: 30000, err: "dial tcp: connection refused"},
			{startMillis: 40000, request: weatherQuestion, status: 200, durationMillis: 300, chunks: []string{
				streamedToolUse()[0],
				sse("error", `{"type":"error","error":{"type":"api_error","message":"Internal server error"}}`),
			}},
			{startMillis: 50000, request: weatherQuestion, status: 200, durationMillis: 300, err: "the agent closed the connection before the response ended",
				chunks: streamedToolUse()[:6]},
			{startMillis: 60000, request: weatherQuestion, status: 502, response: `<html>Bad gateway</html>`, durationMillis: 10},
		},
	},
	{
		// Only POST /v1/messages on the LLM channel derives events. A model
		// the price table does not know gets no cost.
		name: "other_paths_and_unknown_model",
		exchanges: []exchange{
			{path: "/v1/messages/count_tokens", request: weatherQuestion, status: 200, response: `{"input_tokens":12}`, durationMillis: 5},
			{method: "GET", path: "/v1/models", status: 200, response: `{"data":[]}`, durationMillis: 5},
			{channel: "mcp", upstream: "mcp:orders", path: "/", request: `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, status: 200, response: `{"jsonrpc":"2.0","id":1,"result":{}}`, durationMillis: 5},
			{startMillis: 100, request: `{"model":"claude-test","messages":[{"role":"user","content":"[REDACTED:1a2b3c4d]"}]}`, status: 200,
				response: `{"type":"message","model":"claude-test","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`, durationMillis: 50},
		},
	},
}

// AC-05 (LLM): events are derived with their §7.4 fields, including tokens
// and estimated cost for each model_response, and correct source and target.
func TestDeriveGolden(t *testing.T) {
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			c := record(t, sc.exchanges)
			events, err := c.Events(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			checkGolden(t, filepath.Join("testdata", sc.name+".jsonl"), eventsJSONL(t, events))
		})
	}
}

// eventsJSONL renders events one per line, with every column.
func eventsJSONL(t *testing.T, events []cassette.Event) []byte {
	t.Helper()
	var buf bytes.Buffer
	for _, e := range events {
		row := map[string]any{
			"seq": e.Seq, "parent_seq": e.ParentSeq, "interaction_id": e.InteractionID, "kind": e.Kind,
			"source": e.Source, "target": e.Target, "started_at": cassette.FormatTime(e.StartedAt),
			"duration_ms": e.DurationMillis, "model": e.Model, "requested_model": e.RequestedModel,
			"tool": e.Tool, "arguments": e.Arguments, "result": e.Result,
			"tokens_in": e.TokensIn, "tokens_out": e.TokensOut,
			"tokens_cache_read": e.TokensCacheRead, "tokens_cache_write": e.TokensCacheWrite,
			"cost_usd": e.CostUSD, "status": e.Status, "error": e.Error,
			"provenance": e.Provenance, "deriver_version": e.DeriverVersion,
		}
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

func checkGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // G304: a test fixture path
	if err != nil {
		t.Fatalf("%v (run go test ./internal/derive -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("events differ from %s (rerun with -update if the change is intended)\ngot:\n%s\nwant:\n%s", path, got, want)
	}
}

func TestIsMessages(t *testing.T) {
	tests := []struct {
		in   cassette.Interaction
		want bool
	}{
		{cassette.Interaction{Channel: "llm", Upstream: "anthropic", HTTPMethod: "POST", Path: "/v1/messages"}, true},
		{cassette.Interaction{Channel: "llm", Upstream: "anthropic", HTTPMethod: "POST", Path: "/v1/messages?beta=true"}, true},
		{cassette.Interaction{Channel: "llm", Upstream: "anthropic", HTTPMethod: "POST", Path: "/v1/%6Dessages"}, true},
		{cassette.Interaction{Channel: "llm", Upstream: "anthropic", HTTPMethod: "POST", Path: "/v1/messages/batches"}, false},
		{cassette.Interaction{Channel: "llm", Upstream: "anthropic", HTTPMethod: "GET", Path: "/v1/messages"}, false},
		{cassette.Interaction{Channel: "mcp", Upstream: "mcp:x", HTTPMethod: "POST", Path: "/v1/messages"}, false},
	}
	for _, tt := range tests {
		if got := isMessages(tt.in); got != tt.want {
			t.Errorf("isMessages(%s %s) = %v, want %v", tt.in.HTTPMethod, tt.in.Path, got, tt.want)
		}
	}
}

// The same text later in a conversation is a new user_message (spec §7.4).
func TestUserMessageRepeatedLaterIsNew(t *testing.T) {
	first := `{"model":"m","messages":[{"role":"user","content":"continue"}]}`
	later := `{"model":"m","messages":[{"role":"user","content":"continue"},{"role":"assistant","content":"ok"},{"role":"user","content":"continue"}]}`
	resp := `{"type":"message","model":"m","content":[],"stop_reason":"end_turn"}`
	c := record(t, []exchange{
		{request: first, status: 200, response: resp, durationMillis: 1},
		{startMillis: 10, request: later, status: 200, response: resp, durationMillis: 1},
	})
	events, _ := c.Events(context.Background())
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	if got := strings.Join(kinds, ","); got != "user_message,model_response,user_message,model_response" {
		t.Errorf("kinds = %s", got)
	}
}
