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
	rec := &replayRecorder{
		ResponseWriter: w, h: h, src: src, start: start,
		id: h.cfg.Writer.BeginInteraction(in, blobs...),
	}
	err = Serve(r.Context(), rec, &capture{Source: h.cfg.Source, rec: rec}, src, h.cfg.Options)
	switch {
	case err == nil:
		rec.complete(nil) // a no-op unless Serve wrote no body at all
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

// capture is the Source of one served interaction. It keeps what Serve reads,
// so the recorder knows the response before the first byte is written.
type capture struct {
	Source
	rec *replayRecorder
}

func (c *capture) Chunks(ctx context.Context, id int64) ([]cassette.Chunk, error) {
	chunks, err := c.Source.Chunks(ctx, id)
	c.rec.writes = len(chunks)
	return chunks, err
}

func (c *capture) Blob(ctx context.Context, key string) ([]byte, error) {
	b, err := c.Source.Blob(ctx, key)
	c.rec.body = b
	if len(b) > 0 {
		c.rec.writes = 1
	}
	return b, err
}

// replayRecorder records a replayed response in the new session as Serve
// writes it: each SSE chunk with the offset at which it was served, and the
// completion just before the last write reaches the agent. A request the agent
// sends after reading the response is then always queued after this
// completion, as in record mode (spec §7.4). It relies on Serve writing a body
// in one Write and each chunk in its own Write. It is used by one request's
// goroutine only.
type replayRecorder struct {
	http.ResponseWriter
	h           *LLMHandler
	src         cassette.Interaction
	id          int64
	start       time.Time
	body        []byte // the non-streamed body, as read
	writes      int    // writes Serve will make after the headers
	written     int
	headerAt    time.Time
	wroteHeader bool
	done        bool
}

// Unwrap lets http.ResponseController reach the underlying writer to flush.
func (r *replayRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *replayRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.wroteHeader, r.headerAt = true, r.h.clock.Now()
		if r.writes == 0 {
			r.complete(nil)
		}
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *replayRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	if r.src.Streamed && !r.done {
		r.h.cfg.Writer.AppendChunk(cassette.Chunk{
			InteractionID: r.id, Index: r.written,
			OffsetMillis: r.h.clock.Now().Sub(r.headerAt).Milliseconds(), Data: b,
		})
	}
	r.written++
	if r.written >= r.writes {
		r.complete(nil)
	}
	return r.ResponseWriter.Write(b)
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
			key := cassette.BlobKey(r.body)
			done.ResponseBlob = &key
			blobs = append(blobs, r.body)
		}
	}
	r.h.cfg.Writer.CompleteInteraction(r.id, done, blobs...)
}
