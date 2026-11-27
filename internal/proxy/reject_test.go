package proxy

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pmlucero/retrace/internal/httpx"
	"github.com/pmlucero/retrace/internal/httpx/httpxtest"
)

// AC-34: the path check of spec §3.6, on the path as the HTTP server decodes
// it (percent-decoded once, without the query).
func TestGuardRejectsDotSegments(t *testing.T) {
	const token = "AC34-QUERY-TOKEN" //nolint:gosec // G101: a fake value the log must not hold
	tests := []struct {
		target    string
		wantCode  int
		wantRoute string // "llm" selects the Anthropic error shape
	}{
		{"/mcp/orders/../x", 400, routeMCP},
		{"/mcp/orders/./x", 400, routeMCP},
		{"/mcp/orders/x/..", 400, routeMCP},
		{"/mcp/orders/%2e%2e/x", 400, routeMCP},
		{"/mcp/orders/%2E%2e/x", 400, routeMCP},
		{"/mcp/orders/..%2fx", 400, routeMCP},
		{`/mcp/orders/..\x`, 400, routeMCP},
		{"/mcp/orders/..%5cx", 400, routeMCP},
		{"/mcp/../llm/anthropic/v1/messages", 400, routeMCP}, // the raw prefix is the route
		{"/llm/anthropic/../mcp/orders", 400, routeLLM},
		{"/llm/anthropic/v1/%2e%2e/messages", 400, routeLLM},
		{"/llm/anthropic/./v1/messages", 400, routeLLM},
		{"/elsewhere/../llm/anthropic/v1/messages", 400, ""},
		// Not dot segments: passed on.
		{"/llm/anthropic/v1/messages", 200, ""},
		{"/llm/anthropic/v1/messages?next=../x", 200, ""},
		{"/mcp/orders/...", 200, ""},
		{"/mcp/orders/a..b/.x", 200, ""},
		{"/mcp/orders/%252e%252e", 200, ""}, // decoded once: "%2e%2e"
	}
	for _, tt := range tests {
		t.Run(tt.target, func(t *testing.T) {
			var logs bytes.Buffer
			log := slog.New(slog.NewTextHandler(&logs, nil))
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
			target := tt.target
			if tt.wantCode != 200 {
				target += "?token=" + token
			}
			rec := httptest.NewRecorder()
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, target, nil)
			r.Host = "127.0.0.1:4790"
			Guard(next, log, "127.0.0.1").ServeHTTP(rec, r)

			if rec.Code != tt.wantCode {
				t.Fatalf("status %d, want %d", rec.Code, tt.wantCode)
			}
			if tt.wantCode == 200 {
				if logs.Len() != 0 {
					t.Errorf("logged %q for a request that was passed on", logs.String())
				}
				return
			}
			httpxtest.CheckRejection(t, rec, tt.wantRoute, "invalid_request_error")
			httpxtest.CheckRejectionLog(t, logs.String(), tt.wantRoute, token, strings.TrimPrefix(tt.target, "/"))
		})
	}
}

// AC-34: a Proxy mounted without Guard still refuses a dot segment, so the
// route stays a boundary (spec §3.4), and forwards nothing.
func TestProxyRejectsDotSegmentsWithoutGuard(t *testing.T) {
	var calls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	t.Cleanup(up.Close)
	target, _ := url.Parse(up.URL + "/mcp")
	var logs bytes.Buffer
	p := New(Config{Routes: []Route{{Prefix: "/mcp/orders", Channel: "mcp", Upstream: "mcp:orders", Target: target}},
		Logger: slog.New(slog.NewTextHandler(&logs, nil))})

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp/orders/%2e%2e/admin", nil))
	if rec.Code != http.StatusBadRequest || calls.Load() != 0 {
		t.Fatalf("status %d with %d upstream calls, want 400 and none", rec.Code, calls.Load())
	}
	httpxtest.CheckRejection(t, rec, routeMCP, "invalid_request_error")
	httpxtest.CheckRejectionLog(t, logs.String(), routeMCP, "admin")
}

