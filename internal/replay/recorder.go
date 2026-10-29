package replay

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/derive"
	"github.com/pmlucero/retrace/internal/proxy"
	"github.com/pmlucero/retrace/internal/redact"
)

// Divergence signals the first cassette miss of a replay, on any channel or
// upstream: the replay then ends as diverged (spec §6.2). The handlers of one
// replay share it. It is safe for concurrent use.
type Divergence struct {
	once sync.Once
	ch   chan struct{}
}

// NewDivergence returns a Divergence that has not been signaled.
func NewDivergence() *Divergence { return &Divergence{ch: make(chan struct{})} }

// Done is closed at the first cassette miss.
func (d *Divergence) Done() <-chan struct{} { return d.ch }

func (d *Divergence) signal() { d.once.Do(func() { close(d.ch) }) }

// recorder records what one replay handler answers in the new session: the
// exchanges it serves and the cassette misses (spec §4.4, §6.2 step 1). It
// is safe for concurrent use.
type recorder struct {
	channel, upstream string
	source            Source
	opts              Options
	redact            *redact.Rules
	writer            *cassette.Writer
	clock             Clock
	log               *slog.Logger
	divergence        *Divergence
}

// recorderConfig is what a handler shares with its recorder.
type recorderConfig struct {
	channel, upstream string
	source            Source
	opts              Options
	redact            *redact.Rules
	writer            *cassette.Writer
	log               *slog.Logger
	divergence        *Divergence
}

// newRecorder returns a recorder for cfg. Nil rules select the defaults, a
// nil clock the system clock, a nil logger discards and a nil divergence is
// the recorder's own.
func newRecorder(cfg recorderConfig) *recorder {
	rc := &recorder{channel: cfg.channel, upstream: cfg.upstream, source: cfg.source, opts: cfg.opts,
		redact: cfg.redact, writer: cfg.writer, clock: cfg.opts.Clock, log: cfg.log, divergence: cfg.divergence}
	if rc.redact == nil {
		rc.redact = redact.NewRules(nil, nil)
	}
	if rc.clock == nil {
		rc.clock = systemClock{}
	}
	if rc.log == nil {
		rc.log = slog.New(slog.DiscardHandler)
	}
	if rc.divergence == nil {
		rc.divergence = NewDivergence()
	}
	return rc
}

// exchange is one request a handler answers from the cassette.
type exchange struct {
	r      *http.Request
	start  time.Time
	body   []byte // as received
	stored []byte // body after redaction
	fp     string
	src    cassette.Interaction // the recorded interaction that answers it
}

// transportError reports whether in records a transport error, the upstream
// request having failed with no response, and its recording is complete
// (spec §7.3). Queues.Next never returns an incomplete head (AC-28); the
// check restates the spec condition (§5 step 2).
func transportError(in cassette.Interaction) bool {
	return in.StatusCode == nil && in.Error != nil && !in.Incomplete()
}

// serveTransportError answers r with the 502 the proxy sent when it recorded
// src, and records the exchange in the new session with src's error and no
// status, so it derives the same events as src (spec §5 step 2, AC-29). This
// is what the agent saw while recording, not a miss: the replay goes on.
func (rc *recorder) serveTransportError(w http.ResponseWriter, ex exchange) {
	id := rc.begin(ex)
	// Completed before the answer reaches the agent, as serve does.
	rc.writer.CompleteInteraction(id, cassette.Completion{
		DurationMillis: rc.clock.Now().Sub(ex.start).Milliseconds(), Error: ex.src.Error,
	})
	proxy.UpstreamFailed(w, *ex.src.Error)
}

