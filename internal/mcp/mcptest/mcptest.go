// Package mcptest provides an in-process fake MCP server that speaks the
// Streamable HTTP transport (ADR 0004), for record, replay and CLI tests that
// must not reach real MCP servers.
//
// The server answers initialize, ping, tools/list and tools/call. A POST body
// may hold one JSON-RPC message or a batch. Requests get their responses as
// application/json or as a text/event-stream, notifications and client
// responses get 202 Accepted, GET gets 405 and DELETE ends the session with
// 200. One server can hold several sessions at once. Every
// HTTP request it receives is kept, so tests can assert on what reached it.
package mcptest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Protocol headers of the Streamable HTTP transport.
const (
	SessionHeader         = "Mcp-Session-Id"
	ProtocolVersionHeader = "MCP-Protocol-Version"
)

// DefaultProtocolVersion is the protocolVersion initialize returns when
// Config.ProtocolVersion is empty.
const DefaultProtocolVersion = "2025-06-18"

// defaultHeaderVersion is the protocol version a server assumes when a request
// has no MCP-Protocol-Version header.
const defaultHeaderVersion = "2025-03-26"

// JSON-RPC error codes the server answers with.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
)

// Mode is how the server sends the responses to a POST.
type Mode int

const (
	// JSON answers with a single application/json body: an object for a
	// single request, an array for a batch.
	JSON Mode = iota
	// SSE answers with a text/event-stream, one data event per JSON-RPC
	// message.
	SSE
)

// ToolFunc runs a tool call with the call's raw arguments. Its text becomes
// the result's content. A non-nil error becomes a tool error: a result with
// isError true and the error's text as content, not a JSON-RPC error.
type ToolFunc func(ctx context.Context, args json.RawMessage) (string, error)

// Tool is a tool listed by tools/list and served by tools/call. A tool with a
// nil Handler is list-only: tools/call answers it as an unknown tool.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Handler     ToolFunc        `json:"-"`
}

// Config configures a Server. The zero value is a server with no tools that
// answers in JSON and does not check session headers.
type Config struct {
	// ProtocolVersion is returned by initialize and, with Strict, required in
	// the MCP-Protocol-Version header of later requests.
	ProtocolVersion string
	// Name and Version are the serverInfo returned by initialize.
	Name, Version string
	Tools         []Tool
	// Mode is the response mode. MethodModes overrides it per JSON-RPC
	// method; a batch is streamed if any of its requests is.
	Mode        Mode
	MethodModes map[string]Mode
	// LogEvents is how many notifications/message events precede each
	// response in an SSE stream, so a response spans several events.
	LogEvents int
	// EventDelay is the pause before each SSE event after the first, so tests
	// can check recorded offsets.
	EventDelay time.Duration
	// Strict rejects requests after initialize that lack the session header
	// (400), carry an unknown session (404) or another protocol version (400).
	// As the transport says, a missing MCP-Protocol-Version header stands for
	// 2025-03-26, the revision that predates it.
	Strict bool
}

// Received is an HTTP request the server got.
type Received struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// Server is a running fake MCP server. Its URL and Close come from the
// embedded httptest.Server; New closes it when the test ends.
type Server struct {
	*httptest.Server
	cfg Config

	mu       sync.Mutex
	received []Received
	sessions map[string]bool
	nextID   int
}

