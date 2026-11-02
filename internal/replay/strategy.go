package replay

import "context"

// DivergenceStrategy decides how a replay answers a request that missed the
// cassette (spec §6.1). The handlers append the miss's divergence event
// before they ask it (spec §6.2 step 1), so every miss is recorded whatever
// the strategy decides.
type DivergenceStrategy interface {
	// Name is the --on-miss value that selects the strategy, stored as the
	// session's on_miss (spec §7.2).
	Name() string
	// OnMiss resolves m. An error is logged and the miss is aborted.
	OnMiss(ctx context.Context, m Miss) (Resolution, error)
}

// Miss is a request that missed the cassette, as its divergence event
// records it (spec §7.4).
type Miss struct {
	// Channel and Upstream name the route the request came in on.
	Channel, Upstream string
	// Reason says why the request missed. The agent gets it in the error of
	// an aborted miss.
	Reason string
}

// Resolution is a strategy's answer to a miss. Its zero value aborts: the
// agent gets the channel's cassette-miss error and the replay ends with
// status diverged (spec §6.2 steps 2 to 4).
type Resolution struct {
	// Respond, when set, answers the agent with a response that is not in
	// the cassette, and the replay goes on.
	Respond *Response
}

// Response is a response that a strategy makes up for a miss. It holds only
// what spec §6.1 makes normative: the fields that carry the response itself
// come with the first strategy that responds, whose needs (a stream, one
// JSON-RPC message per batch element) decide their shape.
type Response struct {
	// Provenance says where the response came from, and is stored with it.
	// It is mandatory (spec §6.1): fallthrough, nearest or synthetic for
	// the reserved strategies (spec §6.3).
	Provenance string
}

// Strict is the default strategy, and the only one in the MVP: every miss is
// aborted (spec §6.2, ADR 0006).
type Strict struct{}

// Name returns "strict".
func (Strict) Name() string { return "strict" }

// OnMiss aborts m.
func (Strict) OnMiss(context.Context, Miss) (Resolution, error) { return Resolution{}, nil }
