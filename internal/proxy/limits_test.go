package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pmlucero/retrace/internal/httpx"
	"github.com/pmlucero/retrace/internal/httpx/httpxtest"
)

// AC-53, AC-03: the body-read deadline, the in-flight limit and the idle
// timeout in record mode, with a fake upstream that counts what reaches it.
func TestRecordConnectionLimits(t *testing.T) {
	const sseBody = `{"stream":true,"pad":"............"}`
	httpxtest.CheckLimits(t, func(t *testing.T, p httpxtest.LimitsParams) httpxtest.LimitsEnv {
		gap := 3 * p.BodyTimeout
		var forwarded atomic.Int32
		upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			forwarded.Add(1)
			body, _ := io.ReadAll(r.Body)
			if string(body) != sseBody {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"ok":true}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: one\ndata: 1\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(gap)
			_, _ = io.WriteString(w, "event: two\ndata: 2\n\n")
		})
		logs := &httpxtest.LockedBuffer{}
		h := newHarnessOpts(t, upstream, harnessOptions{
			logger: slog.New(slog.NewTextHandler(logs, nil)),
			limits: &Limits{BodyReadTimeout: p.BodyTimeout, IdleTimeout: p.Idle, MaxInFlight: p.MaxInFlight},
		})
		return httpxtest.LimitsEnv{
			Addr: strings.TrimPrefix(h.server.URL, "http://"), Path: "/llm/anthropic/v1/messages",
			OKBody: `{"model":"m","messages":[]}`, SSEBody: sseBody, SSEGap: gap,
			SSEWant:   []string{"event: one", "event: two"},
			Forwarded: func() int { return int(forwarded.Load()) },
			Recorded:  func() int { return len(h.interactions()) },
			Logs:      logs.String,
		}
	})
}

// AC-53: the 408 and 503 answers are as spec §3.6 says on both routes, and
// ReadBody asks for the connection to be closed after a 408.
func TestRejectTimeoutAndOverload(t *testing.T) {
	for _, route := range []string{httpx.RouteLLM, httpx.RouteMCP} {
		for _, tt := range []struct {
			status  int
			errType string
		}{
			{http.StatusRequestTimeout, "invalid_request_error"},
			{http.StatusServiceUnavailable, "overloaded_error"},
		} {
			rec := httptest.NewRecorder()
			httpx.Reject(rec, slog.New(slog.DiscardHandler), route, tt.status, "why")
			if rec.Code != tt.status {
				t.Errorf("%s: status %d, want %d", route, rec.Code, tt.status)
			}
			httpxtest.CheckRejection(t, rec, route, tt.errType)
		}
	}
}
