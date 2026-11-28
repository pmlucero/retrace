package replay

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/httpx/httpxtest"
	"github.com/pmlucero/retrace/internal/match"
	"github.com/pmlucero/retrace/internal/proxy"
)

// AC-53, AC-03: the body-read deadline, the in-flight limit and the idle
// timeout in exact mode. Nothing is forwarded: the proxy has no upstream.
func TestExactConnectionLimits(t *testing.T) {
	const okBody, sseBody = `{"q":"plain request"}`, `{"q":"streamed request"}`
	httpxtest.CheckLimits(t, func(t *testing.T, p httpxtest.LimitsParams) httpxtest.LimitsEnv {
		gap := 3 * p.BodyTimeout
		src := sourceCassette(t,
			recorded{request: okBody, body: `{"ok":true}`},
			recorded{request: sseBody, chunks: []cassette.Chunk{
				{Index: 0, Data: []byte("event: one\ndata: 1\n\n")},
				{Index: 1, OffsetMillis: gap.Milliseconds(), Data: []byte("event: two\ndata: 2\n\n")},
			}},
		)
		ctx := context.Background()
		ins, err := src.Interactions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		queues := match.NewQueues(ins, func(in cassette.Interaction) (string, bool) {
			b, err := src.Blob(ctx, *in.RequestBlob)
			if err != nil {
				return "", false
			}
			return testFingerprint(b), true
		})
		out := newCassette(t, "exact")
		w, err := cassette.NewWriter(ctx, out, cassette.WriterOptions{})
		if err != nil {
			t.Fatal(err)
		}
		logs := &httpxtest.LockedBuffer{}
		log := slog.New(slog.NewTextHandler(logs, nil))
		h := NewLLMHandler(LLMConfig{Source: src, Queues: queues, Canonicalize: testCanonicalize,
			Writer: w, Logger: log, Options: Options{Realtime: true}})
		server := httptest.NewUnstartedServer(nil)
		server.Config = proxy.NewServer(http.StripPrefix("/llm/anthropic", h), log, proxy.Limits{
			BodyReadTimeout: p.BodyTimeout, IdleTimeout: p.Idle, MaxInFlight: p.MaxInFlight}, "127.0.0.1")
		server.Start()
		t.Cleanup(server.Close)
		return httpxtest.LimitsEnv{
			Addr: strings.TrimPrefix(server.URL, "http://"), Path: "/llm/anthropic/v1/messages",
			OKBody: okBody, SSEBody: sseBody, SSEGap: gap,
			SSEWant:   []string{"event: one", "event: two"},
			Forwarded: func() int { return 0 },
			Recorded: func() int {
				server.Close()
				if err := w.Close(ctx, cassette.SessionEnd{Status: "completed", EndedAt: time.Now()}); err != nil {
					t.Fatalf("closing writer: %v", err)
				}
				got, err := out.Interactions(ctx)
				if err != nil {
					t.Fatal(err)
				}
				return len(got)
			},
			Logs: logs.String,
		}
	})
}
