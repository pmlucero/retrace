package replay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/match"
	"github.com/pmlucero/retrace/internal/mcp"
	"github.com/pmlucero/retrace/internal/redact"
)

const (
	mcpChannel = "mcp"
	// sessionHeader carries the MCP session id (Streamable HTTP).
	sessionHeader = "Mcp-Session-Id"
	// missCode is the JSON-RPC error code of a cassette miss (spec §6.2).
	missCode = -32099
)

// MCPConfig configures an MCPHandler.
type MCPConfig struct {
	// Server is the name the MCP server was registered with (spec §3.4).
	Server string
	// Source is the cassette being replayed.
	Source Source
	// Queues holds the source's interactions with this server by
	// fingerprint.
	Queues *match.Queues
	// Redact holds the session's redaction rules. Nil selects the defaults.
	Redact *redact.Rules
	// Fingerprint computes the matching fingerprint of a request body after
	// redaction (spec §5 step 1). It must agree with the one Queues was built
	// with.
	Fingerprint func(body []byte) string
	// Writer records the new session.
	Writer *cassette.Writer
	// Divergence is signaled at a cassette miss. Nil selects the handler's
	// own.
	Divergence *Divergence
	Logger     *slog.Logger
	Options
}

// MCPHandler answers one MCP server's channel of an exact replay from the
// cassette (spec §4.1). It serves each matched POST with the recorded
// response, rewritten to carry the incoming JSON-RPC ids and the session ids
// it issued (spec §5 step 3), and records it in the new session with
// provenance replayed (spec §4.4). It answers a recorded transport error as
// the proxy did (spec §5 step 2), and a cassette miss with a JSON-RPC error
// -32099 after appending a divergence event (spec §6.2 steps 1 and 2). It is
// safe for concurrent use.
type MCPHandler struct {
	cfg MCPConfig
	rc  *recorder

	mu     sync.Mutex
	issued map[string]bool // the session ids this handler issued
}

// NewMCPHandler returns an MCPHandler for cfg. It serves paths relative to
// the server's route, as http.StripPrefix leaves them.
func NewMCPHandler(cfg MCPConfig) *MCPHandler {
	rc := newRecorder(recorderConfig{channel: mcpChannel, upstream: "mcp:" + cfg.Server, source: cfg.Source,
		opts: cfg.Options, redact: cfg.Redact, writer: cfg.Writer, log: cfg.Logger, divergence: cfg.Divergence})
	cfg.Redact = rc.redact
	return &MCPHandler{cfg: cfg, rc: rc, issued: map[string]bool{}}
}

// Diverged is closed at the first cassette miss, on this handler or on any
// other sharing its Divergence (spec §6.2, AC-31).
func (h *MCPHandler) Diverged() <-chan struct{} { return h.rc.divergence.Done() }

// ServeHTTP answers r from the cassette, or as a cassette miss.
func (h *MCPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := h.rc.clock.Now()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "retrace: reading request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Redaction comes before fingerprinting (spec §5 step 1), so a request
	// matches the stored, redacted copy of the recorded one.
	stored, err := h.cfg.Redact.Body(body)
	if err != nil {
		h.miss(w, start, body, missResult{}, "the request body cannot be redacted, so it cannot be matched: "+err.Error())
		return
	}
	fp := h.cfg.Fingerprint(stored)
	src, ok := h.cfg.Queues.Next(fp)
	if !ok {
		res := missResult{ReceivedFingerprint: &fp}
		if blocked, ok := h.cfg.Queues.Blocked(fp); ok {
			// No diff for an incomplete recording (spec §5 step 4).
			res.IncompleteInteraction = &blocked.ID
			h.miss(w, start, body, res, fmt.Sprintf("the recording of the matching interaction %d is incomplete; record the session again", blocked.ID))
			return
		}
		h.miss(w, start, body, res, "no recorded request matches this one")
		return
	}
	ex := exchange{r: r, start: start, body: body, stored: stored, fp: fp, src: src}
	if transportError(src) {
		h.rc.serveTransportError(w, ex)
		return
	}
	if err := servable(src); err != nil {
		h.miss(w, start, body, missResult{ReceivedFingerprint: &fp},
			fmt.Sprintf("the matching interaction %d has no recorded response to replay", src.ID))
		return
	}
	ex.adapt = func() (adaptation, error) { return h.adapt(r.Context(), ex) }
	h.rc.serveRecorded(r.Context(), w, ex)
}