// New starts a Server with cfg and registers its Close with tb.Cleanup.
func New(tb testing.TB, cfg Config) *Server {
	tb.Helper()
	if cfg.ProtocolVersion == "" {
		cfg.ProtocolVersion = DefaultProtocolVersion
	}
	if cfg.Name == "" {
		cfg.Name = "mcptest"
	}
	if cfg.Version == "" {
		cfg.Version = "0.0.0"
	}
	s := &Server{cfg: cfg, sessions: map[string]bool{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	tb.Cleanup(s.Close)
	return s
}

// Received returns a copy of the requests received so far, in arrival order.
func (s *Server) Received() []Received {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Received, len(s.received))
	copy(out, s.received)
	return out
}

// message is any JSON-RPC message: a request, a notification or a response.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (m *message) isRequest() bool { return m.Method != "" && len(m.ID) > 0 }

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.received = append(s.received, Received{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
	s.mu.Unlock()

	switch r.Method {
	case http.MethodPost:
		s.post(w, r, body)
	case http.MethodDelete:
		s.mu.Lock()
		id := r.Header.Get(SessionHeader)
		known := s.sessions[id]
		delete(s.sessions, id)
		s.mu.Unlock()
		if !known {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	default:
		w.Header().Set("Allow", "POST, DELETE")
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) post(w http.ResponseWriter, r *http.Request, body []byte) {
	msgs, batch, err := parse(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, message{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: err})
		return
	}
	var reqs []message
	initialize := false
	for _, m := range msgs {
		if m.isRequest() {
			reqs = append(reqs, m)
			initialize = initialize || m.Method == "initialize"
		}
	}
	if s.cfg.Strict && !initialize {
		if status := s.checkSession(r.Header); status != 0 {
			w.WriteHeader(status)
			return
		}
	}
	if len(reqs) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	resps := make([]message, len(reqs))
	stream := false
	for i, req := range reqs {
		resps[i] = s.handle(r.Context(), w.Header(), req)
		mode, ok := s.cfg.MethodModes[req.Method]
		if !ok {
			mode = s.cfg.Mode
		}
		stream = stream || mode == SSE
	}
	if stream {
		s.stream(r.Context(), w, resps)
		return
	}
	if batch {
		writeJSON(w, http.StatusOK, resps)
		return
	}
	writeJSON(w, http.StatusOK, resps[0])
}

// parse splits a POST body into its messages and reports whether it was a
// batch. Its error is the JSON-RPC error to answer with.
func parse(body []byte) (msgs []message, batch bool, _ *rpcError) {
	trimmed := bytes.TrimSpace(body)
	batch = len(trimmed) > 0 && trimmed[0] == '['
	var err error
	if batch {
		err = json.Unmarshal(trimmed, &msgs)
	} else {
		var m message
		err = json.Unmarshal(trimmed, &m)
		msgs = []message{m}
	}
	if err != nil {
		return nil, batch, &rpcError{Code: CodeParseError, Message: "parse error"}
	}
	if len(msgs) == 0 {
		return nil, batch, &rpcError{Code: CodeInvalidRequest, Message: "empty batch"}
	}
	return msgs, batch, nil
}

// checkSession returns the HTTP status that rejects a request with header h,
// or 0 if it may proceed.
func (s *Server) checkSession(h http.Header) int {
	id := h.Get(SessionHeader)
	if id == "" {
		return http.StatusBadRequest
	}
	s.mu.Lock()
	known := s.sessions[id]
	s.mu.Unlock()
	if !known {
		return http.StatusNotFound
	}
	v := h.Get(ProtocolVersionHeader)
	if v == "" {
		v = defaultHeaderVersion
	}
	if v != s.cfg.ProtocolVersion {
		return http.StatusBadRequest
	}
	return 0
}

// handle answers one JSON-RPC request. initialize sets the session header on
// respHeader.
func (s *Server) handle(ctx context.Context, respHeader http.Header, req message) message {
	resp := message{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		s.mu.Lock()
		s.nextID++
		id := fmt.Sprintf("mcptest-session-%d", s.nextID)
		s.sessions[id] = true
		s.mu.Unlock()
		respHeader.Set(SessionHeader, id)
		resp.Result = map[string]any{
			"protocolVersion": s.cfg.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": s.cfg.Name, "version": s.cfg.Version},
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		tools := make([]Tool, len(s.cfg.Tools))
		for i, t := range s.cfg.Tools {
			if t.InputSchema == nil {
				t.InputSchema = json.RawMessage(`{"type":"object"}`)
			}
			tools[i] = t
		}
		resp.Result = map[string]any{"tools": tools}
	case "tools/call":
		resp.Result, resp.Error = s.call(ctx, req.Params)
	default:
		resp.Error = &rpcError{Code: CodeMethodNotFound, Message: "method not found: " + req.Method}
	}
	return resp
}

func (s *Server) call(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: CodeInvalidParams, Message: "invalid params"}
	}
	for _, t := range s.cfg.Tools {
		if t.Name != p.Name || t.Handler == nil {
			continue
		}
		text, err := t.Handler(ctx, p.Arguments)
		isError := err != nil
		if isError {
			text = err.Error()
		}
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
			"isError": isError,
		}, nil
	}
	return nil, &rpcError{Code: CodeInvalidParams, Message: "unknown tool: " + p.Name}
}

// stream writes resps as SSE data events, each preceded by cfg.LogEvents log
// notifications, pausing cfg.EventDelay between events.
func (s *Server) stream(ctx context.Context, w http.ResponseWriter, resps []message) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	first := true
	send := func(m message) bool {
		if !first && s.cfg.EventDelay > 0 {
			select {
			case <-ctx.Done():
				return false
			case <-time.After(s.cfg.EventDelay):
			}
		}
		first = false
		data, _ := json.Marshal(m) // only marshals JSON-safe values
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	for _, resp := range resps {
		for i := range s.cfg.LogEvents {
			params, _ := json.Marshal(map[string]any{"level": "info", "data": fmt.Sprintf("event %d", i+1)})
			if !send(message{JSONRPC: "2.0", Method: "notifications/message", Params: params}) {
				return
			}
		}
		if !send(resp) {
			return
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	data, _ := json.Marshal(v) // only marshals JSON-safe values
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
