package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"strings"
)

// MaxRequestBytes is the largest request body retrace accepts, 64 MiB (spec
// §3.6). It is also the default limit for one stored response body or SSE
// event (spec §3.5, Config.MaxBodyBytes), and is not configurable in the MVP.
const MaxRequestBytes = 64 << 20

// The routes a rejected request can be on, as its log warning names them.
const (
	routeLLM = "llm"
	routeMCP = "mcp"
)

const (
	reasonHost    = "the request's host does not name the address retrace listens on"
	reasonBrowser = "the request has an Origin or Sec-Fetch-Site header, which a browser sends"
)

const reasonDotSegment = "the request path has a dot segment (. or ..), which could leave its route"

var reasonTooLarge = fmt.Sprintf("the request body is larger than %d MiB (%d bytes)", MaxRequestBytes>>20, MaxRequestBytes)

// Guard answers a request retrace refuses before any router sees it, and
// passes every other request to next (spec §3.6). The checks run in this
// order, and the first that fails decides the answer: the host (403), the
// browser headers Origin and Sec-Fetch-Site (403), a path with a dot segment
// (400). The body-size check (413) is ReadBody's. A rejected request is
// neither forwarded nor recorded.
//
// listenHosts are the host parts, without brackets, that name the address
// retrace listens on (the --listen host and the host of the listener's
// address). "localhost" and any IP literal are always accepted, and an empty
// host never is. The port is not compared.
//
// Dot segments must be refused before routing: http.ServeMux cleans such a
// path and answers with a redirect, and a reverse proxy would forward it
// outside the route. The path is the one the agent sent, without the query,
// percent-decoded once, as the HTTP server leaves it in r.URL.Path. The
// server itself refuses a request line with an invalid percent-encoding. log
// must not be nil.
func Guard(next http.Handler, log *slog.Logger, listenHosts ...string) http.Handler {
	names := make([]string, 0, len(listenHosts))
	for _, h := range listenHosts {
		if strings.Contains(h, ":") {
			h = "[" + h + "]" // as a Host header carries an IPv6 address
		}
		names = append(names, strings.ToLower(h))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case !hostAllowed(r.Host, names):
			reject(w, log, routeOf(r.URL.Path), http.StatusForbidden, reasonHost)
		case hasBrowserHeader(r.Header):
			reject(w, log, routeOf(r.URL.Path), http.StatusForbidden, reasonBrowser)
		case hasDotSegment(r.URL.Path):
			reject(w, log, routeOf(r.URL.Path), http.StatusBadRequest, reasonDotSegment)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// hostAllowed reports whether hostport, a Host header or the authority of an
// absolute request target, names this proxy (spec §3.6). names are the
// lower-case listen hosts, IPv6 in brackets. Only the ASCII case is folded:
// "localhost." is not "localhost".
func hostAllowed(hostport string, names []string) bool {
	host, ok := splitHost(hostport)
	if !ok || host == "" {
		return false
	}
	host = strings.ToLower(host)
	if host == "localhost" || slices.Contains(names, host) {
		return true
	}
	return isIPLiteral(host)
}

// splitHost returns the host part of hostport, an IPv6 address keeping its
// brackets, and false when hostport is not a host with an optional numeric
// port.
func splitHost(hostport string) (string, bool) {
	host, port := hostport, ""
	if strings.HasPrefix(hostport, "[") {
		end := strings.IndexByte(hostport, ']')
		if end < 0 {
			return "", false
		}
		host, port = hostport[:end+1], hostport[end+1:]
		if port != "" && port[0] != ':' {
			return "", false
		}
	} else if i := strings.IndexByte(hostport, ':'); i >= 0 {
		host, port = hostport[:i], hostport[i:]
	}
	for _, c := range strings.TrimPrefix(port, ":") {
		if c < '0' || c > '9' {
			return "", false
		}
	}
	return host, true
}

// isIPLiteral reports whether host is an IPv4 address in dotted-decimal form
// without leading zeros, or an IPv6 address in brackets without a zone. Such a
// host cannot carry DNS rebinding.
func isIPLiteral(host string) bool {
	if inner, ok := strings.CutPrefix(host, "["); ok {
		// splitHost keeps the closing bracket, so TrimSuffix always trims it.
		a, err := netip.ParseAddr(strings.TrimSuffix(inner, "]"))
		return err == nil && a.Is6() && a.Zone() == ""
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.Is4()
}

// hasBrowserHeader reports whether h has an Origin or a Sec-Fetch-Site
// header, whatever its value. Other Sec-Fetch-* headers are not checked:
// Node's fetch sends Sec-Fetch-Mode on every request.
func hasBrowserHeader(h http.Header) bool {
	_, origin := h["Origin"]
	_, site := h["Sec-Fetch-Site"]
	return origin || site
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
// is the route r arrived on, "llm" or "mcp", and log must not be nil.
func ReadBody(w http.ResponseWriter, r *http.Request, log *slog.Logger, channel string) ([]byte, bool) {
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
		// Not a rejection of spec §3.6: the agent sent a broken body or left,
		// so plain text on every route, as before the limit existed.
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
