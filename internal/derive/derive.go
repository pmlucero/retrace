// Package derive builds the semantic layer of a cassette (spec §7.4, ADR
// 0009): the events derived from its interactions. The same Deriver runs at
// record time, on the cassette writer's goroutine, and in `retrace reindex`
// (spec §7.6), so both produce the same events from the same stored bytes.
package derive

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/provider/anthropic"
)

// Version is the version of the derivation logic, stored in each event and in
// the session (spec §7.2, §7.4). Bump it whenever the events derived from the
// same interactions change, so cassettes can be told apart and reindexed.
// Version 2 adds the MCP events, and version 3 links each tool_call to its
// tool_use.
const Version = 3

// Deriver derives LLM and MCP events (spec §7.4). It implements
// cassette.Deriver. It is not safe for concurrent use: the cassette writer
// and reindex each call it from one goroutine.
type Deriver struct {
	calls    map[int64]*llmCall // Messages API interactions not yet complete, by id
	mcpCalls map[int64]*mcpCall // MCP POSTs with tools/call requests not yet complete, by id
	// userMessages maps a user message's key to the seq of its user_message
	// event, so it is emitted once (spec §7.4). It grows by one entry per
	// distinct user message of the session.
	userMessages map[string]int64
	links        toolLinks
}

// llmCall is what a Messages API interaction contributes until it completes.
type llmCall struct {
	in      cassette.Interaction
	model   string // requested model
	userSeq *int64 // the user_message of its trailing user message
	stream  anthropic.Stream
}

// New returns a Deriver for one session.
func New() *Deriver {
	return &Deriver{calls: map[int64]*llmCall{}, mcpCalls: map[int64]*mcpCall{}, userMessages: map[string]int64{}}
}

var _ cassette.Deriver = (*Deriver)(nil)

// isMessages reports whether in is a Messages API call: POST /v1/messages on
// the LLM channel, whatever its query string (the SDKs add ?beta=true).
func isMessages(in cassette.Interaction) bool {
	if in.Channel != "llm" || in.Upstream != "anthropic" || in.HTTPMethod != "POST" {
		return false
	}
	path, _, _ := strings.Cut(in.Path, "?")
	if p, err := url.PathUnescape(path); err == nil {
		path = p
	}
	return path == "/v1/messages"
}

// Begin derives the events of a request: the user_message of a Messages API
// call, once per distinct message, or the events of an MCP POST's messages.
func (d *Deriver) Begin(in cassette.Interaction, request []byte, emit cassette.EmitFunc) {
	if isMCPPost(in) {
		d.beginMCP(in, request, emit)
		return
	}
	if !isMessages(in) {
		return
	}
	call := &llmCall{in: in}
	d.calls[in.ID] = call
	if request == nil {
		return
	}
	req, err := anthropic.ParseRequest(request)
	if err != nil {
		return // not JSON: there is no message to derive, and the response still gets its event
	}
	call.model = req.Model
	if !req.HasUserText {
		return
	}
	if seq, ok := d.userMessages[req.UserKey]; ok {
		call.userSeq = &seq
		return
	}
	seq := emit(cassette.Event{
		InteractionID: ptr(in.ID), Kind: "user_message", Source: "user", Target: "model",
		StartedAt: in.StartedAt, Result: mustJSON(map[string]string{"text": req.UserText}),
		Status: "ok", Provenance: in.Provenance, DeriverVersion: Version,
	})
	d.userMessages[req.UserKey] = seq
	call.userSeq = &seq
}

// Chunk adds one SSE event of a streamed response.
func (d *Deriver) Chunk(ch cassette.Chunk) {
	if call := d.calls[ch.InteractionID]; call != nil {
		call.stream.Feed(ch.Data)
	} else if call := d.mcpCalls[ch.InteractionID]; call != nil {
		call.chunk(ch.Data)
	}
}

