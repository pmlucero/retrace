package proxy

import (
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/pmlucero/retrace/internal/httpx"
)

// The routes a rejected request can be on, as its log warning names them.
const (
	routeLLM = httpx.RouteLLM
	routeMCP = httpx.RouteMCP
)

const (
	reasonHost    = "the request's host does not name the address retrace listens on"
	reasonBrowser = "the request has an Origin or Sec-Fetch-Site header, which a browser sends"
)

// expiredDeadline is a read deadline that has always passed.
var expiredDeadline = time.Unix(1, 0)

const reasonBusy = "too many requests are in flight"

const reasonDotSegment = "the request path has a dot segment (. or ..), which could leave its route"

// Guard answers a request retrace refuses before any router sees it, and
// passes every other request to next (spec §3.6). The checks run in this
// order, and the first that fails decides the answer: the host (403), the
// browser headers Origin and Sec-Fetch-Site (403), a path with a dot segment
// (400), the in-flight limit (503). The body-size check (413) and the
// body-read deadline (408) are httpx.ReadBody's. A rejected request is
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
	return GuardLimits(next, log, Limits{}, listenHosts...)
}

// GuardLimits is Guard with the connection limits of spec §3.6: after the
// path check, a request takes one of limits.MaxInFlight places until its
// response ends, or it is answered 503 with Connection: close, without its
// body being read. The body-read deadline travels with the request to
// httpx.ReadBody.
func GuardLimits(next http.Handler, log *slog.Logger, limits Limits, listenHosts ...string) http.Handler {
	limits = limits.withDefaults()
	slots := make(chan struct{}, limits.MaxInFlight)
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
			httpx.Reject(w, log, routeOf(r.URL.Path), http.StatusForbidden, reasonHost)
		case hasBrowserHeader(r.Header):
			httpx.Reject(w, log, routeOf(r.URL.Path), http.StatusForbidden, reasonBrowser)
		case hasDotSegment(r.URL.Path):
			httpx.Reject(w, log, routeOf(r.URL.Path), http.StatusBadRequest, reasonDotSegment)
		default:
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			default:
				// Bytes of the unread body may still arrive.
				w.Header().Set("Connection", "close")
				httpx.Reject(w, log, routeOf(r.URL.Path), http.StatusServiceUnavailable, reasonBusy)
				// The body is not read, and the server would wait for the
				// rest of it before closing: an expired deadline ends that.
				_ = http.NewResponseController(w).SetReadDeadline(expiredDeadline)
				return
			}
			next.ServeHTTP(w, r.WithContext(httpx.WithBodyReadTimeout(r.Context(), limits.BodyReadTimeout)))
		}
	})
}

// hostAllowed reports whether hostport, a Host header or the authority of an
// absolute request target, names this proxy (spec §3.6). names are the
// lower-case listen hosts, IPv6 in brackets. Names are compared with no other
// normalization, so "localhost." is not "localhost". The HTTP server refuses
// non-ASCII Host bytes before this runs, so lower-casing is plain ASCII.
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
