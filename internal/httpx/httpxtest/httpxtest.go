// Package httpxtest holds test helpers for the rejection responses of package
// httpx. It is imported by tests only.
package httpxtest

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pmlucero/retrace/internal/httpx"
)

// CheckRejection checks the body of a rejected request (spec §3.6): the
// Anthropic error shape with errType on the LLM route, plain text elsewhere,
// and a message starting with "retrace:" in both.
func CheckRejection(t *testing.T, rec *httptest.ResponseRecorder, route, errType string) {
	t.Helper()
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Errorf("Location = %q, want no redirect", loc)
	}
	if route != httpx.RouteLLM {
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Errorf("Content-Type = %q, want plain text", ct)
		}
		if !strings.HasPrefix(rec.Body.String(), "retrace: ") {
			t.Errorf("body %q, want a message starting with retrace:", rec.Body)
		}
		return
	}
	var got struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body %q: %v", rec.Body, err)
	}
	if got.Type != "error" || got.Error.Type != errType || !strings.HasPrefix(got.Error.Message, "retrace: ") {
		t.Errorf("body %s, want an Anthropic %s error whose message starts with retrace:", rec.Body, errType)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

// CheckRejectionLog checks the warning of a rejected request: one line, with
// its route if it has one, and none of the leaks given.
func CheckRejectionLog(t *testing.T, logs, route string, leaks ...string) {
	t.Helper()
	if strings.Count(logs, "\n") != 1 || !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, `msg="request rejected"`) {
		t.Errorf("logs %q, want one rejection warning", logs)
	}
	if route == "" && strings.Contains(logs, "route=") || route != "" && !strings.Contains(logs, "route="+route+" ") {
		t.Errorf("logs %q, want route %q", logs, route)
	}
	for _, leak := range leaks {
		if leak != "" && strings.Contains(logs, leak) {
			t.Errorf("logs %q contain %q", logs, leak)
		}
	}
}

// Zeros is an endless body.
type Zeros struct{}

func (Zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// CountingReader counts what is read from R, in N.
type CountingReader struct {
	R io.Reader
	N int64
}

func (c *CountingReader) Read(p []byte) (int, error) {
	n, err := c.R.Read(p)
	c.N += int64(n)
	return n, err
}
