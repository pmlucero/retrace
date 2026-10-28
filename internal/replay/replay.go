// Package replay answers requests from a cassette (spec §4). Serve writes one
// recorded response back to the agent; deciding which recorded interaction
// answers a request is request matching (spec §5).
package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
)

// ErrUnservable reports that an interaction holds no complete response to
// replay: it has no valid status or no headers, its body was not recorded, or
// its recording is marked incomplete (spec §7.3). Serve returns it before
// writing anything, so the caller can still answer. A recorded transport error
// is one such interaction: on the LLM channel, LLMHandler answers it with the
// proxy's 502 rather than through Serve (spec §5 step 2).
var ErrUnservable = errors.New("interaction has no recorded response to replay")

// Source reads what Serve needs from the cassette being replayed.
// *cassette.Cassette implements it.
type Source interface {
	Chunks(ctx context.Context, interactionID int64) ([]cassette.Chunk, error)
	Blob(ctx context.Context, key string) ([]byte, error)
}

var _ Source = (*cassette.Cassette)(nil)

// Options configures Serve.
type Options struct {
	// Realtime emits each SSE chunk at its recorded offset from the response
	// headers (--realtime), instead of immediately.
	Realtime bool
	// Clock times realtime replay. Nil selects the system clock.
	Clock Clock
}

// Serve writes the recorded response of in to w: its status, its headers and
// then its body, or its SSE chunks byte for byte, flushing after each one
// (spec §4.1, AC-10). It returns when the response is written, when writing
// to the agent fails, or when ctx is done.
func Serve(ctx context.Context, w http.ResponseWriter, src Source, in cassette.Interaction, opts Options) error {
	return serve(ctx, w, src, in, opts, noObserver{})
}

// observer sees a response as serve writes it, each step just before it
// reaches the agent. The LLM handler records the replayed interaction with it.
type observer interface {
	// headers runs before the status is written. empty reports that no body
	// or chunk follows.
	headers(empty bool)
	// write runs before the body or a chunk is written. last reports the final
	// write of the response.
	write(data []byte, last bool)
}

type noObserver struct{}

func (noObserver) headers(bool)       {}
func (noObserver) write([]byte, bool) {}

func serve(ctx context.Context, w http.ResponseWriter, src Source, in cassette.Interaction, opts Options, obs observer) error {
	if err := servable(in); err != nil {
		return err
	}
	var recorded http.Header
	if err := json.Unmarshal(in.ResponseHeaders, &recorded); err != nil {
		return fmt.Errorf("serving interaction %d: response headers: %w", in.ID, err)
	}

	// Read everything before writing the headers, so a cassette error can
	// still be answered with an error status. The cost is one whole response
	// in memory; reading chunks as they are served would bound memory but
	// could fail after the status is sent.
	var (
		body   []byte
		chunks []cassette.Chunk
		err    error
	)
	if in.Streamed {
		if chunks, err = src.Chunks(ctx, in.ID); err != nil {
			return fmt.Errorf("serving interaction %d: %w", in.ID, err)
		}
	} else if body, err = src.Blob(ctx, *in.ResponseBlob); err != nil {
		return fmt.Errorf("serving interaction %d: %w", in.ID, err)
	}

	h := w.Header()
	copyHeaders(h, recorded)
	if !in.Streamed && len(body) > 0 {
		h.Set("Content-Length", strconv.Itoa(len(body)))
	}
	empty := len(body) == 0
	if in.Streamed {
		empty = len(chunks) == 0
	}
	obs.headers(empty)
	w.WriteHeader(*in.StatusCode)
	if !in.Streamed {
		if len(body) == 0 {
			return nil
		}
		obs.write(body, true)
		if _, err := w.Write(body); err != nil {
			return fmt.Errorf("serving interaction %d: writing body: %w", in.ID, err)
		}
		return nil
	}
	return serveChunks(ctx, w, in.ID, chunks, opts, obs)
}

// servable returns an error wrapping ErrUnservable when Serve would refuse in
// without reading the cassette.
func servable(in cassette.Interaction) error {
	// The cassette is untrusted input: WriteHeader panics on a code outside
	// 100-999, and a 1xx code is not a final response.
	if in.StatusCode == nil || *in.StatusCode < 200 || *in.StatusCode > 999 || in.ResponseHeaders == nil {
		return fmt.Errorf("serving interaction %d: %w", in.ID, ErrUnservable)
	}
	// Matching keeps incomplete recordings out of its queues (spec §5); this
	// check keeps a truncated stream from being served as a complete one.
	if in.Error != nil && strings.Contains(*in.Error, cassette.RecordingIncomplete) {
		return fmt.Errorf("serving interaction %d: %s: %w", in.ID, cassette.RecordingIncomplete, ErrUnservable)
	}
	if !in.Streamed && in.ResponseBlob == nil {
		return fmt.Errorf("serving interaction %d: %w", in.ID, ErrUnservable)
	}
	return nil
}

// serveChunks writes chunks after the headers were written, flushing the
// headers first: recorded offsets count from when they arrived (spec §3.3).
func serveChunks(ctx context.Context, w http.ResponseWriter, id int64, chunks []cassette.Chunk, opts Options, obs observer) error {
	rc := http.NewResponseController(w)
	if err := rc.Flush(); err != nil {
		return fmt.Errorf("serving interaction %d: flushing headers: %w", id, err)
	}
	clock := opts.Clock
	if clock == nil {
		clock = systemClock{}
	}
	start := clock.Now()
	for i, ch := range chunks {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("serving interaction %d: chunk %d: %w", id, ch.Index, err)
		}
		if opts.Realtime {
			at := time.Duration(ch.OffsetMillis) * time.Millisecond
			if d := at - clock.Now().Sub(start); d > 0 {
				if err := clock.Sleep(ctx, d); err != nil {
					return fmt.Errorf("serving interaction %d: chunk %d: %w", id, ch.Index, err)
				}
			}
		}
		obs.write(ch.Data, i == len(chunks)-1)
		if _, err := w.Write(ch.Data); err != nil {
			return fmt.Errorf("serving interaction %d: writing chunk %d: %w", id, ch.Index, err)
		}
		if err := rc.Flush(); err != nil {
			return fmt.Errorf("serving interaction %d: flushing chunk %d: %w", id, ch.Index, err)
		}
	}
	return nil
}

// hopByHop are the headers that describe one connection rather than the
// response (RFC 9110 §7.6.1). The reverse proxy did not pass them to the
// agent, and the replaying server sets its own.
var hopByHop = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// copyHeaders copies the recorded headers into dst, except the hop-by-hop
// ones and Content-Length: the stored body is redacted and may differ in
// length from the one recorded, and a stream has none.
func copyHeaders(dst, recorded http.Header) {
	drop := map[string]bool{"Content-Length": true}
	for _, name := range hopByHop {
		drop[name] = true
	}
	for _, v := range recorded["Connection"] {
		for name := range strings.SplitSeq(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				drop[textproto.CanonicalMIMEHeaderKey(name)] = true
			}
		}
	}
	for name, values := range recorded {
		if drop[textproto.CanonicalMIMEHeaderKey(name)] {
			continue
		}
		dst[name] = append(dst[name], values...)
	}
}
