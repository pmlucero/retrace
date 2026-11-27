package session

import (
	"log/slog"
	"net/http"
	"net/url"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/provider/anthropic"
	"github.com/pmlucero/retrace/internal/proxy"
	"github.com/pmlucero/retrace/internal/redact"
	"github.com/pmlucero/retrace/internal/replay"
)

// LiveServer is an MCP server that a live-tools replay forwards to.
type LiveServer struct {
	Name   string
	Target *url.URL
}

// Routes is what Handler routes a replay's requests to. Every mode needs
// Mode, Writer, Rules, Log and Src. The modes differ in the rest:
//
//   - exact answers the LLM and every MCP server from the cassette: Queues,
//     LLMCanonicalize, MCPCanonicalize, Divergence, Strategy and Opts, with
//     every server in Replayed.
//   - swap-model forwards the LLM with Anthropic and Model, and answers MCP
//     as exact does.
//   - live-tools answers the LLM as exact does and forwards the MCP servers in
//     Live. Replayed is empty.
type Routes struct {
	Mode       cassette.Mode
	Src        *cassette.Cassette
	Queues     replay.QueueSet
	Rules      *redact.Rules
	Writer     *cassette.Writer
	Divergence *replay.Divergence
	Strategy   replay.DivergenceStrategy
	Log        *slog.Logger
	Opts       replay.Options
	Anthropic  *url.URL     // the LLM upstream of swap-model
	Model      string       // the model of swap-model
	Live       []LiveServer // the MCP servers that live-tools forwards to
	Replayed   []string     // the MCP servers answered from the cassette

	// LLMCanonicalize and MCPCanonicalize are the providers' canonical forms
	// of a request body (spec §5 step 1), injected as in replay.BuildQueues.
	LLMCanonicalize func(body []byte) []byte
	MCPCanonicalize func(server string, body []byte) []byte
}

// Handler returns the handler of a replay's proxy (spec §4). In exact
// mode every channel answers from the cassette and nothing is forwarded
// upstream (spec §4.1). swap-model forwards the LLM with the model rewritten
// (spec §4.2), and live-tools forwards MCP unchanged (spec §4.3). A miss on
// a channel served from the cassette ends the replay (spec §6.2).
func Handler(rt Routes) http.Handler {
	mux := http.NewServeMux()
	if rt.Mode.ForwardsLLM() {
		mux.Handle("/llm/anthropic/", proxy.New(proxy.Config{Routes: []proxy.Route{{
			Prefix: "/llm/anthropic", Channel: "llm", Upstream: "anthropic", Target: rt.Anthropic,
			Provenance: "live", Rewrite: anthropic.ModelRewriter(rt.Model),
		}}, Writer: rt.Writer, Redact: rt.Rules, Logger: rt.Log}))
	} else {
		mux.Handle("/llm/anthropic/", http.StripPrefix("/llm/anthropic", replay.NewLLMHandler(replay.LLMConfig{
			Source: rt.Src, Queues: rt.Queues.LLM, Redact: rt.Rules, Canonicalize: rt.LLMCanonicalize, Writer: rt.Writer,
			Divergence: rt.Divergence, Strategy: rt.Strategy, Logger: rt.Log, Options: rt.Opts,
		})))
	}
	if len(rt.Live) > 0 {
		routes := make([]proxy.Route, len(rt.Live))
		for i, sv := range rt.Live {
			routes[i] = proxy.Route{Prefix: "/mcp/" + sv.Name, Channel: "mcp", Upstream: "mcp:" + sv.Name,
				Target: sv.Target, Provenance: "live"}
		}
		forward := proxy.New(proxy.Config{Routes: routes, Writer: rt.Writer, Redact: rt.Rules, Logger: rt.Log})
		for _, r := range routes {
			mux.Handle(r.Prefix, forward) // the URL the agent got (spec §3.2)
			mux.Handle(r.Prefix+"/", forward)
		}
	}
	for _, name := range rt.Replayed {
		prefix := "/mcp/" + name
		h := http.StripPrefix(prefix, replay.NewMCPHandler(replay.MCPConfig{
			Server: name, Source: rt.Src, Queues: rt.Queues.MCP[name], Redact: rt.Rules,
			Canonicalize: mcpCanonicalize(rt.MCPCanonicalize, name), Writer: rt.Writer, Divergence: rt.Divergence, Strategy: rt.Strategy,
			Logger: rt.Log, Options: rt.Opts,
		}))
		mux.Handle(prefix, h) // the URL the agent got (spec §3.2)
		mux.Handle(prefix+"/", h)
	}
	return mux
}

// mcpCanonicalize returns the canonical form of a request body sent to MCP
// server name, after redaction (spec §5 step 1).
func mcpCanonicalize(canonical func(server string, body []byte) []byte, name string) func(body []byte) []byte {
	return func(body []byte) []byte { return canonical(name, body) }
}
