package httpx_test

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pmlucero/retrace/internal/httpx"
	"github.com/pmlucero/retrace/internal/httpx/httpxtest"
)

// AC-34: a body over 64 MiB is answered 413 with or without a Content-Length,
// and one at the limit is read.
func TestReadBodyLimit(t *testing.T) {
	tests := []struct {
		name     string
		size     int64
		withLen  bool
		channel  string
		wantRead bool
	}{
		{"llm with content length", httpx.MaxRequestBytes + 1, true, httpx.RouteLLM, false},
		{"llm chunked", httpx.MaxRequestBytes + 1, false, httpx.RouteLLM, false},
		{"mcp with content length", httpx.MaxRequestBytes + 1, true, httpx.RouteMCP, false},
		{"mcp chunked", httpx.MaxRequestBytes + 1, false, httpx.RouteMCP, false},
		{"at the limit", httpx.MaxRequestBytes, false, httpx.RouteLLM, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := &httpxtest.CountingReader{R: io.LimitReader(httpxtest.Zeros{}, tt.size)}
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", body)
			r.ContentLength = -1
			if tt.withLen {
				r.ContentLength = tt.size
			}
			var logs bytes.Buffer
			rec := httptest.NewRecorder()
			got, ok := httpx.ReadBody(rec, r, slog.New(slog.NewTextHandler(&logs, nil)), tt.channel)

			if ok != tt.wantRead {
				t.Fatalf("read = %v, want %v (status %d)", ok, tt.wantRead, rec.Code)
			}
			if tt.wantRead {
				if int64(len(got)) != tt.size {
					t.Errorf("read %d bytes, want %d", len(got), tt.size)
				}
				return
			}
			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status %d, want 413", rec.Code)
			}
			if tt.withLen && body.N != 0 {
				t.Errorf("read %d bytes of a body whose Content-Length is over the limit", body.N)
			}
			if !tt.withLen && body.N > httpx.MaxRequestBytes+1 {
				t.Errorf("read %d bytes, want at most the limit and one more", body.N)
			}
			httpxtest.CheckRejection(t, rec, tt.channel, "request_too_large")
			httpxtest.CheckRejectionLog(t, logs.String(), tt.channel, "", "")
		})
	}
}

func TestRejectShapes(t *testing.T) {
	tests := []struct {
		status  int
		route   string
		errType string
	}{
		{http.StatusForbidden, httpx.RouteLLM, "permission_error"},
		{http.StatusBadRequest, httpx.RouteLLM, "invalid_request_error"},
		{http.StatusRequestEntityTooLarge, httpx.RouteLLM, "request_too_large"},
		{http.StatusForbidden, httpx.RouteMCP, ""},
		{http.StatusBadRequest, "", ""},
	}
	for _, tt := range tests {
		var logs bytes.Buffer
		rec := httptest.NewRecorder()
		httpx.Reject(rec, slog.New(slog.NewTextHandler(&logs, nil)), tt.route, tt.status, "because")
		if rec.Code != tt.status {
			t.Errorf("status %d, want %d", rec.Code, tt.status)
		}
		httpxtest.CheckRejection(t, rec, tt.route, tt.errType)
		httpxtest.CheckRejectionLog(t, logs.String(), tt.route)
	}
}
