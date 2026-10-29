package replay

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/match"
	"github.com/pmlucero/retrace/internal/redact"
)

// The LLM route is the one replayed by this handler (spec §7.3).
const (
	llmChannel  = "llm"
	llmUpstream = "anthropic"
)

// LLMConfig configures an LLMHandler.
type LLMConfig struct {
	// Source is the cassette being replayed.
	Source Source
	// Queues holds the source's LLM interactions by fingerprint. After the
	// replay, Queues.Unconsumed lists the ones never served (spec §4.4).
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

// LLMHandler answers the LLM channel of an exact replay from the cassette
// (spec §4.1). It records each served interaction in the new session with
// provenance replayed (spec §4.4), a recorded transport error included,
// which it answers as the proxy did (spec §5 step 2). On a cassette miss it
// applies steps 1 and 2 of strict (spec §6.2): it appends a divergence event
// and answers 422; the caller ends the session as diverged (docs/plan/mvp.md,
// "Miss handling before strict"). It is safe for concurrent use.
type LLMHandler struct {
	cfg LLMConfig
	rc  *recorder
}

// NewLLMHandler returns an LLMHandler for cfg. It serves paths relative to the
// LLM route, as http.StripPrefix leaves them.
func NewLLMHandler(cfg LLMConfig) *LLMHandler {
	rc := newRecorder(recorderConfig{channel: llmChannel, upstream: llmUpstream, source: cfg.Source,
		opts: cfg.Options, redact: cfg.Redact, writer: cfg.Writer, log: cfg.Logger, divergence: cfg.Divergence})
	cfg.Redact = rc.redact
	return &LLMHandler{cfg: cfg, rc: rc}
}

// Diverged is closed at the first cassette miss, on this handler or on any
// other sharing its Divergence. The replay then ends with status diverged and
// exit code 3 (spec §6.2, AC-28).
func (h *LLMHandler) Diverged() <-chan struct{} { return h.rc.divergence.Done() }

// ServeHTTP answers r from the cassette, or as a cassette miss.
func (h *LLMHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
		h.miss(w, start, missResult{}, "the request body cannot be redacted, so it cannot be matched: "+err.Error())
		return
	}
	fp := h.cfg.Fingerprint(stored)
	src, ok := h.cfg.Queues.Next(fp)
	if !ok {
		res := missResult{ReceivedFingerprint: &fp}
		if blocked, ok := h.cfg.Queues.Blocked(fp); ok {
			// No diff for an incomplete recording (spec §5 step 4).
			res.IncompleteInteraction = &blocked.ID
			h.miss(w, start, res, fmt.Sprintf("the recording of the matching interaction %d is incomplete; record the session again", blocked.ID))
			return
		}
		h.miss(w, start, res, "no recorded request matches this one")
		return
	}
	ex := exchange{r: r, start: start, body: body, stored: stored, fp: fp, src: src}
	if transportError(src) {
		h.rc.serveTransportError(w, ex)
		return
	}
	if err := servable(src); err != nil {
		h.miss(w, start, missResult{ReceivedFingerprint: &fp},
			fmt.Sprintf("the matching interaction %d has no recorded response to replay", src.ID))
		return
	}
	h.rc.serveRecorded(r.Context(), w, ex)
}

// miss handles a cassette miss with steps 1 and 2 of strict (spec §6.2).
func (h *LLMHandler) miss(w http.ResponseWriter, start time.Time, res missResult, reason string) {
	msg := h.rc.miss(start, res, reason)

	resp, err := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": "retrace_cassette_miss", "message": msg},
	})
	if err != nil {
		resp = nil // unreachable
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnprocessableEntity)
	_, _ = w.Write(resp) // the agent may be gone; the miss is already recorded
}
