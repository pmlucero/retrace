package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/pmlucero/retrace/internal/cassette"
)

// swapUpstream is a fake Anthropic API that answers with the model it was
// asked for, and keeps the models it was asked for.
type swapUpstream struct {
	*httptest.Server
	mu     sync.Mutex
	models []string
}

func newSwapUpstream(t *testing.T) *swapUpstream {
	t.Helper()
	u := &swapUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Model string }
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/v1/messages" || r.Header.Get("X-Api-Key") != agentAPIKey || json.Unmarshal(body, &req) != nil {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		u.mu.Lock()
		u.models = append(u.models, req.Model)
		u.mu.Unlock()
		resp, _ := json.Marshal(map[string]any{
			"type": "message", "role": "assistant", "model": req.Model,
			"content":     []map[string]string{{"type": "text", "text": "Sunny."}},
			"stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 100, "output_tokens": 10},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(resp)
	}))
	t.Cleanup(u.Close)
	return u
}

// AC-14: run --model forwards LLM requests with the model rewritten, serves
// MCP from the cassette, and records both models (spec §4.2).
func TestRunSwapModel(t *testing.T) {
	dir := t.TempDir()
	recorded := recordMCPForReplay(t, dir, "s") // its upstreams are closed now
	up := newSwapUpstream(t)

	r := runRunCmd(t, dir, "s", "--model", "claude-opus-5-5", "--anthropic-upstream", up.URL, "--listen", "127.0.0.1:0")
	if r.code != ExitOK {
		t.Fatalf("run: exit %d\n%s", r.code, r.stderr)
	}
	// MCP was answered from the cassette as recorded.
	replayed := agentExchanges(t, r.stdout.String())
	if len(replayed) != len(recorded) {
		t.Fatalf("replay saw %d MCP exchanges, want %d", len(replayed), len(recorded))
	}
	for i := range recorded {
		sameExchange(t, recorded[i], replayed[i])
	}
	if len(up.models) != 1 || up.models[0] != "claude-opus-5-5" {
		t.Errorf("upstream was asked for %v, want [claude-opus-5-5]", up.models)
	}

	src, _ := sessionOf(t, dir, "s")
	s, ins := sessionOf(t, dir, "s.swap-model.1")
	if s.Mode != "swap-model" || s.Status != "completed" || s.ModelOverride == nil || *s.ModelOverride != "claude-opus-5-5" ||
		string(s.MCPServers) != string(src.MCPServers) {
		t.Errorf("session = %+v, want swap-model, completed, model_override claude-opus-5-5 and the source's mcp_servers", s)
	}
	c := openCassette(t, cassette.Path(dir, "s.swap-model.1"))
	var llm int
	for _, in := range ins {
		switch in.Channel {
		case "llm":
			llm++
			body, err := c.Blob(context.Background(), *in.RequestBlob)
			if err != nil {
				t.Fatal(err)
			}
			var req struct{ Model string }
			if err := json.Unmarshal(body, &req); err != nil || req.Model != "claude-sonnet-5-5" {
				t.Errorf("stored LLM request %s, want the agent's model", body)
			}
			if in.Provenance != "live" || in.SourceInteractionID != nil {
				t.Errorf("LLM interaction provenance %q, source %v; want live and none", in.Provenance, in.SourceInteractionID)
			}
		case "mcp":
			if in.Provenance != "replayed" {
				t.Errorf("MCP interaction %d provenance %q, want replayed", in.ID, in.Provenance)
			}
		}
	}
	if llm != 1 {
		t.Errorf("recorded %d LLM interactions, want 1", llm)
	}

	events, err := c.Events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var responses int
	for _, e := range events {
		if e.Kind != "model_response" {
			continue
		}
		responses++
		if e.RequestedModel == nil || *e.RequestedModel != "claude-sonnet-5-5" || e.Model == nil || *e.Model != "claude-opus-5-5" {
			t.Errorf("model_response requested %v, model %v; want claude-sonnet-5-5 and claude-opus-5-5", e.RequestedModel, e.Model)
		}
		if e.Provenance != "live" {
			t.Errorf("model_response provenance %q, want live", e.Provenance)
		}
	}
	if responses != 1 {
		t.Errorf("got %d model_response events, want 1", responses)
	}
}

// spec §4.4: in swap-model the LLM is forwarded, so its recorded
// interactions are not reported as unconsumed.
func TestRunSwapModelDoesNotCountTheLLMAsUnconsumed(t *testing.T) {
	dir := t.TempDir()
	recordMCPForReplay(t, dir, "s")
	up := newSwapUpstream(t)
	r := runRunCmd(t, dir, "s", "--model", "m", "--anthropic-upstream", up.URL, "--listen", "127.0.0.1:0")
	if r.code != ExitOK {
		t.Fatalf("run: exit %d\n%s", r.code, r.stderr)
	}
	if out := r.stderr.String(); strings.Contains(out, "never replayed") {
		t.Errorf("unexpected unconsumed warning:\n%s", out)
	}
}