// AC-37: the host and browser-header checks of spec §3.6, in the order host,
// browser headers, path.
func TestGuardHostAndBrowserHeaders(t *testing.T) {
	const secret = "rebind.example"
	tests := []struct {
		name   string
		listen []string // hosts given to Guard, without brackets
		host   string
		header http.Header
		target string
		want   int
	}{
		{"loopback localhost", []string{"127.0.0.1"}, "localhost:4790", nil, "", 200},
		{"loopback ipv4", []string{"127.0.0.1"}, "127.0.0.1:4790", nil, "", 200},
		{"loopback ipv6", []string{"::1"}, "[::1]:4790", nil, "", 200},
		{"port forward", []string{"127.0.0.1"}, "localhost:9000", nil, "", 200},
		{"no port", []string{"127.0.0.1"}, "localhost", nil, "", 200},
		{"uppercase", []string{"127.0.0.1"}, "LocalHost:4790", nil, "", 200},
		{"other ip on loopback", []string{"127.0.0.1"}, "10.0.0.5:4790", nil, "", 200},
		{"ipv6 literal", []string{"127.0.0.1"}, "[fd00::1]:4790", nil, "", 200},
		{"ipv4-mapped", []string{"127.0.0.1"}, "[::ffff:10.0.0.5]:4790", nil, "", 200},
		{"listen host name", []string{"myhost.internal"}, "MyHost.internal:4790", nil, "", 200},
		{"unsafe localhost", []string{"0.0.0.0"}, "localhost:4790", nil, "", 200},
		{"rebinding name", []string{"127.0.0.1"}, secret + ":4790", nil, "", 403},
		{"trailing dot", []string{"127.0.0.1"}, "localhost.:4790", nil, "", 403},
		{"name on unsafe", []string{"0.0.0.0"}, secret + ":4790", nil, "", 403},
		{"empty host", []string{"127.0.0.1"}, "", nil, "", 403},
		{"empty listen host", []string{""}, "", nil, "", 403},
		{"leading zeros", []string{"127.0.0.1"}, "010.0.0.5:4790", nil, "", 403},
		{"short ipv4", []string{"127.0.0.1"}, "127.1:4790", nil, "", 403},
		{"octet range", []string{"127.0.0.1"}, "256.0.0.1:4790", nil, "", 403},
		{"ipv6 zone", []string{"127.0.0.1"}, "[fe80::1%25en0]:4790", nil, "", 403},
		{"unbracketed ipv6", []string{"127.0.0.1"}, "::1", nil, "", 403},
		{"bracketed ipv4", []string{"127.0.0.1"}, "[10.0.0.5]:4790", nil, "", 403},
		{"userinfo", []string{"127.0.0.1"}, "user@localhost:4790", nil, "", 403},
		{"unclosed bracket", []string{"127.0.0.1"}, "[::1", nil, "", 403},
		{"junk after bracket", []string{"127.0.0.1"}, "[::1]4790", nil, "", 403},
		{"non-numeric port", []string{"127.0.0.1"}, "localhost:47a0", nil, "", 403},
		{"docker internal", []string{"127.0.0.1"}, "host.docker.internal:4790", nil, "", 403},
		{"origin", []string{"127.0.0.1"}, "localhost:4790", http.Header{"Origin": {"http://localhost:4790"}}, "", 403},
		{"origin null", []string{"127.0.0.1"}, "localhost:4790", http.Header{"Origin": {"null"}}, "", 403},
		{"origin empty", []string{"127.0.0.1"}, "localhost:4790", http.Header{"Origin": {""}}, "", 403},
		{"fetch site", []string{"127.0.0.1"}, "localhost:4790", http.Header{"Sec-Fetch-Site": {"cross-site"}}, "", 403},
		{"fetch site same-origin", []string{"127.0.0.1"}, "localhost:4790", http.Header{"Sec-Fetch-Site": {"same-origin"}}, "", 403},
		{"fetch mode only", []string{"127.0.0.1"}, "localhost:4790", http.Header{"Sec-Fetch-Mode": {"cors"}}, "", 200},
		{"wrong host and dot segment", []string{"127.0.0.1"}, secret, nil, "/mcp/orders/../x", 403},
		{"origin and dot segment", []string{"127.0.0.1"}, "localhost", http.Header{"Origin": {"null"}}, "/mcp/orders/../x", 403},
		{"right host and dot segment", []string{"127.0.0.1"}, "localhost", nil, "/mcp/orders/../x", 400},
	}
	for _, tt := range tests {
		for _, route := range []struct{ name, path string }{{routeLLM, "/llm/anthropic/v1/messages"}, {routeMCP, "/mcp/orders/x"}} {
			t.Run(tt.name+"/"+route.name, func(t *testing.T) {
				var logs bytes.Buffer
				log := slog.New(slog.NewTextHandler(&logs, nil))
				var reached bool
				next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })
				path := route.path
				if tt.target != "" {
					path = tt.target
				}
				r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path+"?token=AC37-TOKEN", nil)
				r.Host = tt.host
				for k, v := range tt.header {
					r.Header[k] = v
				}
				rec := httptest.NewRecorder()
				Guard(next, log, tt.listen...).ServeHTTP(rec, r)

				if rec.Code != tt.want {
					t.Fatalf("status %d, want %d", rec.Code, tt.want)
				}
				if tt.want == 200 {
					if !reached || logs.Len() != 0 {
						t.Errorf("reached = %v, logs %q; want the request passed on silently", reached, logs.String())
					}
					return
				}
				if reached {
					t.Fatal("a rejected request reached the next handler")
				}
				wantRoute := routeOf(path)
				if tt.want == 403 {
					httpxtest.CheckRejection(t, rec, wantRoute, "permission_error")
				} else {
					httpxtest.CheckRejection(t, rec, wantRoute, "invalid_request_error")
				}
				leaks := []string{"AC37-TOKEN", "orders", "http://localhost", "user@"}
				if tt.host != "" && tt.host != "localhost" {
					leaks = append(leaks, tt.host)
				}
				if v := tt.header.Get("Origin"); v != "" {
					leaks = append(leaks, v)
				}
				httpxtest.CheckRejectionLog(t, logs.String(), wantRoute, leaks...)
				for _, leak := range leaks {
					if strings.Contains(rec.Body.String(), leak) {
						t.Errorf("body %q contains %q", rec.Body, leak)
					}
				}
			})
		}
	}
}

// AC-37: the body-size check comes after the host check, and the body of a
// request with a wrong host is never read.
func TestGuardHostBeforeBody(t *testing.T) {
	body := &httpxtest.CountingReader{R: io.LimitReader(httpxtest.Zeros{}, httpx.MaxRequestBytes+1)}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/llm/anthropic/v1/messages", body)
	r.Host, r.ContentLength = "rebind.example", httpx.MaxRequestBytes+1
	rec := httptest.NewRecorder()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.ReadBody(w, r, slog.New(slog.DiscardHandler), routeLLM)
	})
	Guard(next, slog.New(slog.DiscardHandler), "127.0.0.1").ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden || body.N != 0 {
		t.Errorf("status %d after reading %d bytes, want 403 and none", rec.Code, body.N)
	}
}