// adapt returns how the response to ex differs from the recorded one
// (spec §5 step 3): each response carries the id of the incoming request it
// answers, and the session id follows the agent's session.
func (h *MCPHandler) adapt(ctx context.Context, ex exchange) (adaptation, error) {
	// A body that is not JSON-RPC has no ids: its messages are nil.
	incoming, _ := mcp.ParseBody(ex.body)
	storedIncoming, _ := mcp.ParseBody(ex.stored)
	var recorded mcp.Body
	if ex.src.RequestBlob != nil {
		b, err := h.cfg.Source.Blob(ctx, *ex.src.RequestBlob)
		if err != nil {
			return adaptation{}, fmt.Errorf("reading the recorded request of interaction %d: %w", ex.src.ID, err)
		}
		recorded, _ = mcp.ParseBody(b)
	}
	// The agent gets its own ids. The new session stores them as the
	// redaction rules stored the incoming request, so a redacted id never
	// reaches the cassette.
	served := mcp.NewIDRewriter(recorded.Messages, incoming.Messages)
	store := mcp.NewIDRewriter(recorded.Messages, storedIncoming.Messages)
	ad := adaptation{rewrite: func(data []byte, event bool) ([]byte, []byte) {
		if event {
			return served.Event(data), store.Event(data)
		}
		return served.Body(data), store.Body(data)
	}}

	var hdr http.Header
	if err := json.Unmarshal(ex.src.ResponseHeaders, &hdr); err != nil {
		return adaptation{}, fmt.Errorf("serving interaction %d: response headers: %w", ex.src.ID, err)
	}
	id, ok := h.sessionID(ex.r, incoming, hdr)
	if !ok {
		return ad, nil
	}
	storedHdr := hdr.Clone()
	hdr.Del(sessionHeader)
	storedHdr.Del(sessionHeader)
	if id != "" {
		hdr.Set(sessionHeader, id)
		storedHdr.Set(sessionHeader, h.cfg.Redact.Headers(http.Header{sessionHeader: {id}}).Get(sessionHeader))
	}
	var err error
	if ad.served, err = json.Marshal(hdr); err != nil {
		return adaptation{}, err // http.Header always marshals; unreachable
	}
	if ad.stored, err = json.Marshal(storedHdr); err != nil {
		return adaptation{}, err
	}
	return ad, nil
}

// sessionID returns the Mcp-Session-Id of the response to r (spec §5 step 3):
// a fresh one for initialize, the agent's own when this handler issued it
// and the recorded response carried one, and "" to drop the recorded one.
// ok is false when the recorded header is kept: there is none to change.
func (h *MCPHandler) sessionID(r *http.Request, incoming mcp.Body, recorded http.Header) (id string, ok bool) {
	for _, m := range incoming.Messages {
		if m.Kind == mcp.Request && m.Method == mcp.MethodInitialize {
			return h.issue(), true
		}
	}
	if recorded.Get(sessionHeader) == "" {
		return "", false
	}
	id = r.Header.Get(sessionHeader)
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.issued[id] {
		return "", true
	}
	return id, true
}

// issue returns a new session id and remembers it.
func (h *MCPHandler) issue() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // never fails (crypto/rand)
	id := hex.EncodeToString(b)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.issued[id] = true
	return id
}

// miss handles a cassette miss with steps 1 and 2 of strict (spec §6.2): a
// JSON-RPC error -32099 with each incoming request's id, or null when it
// cannot be read (spec §5 step 3, AC-31).
func (h *MCPHandler) miss(w http.ResponseWriter, start time.Time, body []byte, res missResult, reason string) {
	h.rc.miss(start, res, reason)

	errObj := map[string]any{"code": missCode, "message": "retrace: cassette miss", "data": reason}
	answer := func(id json.RawMessage) map[string]any {
		return map[string]any{"jsonrpc": "2.0", "id": id, "error": errObj}
	}
	var resp any = answer(json.RawMessage("null"))
	if parsed, err := mcp.ParseBody(body); err == nil {
		var answers []map[string]any
		for _, m := range parsed.Messages {
			if m.Kind == mcp.Request {
				answers = append(answers, answer(m.ID))
			}
		}
		switch {
		case parsed.Batch && len(answers) > 0:
			resp = answers
		case !parsed.Batch && len(answers) == 1:
			resp = answers[0]
		}
	}
	b, err := json.Marshal(resp)
	if err != nil {
		b = nil // maps of strings and valid JSON ids always marshal; unreachable
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b) // the agent may be gone; the miss is already recorded
}
