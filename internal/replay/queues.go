package replay

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/match"
)

// QueueSet holds the queues of each route replayed from the cassette.
type QueueSet struct {
	LLM *match.Queues            // nil when the LLM is forwarded
	MCP map[string]*match.Queues // by server name
}

// Unconsumed returns the recorded interactions of every route that the
// replay never served, in recorded order (spec §4.4).
func (q QueueSet) Unconsumed() []cassette.Interaction {
	var all []cassette.Interaction
	if q.LLM != nil {
		all = q.LLM.Unconsumed()
	}
	for _, mq := range q.MCP {
		all = append(all, mq.Unconsumed()...)
	}
	slices.SortFunc(all, func(a, b cassette.Interaction) int { return cmp.Compare(a.ID, b.ID) })
	return all
}

// BuildQueues queues the recorded interactions of src by the fingerprint of
// their stored request bodies, which are already redacted, so they match
// incoming requests redacted with the same rules: one set of queues for the
// LLM route, when llm is not nil, and one per MCP server in servers. llm and
// mcpCanon give the canonical form of a request body, so that this package
// does not depend on a provider or a protocol. MCP requests that replay
// answers without matching are left out, so they are never reported as
// unconsumed (spec §5 step 3, AC-32).
func BuildQueues(ctx context.Context, src *cassette.Cassette, llm func(body []byte) []byte, servers []string,
	mcpCanon func(server string, body []byte) []byte) (QueueSet, error) {
	reqs, err := src.InteractionsWithRequests(ctx, cassette.InteractionFilter{})
	if err != nil {
		return QueueSet{}, fmt.Errorf("reading interactions: %w", err)
	}
	ins := make([]cassette.Interaction, len(reqs))
	for i, r := range reqs {
		ins[i] = r.Interaction
	}
	type key struct {
		route string // "" for the LLM, or the MCP server name
		fp    string
	}
	keys := make(map[int64]key, len(ins))
	for _, r := range reqs {
		in := r.Interaction
		var route string
		switch name, isMCP := strings.CutPrefix(in.Upstream, "mcp:"); {
		case in.Channel == "llm" && llm != nil:
			route = ""
		case in.Channel == "mcp" && isMCP && slices.Contains(servers, name):
			route = name
		default:
			continue
		}
		if r.RequestErr != nil {
			return QueueSet{}, fmt.Errorf("interaction %d: %w", in.ID, r.RequestErr)
		}
		body := r.Request
		switch {
		case in.Channel == "llm":
			keys[in.ID] = key{route, match.Fingerprint(llm(body))}
		case !AnsweredWithoutMatching(in.HTTPMethod, body):
			keys[in.ID] = key{route, match.Fingerprint(mcpCanon(route, body))}
		}
	}
	queuesOf := func(route string) *match.Queues {
		return match.NewQueues(ins, func(in cassette.Interaction) (string, bool) {
			k, ok := keys[in.ID]
			return k.fp, ok && k.route == route
		})
	}
	qs := QueueSet{MCP: make(map[string]*match.Queues, len(servers))}
	if llm != nil {
		qs.LLM = queuesOf("")
	}
	for _, name := range servers {
		qs.MCP[name] = queuesOf(name)
	}
	return qs, nil
}