// Complete derives the events of a response: the model_response of a
// Messages API call and its tool_use events, or the tool_result of each
// tools/call of an MCP POST.
func (d *Deriver) Complete(id int64, done cassette.Completion, response []byte, emit cassette.EmitFunc) {
	if call := d.mcpCalls[id]; call != nil {
		delete(d.mcpCalls, id)
		call.complete(done, response, emit)
		return
	}
	call := d.calls[id]
	if call == nil {
		return
	}
	delete(d.calls, id)

	msg, parseErr := call.message(done, response)
	errMsg := responseError(done, msg, parseErr)
	ended := call.in.StartedAt.Add(time.Duration(done.DurationMillis) * time.Millisecond)

	e := cassette.Event{
		ParentSeq: call.userSeq, InteractionID: ptr(id), Kind: "model_response",
		Source: "model", Target: "agent", StartedAt: call.in.StartedAt,
		DurationMillis: ptr(done.DurationMillis), Model: nonEmpty(msg.Model), RequestedModel: nonEmpty(call.model),
		Result: modelResult(msg), Status: "ok", Provenance: call.in.Provenance, DeriverVersion: Version,
	}
	if errMsg != "" {
		e.Status, e.Error = "error", &errMsg
	}
	if u := msg.Usage; u != nil {
		e.TokensIn, e.TokensOut = u.InputTokens, u.OutputTokens
		e.TokensCacheRead, e.TokensCacheWrite = u.CacheReadInputTokens, u.CacheCreationInputTokens
		e.CostUSD = anthropic.Cost(msg.Model, u)
	}
	parent := emit(e)

	for _, tu := range msg.ToolUses {
		seq := emit(cassette.Event{
			ParentSeq: ptr(parent), InteractionID: ptr(id), Kind: "tool_use", Source: "model", Target: "agent",
			StartedAt: ended, Tool: ptr(tu.Name), Arguments: tu.Input,
			Status: "ok", Provenance: call.in.Provenance, DeriverVersion: Version,
		})
		d.links.add(seq, tu.Name, tu.Input)
	}
}

// message returns the response as a Message, or an error when a stored JSON
// body is not one.
func (c *llmCall) message(done cassette.Completion, response []byte) (anthropic.Message, error) {
	if done.Streamed {
		return c.stream.Message(), nil
	}
	if response == nil {
		return anthropic.Message{}, nil
	}
	return anthropic.ParseResponse(response)
}

// responseError returns why a response is not a successful model response,
// or "" (spec §7.4).
func responseError(done cassette.Completion, msg anthropic.Message, parseErr error) string {
	switch {
	case msg.Error != "":
		return msg.Error
	case done.Error != nil:
		return *done.Error
	case done.StatusCode == nil:
		return "no response"
	case *done.StatusCode >= 400:
		return "HTTP " + strconv.Itoa(*done.StatusCode)
	case parseErr != nil:
		return "unrecognized response body"
	case done.Streamed && !msg.Complete:
		return "the stream ended before message_stop"
	case !done.Streamed && done.ResponseBlob == nil:
		return "the response body was not recorded"
	}
	return ""
}

// modelOutput is the result of a model_response (spec §7.4).
type modelOutput struct {
	StopReason string `json:"stop_reason,omitempty"`
	Text       string `json:"text,omitempty"`
}

// modelResult returns the result of a model_response, or nil when the
// response has neither a stop reason nor text.
func modelResult(msg anthropic.Message) json.RawMessage {
	if msg.StopReason == "" && msg.Text == "" {
		return nil
	}
	return mustJSON(modelOutput{StopReason: msg.StopReason, Text: msg.Text})
}

// mustJSON encodes v, which is always encodable, without HTML escaping.
func mustJSON(v any) json.RawMessage {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic(err) // impossible: only strings and structs of strings are encoded
	}
	return json.RawMessage(strings.TrimSuffix(b.String(), "\n"))
}

func ptr[T any](v T) *T { return &v }

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
