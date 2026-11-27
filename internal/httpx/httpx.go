// Package httpx holds the HTTP helpers that recording and replay share: the
// request body limit, the rejection answers of spec §3.6 and the answer to a
// failed upstream request. It imports neither the proxy nor replay.
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
)

// MaxRequestBytes is the largest request body retrace accepts, 64 MiB (spec
// §3.6). It is also the default limit for one stored response body or SSE
// event (spec §3.5, Config.MaxBodyBytes), and is not configurable in the MVP.
const MaxRequestBytes = 64 << 20

// RouteLLM is the LLM route of a rejected request, as its log warning names
// it. Reject gives it the Anthropic API's error shape.
const RouteLLM = "llm"

// RouteMCP is the MCP route of a rejected request, as its log warning names
// it. Reject answers it in plain text.
const RouteMCP = "mcp"

var reasonTooLarge = fmt.Sprintf("the request body is larger than %d MiB (%d bytes)", MaxRequestBytes>>20, MaxRequestBytes)

// ReadBody reads r's body, up to MaxRequestBytes. When it cannot, it answers
// r itself and returns false: 413 for a larger body (spec §3.6), whether or
// not r has a Content-Length, and 400 when the body cannot be read. A
// Content-Length above the limit is rejected before anything is read. channel
// is the route r arrived on, RouteLLM or RouteMCP, and log must not be nil.
func ReadBody(w http.ResponseWriter, r *http.Request, log *slog.Logger, channel string) ([]byte, bool) {
	if r.ContentLength > MaxRequestBytes {
		Reject(w, log, channel, http.StatusRequestEntityTooLarge, reasonTooLarge)
		return nil, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxRequestBytes))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		Reject(w, log, channel, http.StatusRequestEntityTooLarge, reasonTooLarge)
		return nil, false
	case err != nil:
		// Not a rejection of spec §3.6: the agent sent a broken body or left,
		// so plain text on every route, as before the limit existed.
		http.Error(w, "retrace: reading request body: "+err.Error(), http.StatusBadRequest)
		return nil, false
	}
	return body, true
}

// Reject answers a request that retrace refuses to forward or match, and logs
// why (spec §3.6). The warning leaves out the request's path and query: they
// may carry a credential, and nothing redacts them because the request is not
// recorded. On the LLM route the body has the Anthropic API's error shape, so
// SDK clients show it; elsewhere it is plain text.
func Reject(w http.ResponseWriter, log *slog.Logger, route string, status int, reason string) {
	attrs := []any{"status", status, "reason", reason}
	if route != "" {
		attrs = append([]any{"route", route}, attrs...)
	}
	log.Warn("request rejected", attrs...)

	msg := "retrace: " + reason
	if route != RouteLLM {
		http.Error(w, msg, status)
		return
	}
	typ := "invalid_request_error"
	switch status {
	case http.StatusForbidden:
		typ = "permission_error"
	case http.StatusRequestEntityTooLarge:
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

// UpstreamFailed answers a request whose upstream request failed with no
// response: 502 and a plain-text body that includes msg. Replay answers an
// interaction recorded with a transport error the same way, so the agent gets
// what it got while recording (spec §5 step 2).
func UpstreamFailed(w http.ResponseWriter, msg string) {
	http.Error(w, "retrace: upstream request failed: "+msg, http.StatusBadGateway)
}
