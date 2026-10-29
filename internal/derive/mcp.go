package derive

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/mcp"
)

// mcpCall is what a POST on the MCP channel contributes until it completes:
// its JSON-RPC requests and the responses of its streamed response.
type mcpCall struct {
	in       cassette.Interaction
	requests []mcpRequest
	stream   []mcp.Message
}

// mcpRequest is a JSON-RPC request of a POST, waiting for its response.
type mcpRequest struct {
	msg      mcp.Message // always of Kind mcp.Request
	toolCall int64       // the seq of its tool_call, or 0 when it is not a tools/call
	tool     string      // the tool name of a tools/call, for its tool_result
}

// isMCPPost reports whether in is a POST on the MCP channel, the only
// method that carries client messages (ADR 0004). GET and DELETE derive no
// events.
func isMCPPost(in cassette.Interaction) bool {
	return in.Channel == "mcp" && in.HTTPMethod == "POST"
}

// beginMCP derives the events of the messages in a POST body, in body order:
// a tool_call for each tools/call request, linked to its tool_use when one
// qualifies, and an mcp_message for every other request or notification
// (spec §7.4). Client responses, invalid batch
// elements and bodies that are not JSON-RPC derive nothing.
func (d *Deriver) beginMCP(in cassette.Interaction, request []byte, emit cassette.EmitFunc) {
	body, err := mcp.ParseBody(request)
	if err != nil {
		return // not JSON-RPC, or not stored: nil is not a message either
	}
	call := &mcpCall{in: in}
	toolCalls := false
	for _, m := range body.Messages {
		switch {
		case m.Kind == mcp.Request && m.Method == mcp.MethodToolsCall:
			tc, _ := mcp.ParseToolCall(m.Params) // params that are not an object leave both empty
			seq := emit(cassette.Event{
				ParentSeq: d.links.link(tc.Name, tc.Arguments), InteractionID: ptr(in.ID), Kind: "tool_call", Source: "agent", Target: in.Upstream,
				StartedAt: in.StartedAt, Tool: nonEmpty(tc.Name), Arguments: compact(tc.Arguments),
				Status: "ok", Provenance: in.Provenance, DeriverVersion: Version,
			})
			call.requests = append(call.requests, mcpRequest{msg: m, toolCall: seq, tool: tc.Name})
			toolCalls = true
		case m.Kind == mcp.Request || m.Kind == mcp.Notification:
			emit(cassette.Event{
				InteractionID: ptr(in.ID), Kind: "mcp_message", Source: "agent", Target: in.Upstream,
				StartedAt: in.StartedAt, Arguments: mustJSON(map[string]string{"method": m.Method}),
				Status: "ok", Provenance: in.Provenance, DeriverVersion: Version,
			})
			if m.Kind == mcp.Request {
				// Paired too, so a response to it never answers a tools/call
				// that reuses its id.
				call.requests = append(call.requests, mcpRequest{msg: m})
			}
		}
	}
	if toolCalls {
		d.mcpCalls[in.ID] = call
	}
}

// chunk adds the responses in one SSE event of a streamed response. An
// event that is not JSON-RPC is skipped, as in mcp.StreamMessages, and so
// are the requests and notifications the server sends, which answer nothing.
func (c *mcpCall) chunk(data []byte) {
	msgs, err := mcp.EventMessages(data)
	if err != nil {
		return
	}
	for _, m := range msgs {
		if m.Kind == mcp.Response {
			c.stream = append(c.stream, m)
		}
	}
}

