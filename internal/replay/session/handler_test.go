package session

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/mcp"
	"github.com/pmlucero/retrace/internal/provider/anthropic"
	"github.com/pmlucero/retrace/internal/redact"
	"github.com/pmlucero/retrace/internal/replay"
)

// routesFor returns Routes over an empty source cassette and a new session
// to record into.
func routesFor(t *testing.T, mode string) Routes {
	t.Helper()
	ctx := context.Background()
	open := func(name string) *cassette.Cassette {
		c, err := cassette.Create(ctx, filepath.Join(t.TempDir(), name+".cassette"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		err = c.Write(ctx, func(tx *cassette.Tx) error {
			return tx.InsertSession(ctx, cassette.Session{ID: name, Mode: mode, Status: "recording",
				CreatedAt: time.Now(), Command: []string{"agent"}, Cwd: "/", OnMiss: "strict", RetraceVersion: "dev"})
		})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	src, out := open("src"), open("out")
	w, err := cassette.NewWriter(ctx, out, cassette.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close(ctx, cassette.SessionEnd{Status: "completed", EndedAt: time.Now()}) })
	qs, err := replay.BuildQueues(ctx, src, anthropic.Canonicalize, []string{"a"}, mcp.Canonicalize)
	if err != nil {
		t.Fatal(err)
	}
	return Routes{Mode: mode, Src: src, Queues: qs, Rules: redact.NewRules(nil, nil), Writer: w, Divergence: replay.NewDivergence(),
		Strategy: replay.Strict{}}
}

func post(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// spec §4.1, §4.3: MCP servers answered from the cassette and MCP servers
// forwarded are routed at both their prefix and prefix + "/".
func TestHandlerRoutesMCPByServer(t *testing.T) {
	var hits atomic.Int32
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(live.Close)
	target, err := url.Parse(live.URL)
	if err != nil {
		t.Fatal(err)
	}
	rt := routesFor(t, "live-tools")
	rt.Live = []LiveServer{{Name: "b", Target: target}}
	rt.Replayed = []string{"a"}
	h := Handler(rt)

	const req = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	for _, p := range []string{"/mcp/b", "/mcp/b/"} {
		if rec := post(t, h, p, req); rec.Code != http.StatusOK {
			t.Errorf("POST %s: status %d, want 200 from the live server", p, rec.Code)
		}
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("live server got %d requests, want 2", got)
	}
	for _, p := range []string{"/mcp/a", "/mcp/a/"} {
		if rec := post(t, h, p, req); rec.Code == http.StatusNotFound || hits.Load() != 2 {
			t.Errorf("POST %s: status %d, live hits %d; want an answer from the empty cassette", p, rec.Code, hits.Load())
		}
	}
	if rec := post(t, h, "/mcp/c", req); rec.Code != http.StatusNotFound {
		t.Errorf("POST /mcp/c: status %d, want 404 for an unregistered server", rec.Code)
	}
}

// spec §4.2: swap-model forwards the LLM with the model rewritten.
func TestHandlerSwapModelForwardsTheLLM(t *testing.T) {
	var got atomic.Value
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.Store(string(b))
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(up.Close)
	target, err := url.Parse(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	rt := routesFor(t, "swap-model")
	rt.Anthropic, rt.Model = target, "model-b"
	rec := post(t, Handler(rt), "/llm/anthropic/v1/messages", `{"model":"model-a","messages":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 from the upstream", rec.Code)
	}
	if s, _ := got.Load().(string); !strings.Contains(s, `"model-b"`) || strings.Contains(s, "model-a") {
		t.Errorf("upstream got %q, want the model rewritten to model-b", s)
	}
}

// spec §4.1, §6.2: in exact mode the LLM is answered from the cassette, and
// a miss is not forwarded.
func TestHandlerExactAnswersTheLLMFromTheCassette(t *testing.T) {
	rt := routesFor(t, "exact")
	rec := post(t, Handler(rt), "/llm/anthropic/v1/messages", `{"model":"m","messages":[]}`)
	if rec.Code == http.StatusOK || rec.Code == http.StatusNotFound {
		t.Errorf("status %d, want a cassette miss", rec.Code)
	}
}
