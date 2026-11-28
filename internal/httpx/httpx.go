// Package httpx holds the HTTP helpers that recording and replay share: the
// request body limit, the rejection answers of spec §3.6 and the answer to a
// failed upstream request. It imports neither the proxy nor replay.
package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"
)

// MaxRequestBytes is the largest request body retrace accepts, 64 MiB (spec
// §3.6). It is also the default limit for one stored response body or SSE
// event (spec §3.5, Config.MaxBodyBytes), and is not configurable in the MVP.
const MaxRequestBytes = 64 << 20

// BodyReadTimeout is how long retrace waits for each next part of a request
// body, 60 seconds (spec §3.6). It is not configurable in the MVP; tests
// shorten it with WithBodyReadTimeout.
const BodyReadTimeout = 60 * time.Second

type bodyTimeoutKey struct{}

// WithBodyReadTimeout returns a context that makes ReadBody wait d, instead of
// BodyReadTimeout, for each next part of a body. It is the internal option
// tests use; d <= 0 keeps the default. It travels in the context only because
// ReadBody, called by record and by every replay route, has no configuration
// parameter.
func WithBodyReadTimeout(ctx context.Context, d time.Duration) context.Context {
	return context.WithValue(ctx, bodyTimeoutKey{}, d)
}

func bodyReadTimeout(ctx context.Context) time.Duration {
	if d, _ := ctx.Value(bodyTimeoutKey{}).(time.Duration); d > 0 {
		return d
	}
	return BodyReadTimeout
}

// RouteLLM is the LLM route of a rejected request, as its log warning names
// it. Reject gives it the Anthropic API's error shape.
const RouteLLM = "llm"

// RouteMCP is the MCP route of a rejected request, as its log warning names
// it. Reject answers it in plain text.
const RouteMCP = "mcp"

const reasonBodyStalled = "the request body stopped arriving"

var reasonTooLarge = fmt.Sprintf("the request body is larger than %d MiB (%d bytes)", MaxRequestBytes>>20, MaxRequestBytes)

// ReadBody reads r's body, up to MaxRequestBytes. When it cannot, it answers
// r itself and returns false: 413 for a larger body (spec §3.6), whether or
// not r has a Content-Length, 408 when the body stops arriving for
// BodyReadTimeout, and 400 when the body cannot be read. A
// Content-Length above the limit is rejected before anything is read. channel
// is the route r arrived on, RouteLLM or RouteMCP, and log must not be nil.
func ReadBody(w http.ResponseWriter, r *http.Request, log *slog.Logger, channel string) ([]byte, bool) {
	if r.ContentLength > MaxRequestBytes {
		Reject(w, log, channel, http.StatusRequestEntityTooLarge, reasonTooLarge)
		return nil, false
	}
	src := r.Body
	liftDeadline := func() {}
	if r.Body != nil && r.Body != http.NoBody {
		// The deadline is for inactivity: it is renewed before each read, and
		// lifted once the body is read, so a long response is never cut
		// (spec §3.6, AC-03). A writer without deadline support, as in unit
		// tests, reads without one.
		rc := http.NewResponseController(w)
		src = readCloser{&deadlineReader{r: r.Body, rc: rc, d: bodyReadTimeout(r.Context())}, r.Body}
		liftDeadline = func() { _ = rc.SetReadDeadline(time.Time{}) }
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, src, MaxRequestBytes))
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		liftDeadline()
	}
	var tooLarge *http.MaxBytesError
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded):
		// Bytes of the unread body may still arrive: close the connection so
		// they are not read as the next request. The expired deadline stays,
		// so the server's drain of the unread body fails at once instead of
		// waiting for bytes that may never come.
		w.Header().Set("Connection", "close")
		Reject(w, log, channel, http.StatusRequestTimeout, reasonBodyStalled)
		return nil, false
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

// deadlineReader renews the connection's read deadline before each read.
type deadlineReader struct {
	r  io.Reader
	rc *http.ResponseController
	d  time.Duration
}

func (b *deadlineReader) Read(p []byte) (int, error) {
	_ = b.rc.SetReadDeadline(time.Now().Add(b.d)) // unsupported: no deadline
	return b.r.Read(p)
}

type readCloser struct {
	io.Reader
	io.Closer
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
	case http.StatusServiceUnavailable:
		typ = "overloaded_error"
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