// complete derives a tool_result for each tools/call of the interaction,
// in request order, paired with its response by JSON-RPC id (spec §3.4,
// §7.4). Responses to other requests, and the requests and notifications the
// server sends in an SSE response, derive no events.
func (c *mcpCall) complete(done cassette.Completion, response []byte, emit cassette.EmitFunc) {
	responses, parseErr := c.responses(done, response)
	msgs := make([]mcp.Message, len(c.requests))
	for i, r := range c.requests {
		msgs[i] = r.msg
	}
	// Every message is a request, so Pairs[i] answers c.requests[i].
	pairing := mcp.PairMessages(msgs, responses)
	for i, r := range c.requests {
		if r.toolCall == 0 {
			continue
		}
		result, errMsg := toolResult(done, pairing.Pairs[i].Response, parseErr)
		e := cassette.Event{
			ParentSeq: ptr(r.toolCall), InteractionID: ptr(c.in.ID), Kind: "tool_result",
			Source: c.in.Upstream, Target: "agent", StartedAt: c.in.StartedAt,
			DurationMillis: ptr(done.DurationMillis), Tool: nonEmpty(r.tool), Result: result,
			Status: "ok", Provenance: c.in.Provenance, DeriverVersion: Version,
		}
		if errMsg != "" {
			e.Status, e.Error = "error", &errMsg
		}
		emit(e)
	}
}

// responses returns the JSON-RPC messages of the response, or an error when
// a stored JSON body is not JSON-RPC.
func (c *mcpCall) responses(done cassette.Completion, response []byte) ([]mcp.Message, error) {
	if done.Streamed {
		return c.stream, nil
	}
	if len(response) == 0 {
		// An empty body is nil or empty depending on who read it back, and
		// holds no message either way.
		return nil, nil
	}
	body, err := mcp.ParseBody(response)
	return body.Messages, err
}

// toolResult returns the result of a tool_result and why the call failed, or
// "" (spec §7.4). A JSON-RPC error gives `<code>: <message>`, and a result
// with isError gives the text of its content. An incomplete recording is an
// error even when the response was stored, as for model_response. Any other
// interaction error, such as the agent closing an SSE stream after its
// answer, does not undo a response that was received.
func toolResult(done cassette.Completion, resp *mcp.Message, parseErr error) (json.RawMessage, string) {
	switch {
	case resp != nil && resp.Error != nil:
		return nil, rpcError(resp.Error)
	case done.Incomplete():
		var result json.RawMessage
		if resp != nil {
			result = compact(resp.Result)
		}
		return result, *done.Error
	case resp != nil:
		tr, err := mcp.ParseToolResult(resp.Result)
		if err == nil && tr.IsError {
			return compact(resp.Result), toolErrorText(tr.Content)
		}
		return compact(resp.Result), "" // a result that is not an object, e.g. redacted, is kept as stored
	}
	return nil, missingResponse(done, parseErr)
}

// missingResponse returns why a tools/call has no response in the cassette.
func missingResponse(done cassette.Completion, parseErr error) string {
	switch {
	case done.Error != nil:
		return *done.Error
	case done.StatusCode == nil:
		return "no response"
	case *done.StatusCode >= 400:
		return "HTTP " + strconv.Itoa(*done.StatusCode)
	case parseErr != nil:
		return "unrecognized response body"
	case done.Streamed:
		return "the stream ended before the response to this request"
	case done.ResponseBlob == nil:
		return "the response body was not recorded"
	}
	return "the response has no message with this request's id"
}

// rpcError formats a JSON-RPC error as `<code>: <message>`, like an API
// error of model_response.
func rpcError(obj json.RawMessage) string {
	e, err := mcp.ParseError(obj)
	if err != nil {
		return "unrecognized JSON-RPC error"
	}
	return strconv.FormatInt(e.Code, 10) + ": " + e.Message
}

// toolErrorText returns the text blocks of an isError result's content,
// joined with "\n", or a generic message when it has none.
func toolErrorText(content json.RawMessage) string {
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(content, &blocks) // not an array of blocks: no text
	var texts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			texts = append(texts, b.Text)
		}
	}
	if len(texts) == 0 {
		return "the tool reported an error"
	}
	return strings.Join(texts, "\n")
}

// compact returns v without insignificant whitespace, or nil when it is
// absent. v comes from a parsed body, so it is valid JSON.
func compact(v json.RawMessage) json.RawMessage {
	if v == nil {
		return nil
	}
	var b bytes.Buffer
	if err := json.Compact(&b, v); err != nil {
		return v
	}
	return b.Bytes()
}
