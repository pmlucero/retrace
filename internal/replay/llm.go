package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/derive"
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
	Logger *slog.Logger
	Options
}

// LLMHandler answers the LLM channel of an exact replay from the cassette
// (spec §4.1). It records each served interaction in the new session with
// provenance replayed (spec §4.4). On a cassette miss it applies steps 1 and 2
// of strict (spec §6.2): it appends a divergence event and answers 422; the
// caller ends the session as diverged (docs/plan/mvp.md, "Miss handling
// before strict"). It is safe for concurrent use.
type LLMHandler struct {
	cfg      LLMConfig
	clock    Clock
	log      *slog.Logger
	once     sync.Once
	diverged chan struct{}
}

// NewLLMHandler returns an LLMHandler for cfg. It serves paths relative to the
// LLM route, as http.StripPrefix leaves them.
func NewLLMHandler(cfg LLMConfig) *LLMHandler {
	h := &LLMHandler{cfg: cfg, clock: cfg.Clock, log: cfg.Logger, diverged: make(chan struct{})}
	if h.cfg.Redact == nil {
		h.cfg.Redact = redact.NewRules(nil, nil)
	}
	if h.clock == nil {
		h.clock = systemClock{}
	}
	if h.log == nil {
		h.log = slog.New(slog.DiscardHandler)
	}
	return h
}

// Diverged is closed at the first cassette miss. The replay then ends with
// status diverged and exit code 3 (spec §6.2, AC-28).
func (h *LLMHandler) Diverged() <-chan struct{} { return h.diverged }

// ServeHTTP answers r from the cassette, or as a cassette miss.
func (h *LLMHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := h.clock.Now()
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
	if err := servable(src); err != nil {
		h.miss(w, start, missResult{ReceivedFingerprint: &fp},
			fmt.Sprintf("the matching interaction %d has no recorded response to replay", src.ID))
		return
	}
	h.serve(w, r, start, body, stored, fp, src)
}

// serve answers r with src and records the exchange in the new session.
func (h *LLMHandler) serve(w http.ResponseWriter, r *http.Request, start time.Time, body, stored []byte, fp string, src cassette.Interaction) {
	path := r.URL.Path
	if path == "" {
		path = "/"
	}
	if r.URL.RawQuery != "" {
		path += "?" + h.cfg.Redact.Query(r.URL.RawQuery)
	}
	headers, err := json.Marshal(h.cfg.Redact.Headers(r.Header))
	if err != nil {
		headers = json.RawMessage(`{}`) // http.Header always marshals; unreachable
	}
	srcID := src.ID
	in := cassette.Interaction{
		Channel: llmChannel, Upstream: llmUpstream, HTTPMethod: r.Method, Path: path,
		RequestHeaders: headers, Fingerprint: &fp, StartedAt: start,
		Provenance: "replayed", SourceInteractionID: &srcID,
	}
	var blobs [][]byte
	if len(body) > 0 {
		key := cassette.BlobKey(stored)
		in.RequestBlob = &key
		blobs = append(blobs, stored)
	}
	rec := &replayRecorder{h: h, src: src, start: start, id: h.cfg.Writer.BeginInteraction(in, blobs...)}
	err = serve(r.Context(), w, h.cfg.Source, src, h.cfg.Options, rec)
	switch {
	case err == nil:
		rec.complete(nil) // a safety net: the headers or the last write already completed it
	case !rec.wroteHeader && errors.Is(err, context.Canceled):
		// The agent left while the cassette was being read.
		msg := "the agent closed the connection before the response arrived"
		rec.complete(&msg)
	case !rec.wroteHeader:
		// The cassette could not be read: nothing was sent yet.
		h.log.Error("reading the cassette", "interaction", src.ID, "error", err)
		msg := "reading the recorded response: " + err.Error()
		rec.complete(&msg)
		http.Error(w, "retrace: "+msg, http.StatusInternalServerError)
	default:
		msg := "serving the recorded response: " + err.Error()
		if errors.Is(err, context.Canceled) {
			msg = "the agent closed the connection before the response ended"
		}
		rec.complete(&msg)
	}
}

