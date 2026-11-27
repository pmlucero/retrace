package httpx

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
)

// A body that cannot be read is not a rejection of spec §3.6: it gets the
// plain-text 400 it got before the limit, and no rejection warning.
func TestReadBodyFailure(t *testing.T) {
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages",
		io.MultiReader(strings.NewReader("{"), iotest.ErrReader(io.ErrUnexpectedEOF)))
	var logs bytes.Buffer
	rec := httptest.NewRecorder()
	if _, ok := ReadBody(rec, r, slog.New(slog.NewTextHandler(&logs, nil)), RouteLLM); ok {
		t.Fatal("read a broken body")
	}
	if rec.Code != http.StatusBadRequest || !strings.HasPrefix(rec.Body.String(), "retrace: reading request body: ") {
		t.Errorf("answer %d %q, want 400 with the read error", rec.Code, rec.Body)
	}
	if logs.Len() != 0 {
		t.Errorf("logged %q, want no rejection warning", logs.String())
	}
}

// The limit and the message the agent gets agree (spec §3.6).
func TestTooLargeReasonNamesTheLimit(t *testing.T) {
	if MaxRequestBytes != 1<<26 || !strings.Contains(reasonTooLarge, "64 MiB (67108864 bytes)") {
		t.Errorf("limit %d, reason %q; want 64 MiB in both", MaxRequestBytes, reasonTooLarge)
	}
}

func TestUpstreamFailed(t *testing.T) {
	rec := httptest.NewRecorder()
	UpstreamFailed(rec, "dial tcp: refused")
	if rec.Code != http.StatusBadGateway || rec.Body.String() != "retrace: upstream request failed: dial tcp: refused\n" {
		t.Errorf("answer %d %q, want 502 with the message", rec.Code, rec.Body)
	}
}