// serveRecorded answers ex with its recorded interaction and records the
// exchange in the new session.
func (rc *recorder) serveRecorded(w http.ResponseWriter, ex exchange) {
	src := ex.src
	rec := &servedResponse{rc: rc, src: src, start: ex.start, id: rc.begin(ex)}
	err := serve(ex.r.Context(), w, rc.source, src, rc.opts, rec)
	switch {
	case err == nil:
		rec.complete(nil) // a safety net: the headers or the last write already completed it
	case !rec.wroteHeader && errors.Is(err, context.Canceled):
		// The agent left while the cassette was being read.
		msg := "the agent closed the connection before the response arrived"
		rec.complete(&msg)
	case !rec.wroteHeader:
		// The cassette could not be read: nothing was sent yet.
		rc.log.Error("reading the cassette", "interaction", src.ID, "error", err)
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

// begin records the start of ex in the new session and returns its
// interaction id.
func (rc *recorder) begin(ex exchange) int64 {
	r := ex.r
	path := r.URL.Path
	if path == "" {
		path = "/"
	}
	if r.URL.RawQuery != "" {
		path += "?" + rc.redact.Query(r.URL.RawQuery)
	}
	headers, err := json.Marshal(rc.redact.Headers(r.Header))
	if err != nil {
		headers = json.RawMessage(`{}`) // http.Header always marshals; unreachable
	}
	srcID, fp := ex.src.ID, ex.fp
	in := cassette.Interaction{
		Channel: rc.channel, Upstream: rc.upstream, HTTPMethod: r.Method, Path: path,
		RequestHeaders: headers, Fingerprint: &fp, StartedAt: ex.start,
		Provenance: "replayed", SourceInteractionID: &srcID,
	}
	var blobs [][]byte
	if len(ex.body) > 0 {
		key := cassette.BlobKey(ex.stored)
		in.RequestBlob = &key
		blobs = append(blobs, ex.stored)
	}
	return rc.writer.BeginInteraction(in, blobs...)
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

// miss applies step 1 of strict to a cassette miss (spec §6.2): it
// appends a divergence event and signals the divergence. It returns the
// event's error, which says why the request missed.
func (rc *recorder) miss(start time.Time, res missResult, reason string) string {
	msg := "retrace: cassette miss: " + reason
	res.Channel, res.Upstream = rc.channel, rc.upstream
	res.Diff = []diffItem{}
	result, err := json.Marshal(res)
	if err != nil {
		result = nil // a struct of strings and numbers always marshals; unreachable
	}
	rc.writer.AppendEvent(cassette.Event{
		Kind: "divergence", Source: "retrace", Target: "agent", StartedAt: start,
		Result: result, Status: "divergence", Error: &msg,
		Provenance: "replayed", DeriverVersion: derive.Version,
	})
	rc.divergence.signal()
	rc.log.Warn("cassette miss", "channel", rc.channel, "upstream", rc.upstream, "reason", reason)
	return msg
}

// servedResponse records one replayed response in the new session as serve
// writes it: each SSE chunk with the offset at which it was served, and the
// completion just before the last write reaches the agent. A request the agent
// sends after reading the response is then always queued after this
// completion, as in record mode (spec §7.4). As there, each step is recorded
// before its write: a chunk whose write fails is still recorded as served, and
// a last write that fails leaves the interaction recorded as complete. It is
// used by one request's goroutine only.
type servedResponse struct {
	rc          *recorder
	src         cassette.Interaction
	id          int64
	start       time.Time
	body        []byte // the non-streamed body, as served
	chunks      int    // chunks recorded so far
	headerAt    time.Time
	wroteHeader bool
	done        bool
}

func (r *servedResponse) headers(empty bool) {
	r.wroteHeader, r.headerAt = true, r.rc.clock.Now()
	if empty {
		r.complete(nil)
	}
}

func (r *servedResponse) write(data []byte, last bool) {
	if r.src.Streamed {
		r.rc.writer.AppendChunk(cassette.Chunk{
			InteractionID: r.id, Index: r.chunks,
			OffsetMillis: r.rc.clock.Now().Sub(r.headerAt).Milliseconds(), Data: data,
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
func (r *servedResponse) complete(errMsg *string) {
	if r.done {
		return
	}
	r.done = true
	now := r.rc.clock.Now()
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
	r.rc.writer.CompleteInteraction(r.id, done, blobs...)
}
