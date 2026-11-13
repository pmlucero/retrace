package proxy

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// MaxRequestBytes is the largest request body retrace accepts (spec §3.6).
// It is the proxy's limit for one stored response body or SSE event (spec
// §3.5), and is not configurable in the MVP.
const MaxRequestBytes = 64 << 20

// The routes a rejected request can be on, as its log warning names them.
const (
	routeLLM = "llm"
	routeMCP = "mcp"
)

const (
	reasonDotSegment = "the request path has a dot segment (. or ..), which could leave its route"
	reasonTooLarge   = "the request body is larger than 64 MiB (67108864 bytes)"
)

// Guard answers 400 to a request whose path has a dot segment, and passes
// every other request to next (spec §3.6). It must run before any router:
// http.ServeMux cleans such a path and answers with a redirect, and a
// reverse proxy would forward it outside the route. A rejected request is
// neither forwarded nor recorded.
//
// The path is the one the agent sent, without the query, percent-decoded
// once, as the HTTP server leaves it in r.URL.Path. The server itself
// refuses a request line with an invalid percent-encoding.
func Guard(next http.Handler, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hasDotSegment(r.URL.Path) {
			reject(w, log, routeOf(r.URL.Path), http.StatusBadRequest, reasonDotSegment)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hasDotSegment reports whether path has a "." or ".." segment, splitting it
// on "/" and on "\", which some servers also read as a separator.
func hasDotSegment(path string) bool {
	for _, seg := range strings.FieldsFunc(path, func(c rune) bool { return c == '/' || c == '\\' }) {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

// routeOf returns the route of a rejected request: the prefix of the path
// the agent sent, before any cleaning, so /mcp/../llm/anthropic is on the
// MCP route (spec §3.6). It is empty outside both routes.
func routeOf(path string) string {
	switch {
	case path == "/llm/anthropic" || strings.HasPrefix(path, "/llm/anthropic/"):
		return routeLLM
	case strings.HasPrefix(path, "/mcp/"):
		return routeMCP
	}
	return ""
}

// ReadBody reads r's body, up to MaxRequestBytes. When it cannot, it answers
// r itself and returns false: 413 for a larger body (spec §3.6), whether or
// not r has a Content-Length, and 400 when the body cannot be read. A
// Content-Length above the limit is rejected before anything is read. channel
// is the route r arrived on, "llm" or "mcp".
func ReadBody(w http.ResponseWriter, r *http.Request, log *slog.Logger, channel string) ([]byte, bool) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if r.ContentLength > MaxRequestBytes {
		reject(w, log, channel, http.StatusRequestEntityTooLarge, reasonTooLarge)
		return nil, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxRequestBytes))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		reject(w, log, channel, http.StatusRequestEntityTooLarge, reasonTooLarge)
		return nil, false
	case err != nil:
		http.Error(w, "retrace: reading request body: "+err.Error(), http.StatusBadRequest)
		return nil, false
	}
	return body, true
}

// reject answers a request that retrace refuses to forward or match, and logs
// why (spec §3.6). The warning leaves out the request's path and query: they
// may carry a credential, and nothing redacts them because the request is not
// recorded. On the LLM route the body has the Anthropic API's error shape, so
// SDK clients show it; elsewhere it is plain text.
func reject(w http.ResponseWriter, log *slog.Logger, route string, status int, reason string) {
	attrs := []any{"status", status, "reason", reason}
	if route != "" {
		attrs = append([]any{"route", route}, attrs...)
	}
	log.Warn("request rejected", attrs...)

	msg := "retrace: " + reason
	if route != routeLLM {
		http.Error(w, msg, status)
		return
	}
	typ := "invalid_request_error"
	if status == http.StatusRequestEntityTooLarge {
		typ = "request_too_large"
	}
	var body apiError
	body.Type, body.Error.Type, body.Error.Message = "error", typ, msg
	b, _ := json.Marshal(body) // strings always marshal
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(b) // the agent may be gone; nothing is recorded
}

// apiError is the Anthropic API's error shape.
type apiError struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}