// missResult is the result of a divergence event (spec §7.4).
type missResult struct {
	Channel               string     `json:"channel"`
	Upstream              string     `json:"upstream"`
	ReceivedFingerprint   *string    `json:"received_fingerprint"`
	Diff                  []diffItem `json:"diff"`
	IncompleteInteraction *int64     `json:"incomplete_interaction,omitempty"`
}

// diffItem is one entry of the structural diff of a miss (spec §5 step 4).
// The diff is empty until M5 (docs/plan/mvp.md, "Miss handling before
// strict").
type diffItem struct {
	Path     string          `json:"path"`
	Expected json.RawMessage `json:"expected"`
	Received json.RawMessage `json:"received"`
}

// miss handles a cassette miss with steps 1 and 2 of strict (spec §6.2).
func (h *LLMHandler) miss(w http.ResponseWriter, start time.Time, res missResult, reason string) {
	res.Channel, res.Upstream = llmChannel, llmUpstream
	res.Diff = []diffItem{}
	msg := "retrace: cassette miss: " + reason
	result, err := json.Marshal(res)
	if err != nil {
		result = nil // a struct of strings and numbers always marshals; unreachable
	}
	h.cfg.Writer.AppendEvent(cassette.Event{
		Kind: "divergence", Source: "retrace", Target: "agent", StartedAt: start,
		Result: result, Status: "divergence", Error: &msg,
		Provenance: "replayed", DeriverVersion: derive.Version,
	})
	h.once.Do(func() { close(h.diverged) })
	h.log.Warn("cassette miss", "channel", llmChannel, "reason", reason)

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

// replayRecorder records a replayed response in the new session as serve
// writes it: each SSE chunk with the offset at which it was served, and the
// completion just before the last write reaches the agent. A request the agent
// sends after reading the response is then always queued after this
// completion, as in record mode (spec §7.4). As there, a last write that fails
// leaves the interaction recorded as complete. It is used by one request's
// goroutine only.
type replayRecorder struct {
	h           *LLMHandler
	src         cassette.Interaction
	id          int64
	start       time.Time
	body        []byte // the non-streamed body, as served
	chunks      int    // chunks recorded so far
	headerAt    time.Time
	wroteHeader bool
	done        bool
}

func (r *replayRecorder) headers(empty bool) {
	r.wroteHeader, r.headerAt = true, r.h.clock.Now()
	if empty {
		r.complete(nil)
	}
}

func (r *replayRecorder) write(data []byte, last bool) {
	if r.src.Streamed {
		r.h.cfg.Writer.AppendChunk(cassette.Chunk{
			InteractionID: r.id, Index: r.chunks,
			OffsetMillis: r.h.clock.Now().Sub(r.headerAt).Milliseconds(), Data: data,
		})
		r.chunks++
	} else {
		r.body = data
	}
	if last {
		r.complete(nil)
	}
}

// complete queues the completion of the replayed interaction once. errMsg
// says why the agent did not get the whole response, if it did not.
func (r *replayRecorder) complete(errMsg *string) {
	if r.done {
		return
	}
	r.done = true
	now := r.h.clock.Now()
	done := cassette.Completion{DurationMillis: now.Sub(r.start).Milliseconds(), Error: errMsg}
	var blobs [][]byte
	if r.wroteHeader {
		ttfb := r.headerAt.Sub(r.start).Milliseconds()
		done.StatusCode, done.ResponseHeaders, done.TTFBMillis = r.src.StatusCode, r.src.ResponseHeaders, &ttfb
		done.Streamed = r.src.Streamed
		if !r.src.Streamed && errMsg == nil {
			body := r.body
			if body == nil {
				body = []byte{} // an empty body is stored as an empty blob, not NULL
			}
			key := cassette.BlobKey(body)
			done.ResponseBlob = &key
			blobs = append(blobs, body)
		}
	}
	r.h.cfg.Writer.CompleteInteraction(r.id, done, blobs...)
}
