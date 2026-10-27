// Package proxy is retrace's local reverse proxy (ADR 0002). It routes each
// request by path to its upstream, forwards it, streams the response back
// without buffering, and records the exchange in a cassette (spec §3).
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/redact"
)

// Route maps a path prefix of the local listener to an upstream.
type Route struct {
	Prefix   string   // "/llm/anthropic" or "/mcp/<name>"
	Channel  string   // llm or mcp (spec §7.3)
	Upstream string   // anthropic or mcp:<name>
	Target   *url.URL // where requests under Prefix are forwarded
}

// Config configures a Proxy.
type Config struct {
	Routes    []Route
	Writer    *cassette.Writer
	Redact    *redact.Rules
	Logger    *slog.Logger
	Transport http.RoundTripper // nil selects http.DefaultTransport

	// MaxBodyBytes caps how much of one response, or of one SSE event, is
	// held in memory to be recorded. Larger ones are still forwarded, and the
	// interaction is recorded with an error instead. Zero selects
	// cassette.DefaultQueueBytes: the writer would drop anything larger anyway.
	MaxBodyBytes int64
}

// Proxy is an http.Handler that forwards and records.
type Proxy struct {
	routes  []route
	writer  *cassette.Writer
	redact  *redact.Rules
	log     *slog.Logger
	maxBody int64
}

type route struct {
	Route
	rp *httputil.ReverseProxy
}

// New returns a Proxy for cfg.
func New(cfg Config) *Proxy {
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	maxBody := cfg.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = cassette.DefaultQueueBytes
	}
	p := &Proxy{writer: cfg.Writer, redact: cfg.Redact, log: log, maxBody: maxBody}
	for _, r := range cfg.Routes {
		target := r.Target
		p.routes = append(p.routes, route{Route: r, rp: &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(target)
				// Without this header the transport asks for gzip itself and
				// decodes it, so the agent and the cassette both get plain
				// bytes (spec §3.3).
				pr.Out.Header.Del("Accept-Encoding")
			},
			// Flush every write: the agent gets each SSE event as soon as it
			// arrives (AC-03).
			FlushInterval:  -1,
			Transport:      cfg.Transport,
			ModifyResponse: p.onResponse,
			ErrorHandler:   p.onError,
			ErrorLog:       slog.NewLogLogger(log.Handler(), slog.LevelDebug),
		}})
	}
	return p
}

// ServeHTTP routes r by path prefix, then forwards and records it.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rt, rest, ok := p.match(r.URL.Path)
	if !ok {
		http.Error(w, fmt.Sprintf("retrace: no route for %s", r.URL.Path), http.StatusNotFound)
		return
	}
	start := time.Now()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "retrace: reading request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	path := rest
	if r.URL.RawQuery != "" {
		path += "?" + p.redact.Query(r.URL.RawQuery) // spec §3.5
	}
	in := cassette.Interaction{
		Channel: rt.Channel, Upstream: rt.Upstream, HTTPMethod: r.Method, Path: path,
		RequestHeaders: p.headersJSON(r.Header), StartedAt: start, Provenance: "recorded",
	}
	ex := &exchange{p: p, start: start}
	var blobs [][]byte
	if len(body) > 0 {
		// spec §3.5: only the stored copy is redacted; the upstream gets body.
		if stored, err := p.redact.Body(body); err != nil {
			ex.notes = append(ex.notes, "the request body was forwarded but not recorded: "+err.Error())
		} else {
			key := cassette.BlobKey(stored)
			in.RequestBlob = &key
			blobs = append(blobs, stored)
		}
	}
	ex.id = p.writer.BeginInteraction(in, blobs...)

	out := r.Clone(context.WithValue(r.Context(), exchangeKey{}, ex))
	out.URL.Path, out.URL.RawPath = rest, ""
	rt.rp.ServeHTTP(w, out)
}

// match finds the route for path and returns the path relative to it.
func (p *Proxy) match(path string) (route, string, bool) {
	for _, rt := range p.routes {
		if rest, ok := strings.CutPrefix(path, rt.Prefix); ok && (rest == "" || rest[0] == '/') {
			if rest == "" {
				rest = "/"
			}
			return rt, rest, true
		}
	}
	return route{}, "", false
}

func (p *Proxy) headersJSON(h http.Header) json.RawMessage {
	b, err := json.Marshal(p.redact.Headers(h))
	if err != nil {
		return json.RawMessage(`{}`) // http.Header always marshals; unreachable
	}
	return b
}

type exchangeKey struct{}

func exchangeOf(r *http.Request) *exchange {
	return r.Context().Value(exchangeKey{}).(*exchange)
}

// onResponse runs when the upstream's response headers arrive. It wraps the
// body so the exchange is recorded as the agent reads it.
func (p *Proxy) onResponse(resp *http.Response) error {
	ex := exchangeOf(resp.Request)
	ex.respStart = time.Now()
	ex.status = resp.StatusCode
	ex.respHeaders = p.headersJSON(resp.Header)
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	ex.streamed = mediaType == "text/event-stream"
	resp.Body = &recordingBody{ReadCloser: resp.Body, ex: ex}
	return nil
}

// onError runs when the upstream cannot be reached or the agent goes away
// before the response arrives.
func (p *Proxy) onError(w http.ResponseWriter, r *http.Request, err error) {
	ex := exchangeOf(r)
	msg := err.Error()
	if errors.Is(err, context.Canceled) {
		msg = "the agent closed the connection before the response arrived"
	}
	ex.fail(msg)
	p.log.Warn("upstream request failed", "interaction", ex.id, "error", err)
	http.Error(w, "retrace: upstream request failed: "+err.Error(), http.StatusBadGateway)
}

// exchange is the recording state of one request.
type exchange struct {
	p           *Proxy
	id          int64
	start       time.Time // request arrival
	respStart   time.Time // response headers received
	status      int
	respHeaders json.RawMessage
	streamed    bool

	// Read and Close of recordingBody run on the proxy's goroutine for this
	// request, so the fields below need no lock; once guards completion.
	sse      sseSplitter
	chunkIdx int
	body     bytes.Buffer // the response body when not streamed
	stopped  bool         // the rest of the response is forwarded but not recorded
	notes    []string     // why the recording is incomplete, for the interaction's error
	eof      bool
	readErr  error
	once     sync.Once
}

// observe records bytes of the response as they pass to the agent.
func (ex *exchange) observe(b []byte) {
	if ex.stopped {
		return
	}
	if !ex.streamed {
		if int64(ex.body.Len()+len(b)) > ex.p.maxBody {
			ex.stop(fmt.Sprintf("the response body exceeded %d bytes; it was forwarded but not recorded", ex.p.maxBody))
			return
		}
		ex.body.Write(b)
		return
	}
	for _, event := range ex.sse.feed(b) {
		if int64(len(event)) > ex.p.maxBody {
			ex.stop(fmt.Sprintf("an SSE event exceeded %d bytes; the rest of the stream was forwarded but not recorded", ex.p.maxBody))
			return
		}
		ex.appendChunk(event)
	}
	if int64(len(ex.sse.rest())) > ex.p.maxBody {
		// No event boundary in sight: a malformed or binary stream.
		ex.stop(fmt.Sprintf("an SSE event exceeded %d bytes; the rest of the stream was forwarded but not recorded", ex.p.maxBody))
	}
}

// stop gives up recording the rest of the response, for reason. The agent
// still gets every byte.
func (ex *exchange) stop(reason string) {
	ex.stopped = true
	ex.notes = append(ex.notes, reason)
	ex.body, ex.sse = bytes.Buffer{}, sseSplitter{}
}

func (ex *exchange) appendChunk(event []byte) {
	data, err := ex.p.redact.SSEEvent(event)
	if err != nil {
		ex.stop("an SSE event could not be redacted; the rest of the stream was forwarded but not recorded: " + err.Error())
		return
	}
	ex.p.writer.AppendChunk(cassette.Chunk{
		InteractionID: ex.id, Index: ex.chunkIdx,
		OffsetMillis: time.Since(ex.respStart).Milliseconds(), Data: data,
	})
	ex.chunkIdx++
}

// finish completes the interaction when the upstream body ends, or when it is
// closed before that.
func (ex *exchange) finish() {
	ex.once.Do(func() {
		ttfb := ex.respStart.Sub(ex.start).Milliseconds()
		done := cassette.Completion{
			StatusCode: &ex.status, ResponseHeaders: ex.respHeaders, Streamed: ex.streamed,
			TTFBMillis: &ttfb, DurationMillis: time.Since(ex.start).Milliseconds(),
		}
		var blobs [][]byte
		switch {
		case ex.stopped:
		case ex.streamed:
			if rest := ex.sse.rest(); len(rest) > 0 {
				ex.appendChunk(rest) // an unterminated last event, kept as received
			}
		default:
			if stored, err := ex.p.redact.Body(ex.body.Bytes()); err != nil {
				ex.notes = append(ex.notes, "the response body was forwarded but not recorded: "+err.Error())
			} else {
				key := cassette.BlobKey(stored)
				done.ResponseBlob = &key
				blobs = append(blobs, stored)
			}
		}
		msgs := incompleteNote(ex.notes)
		switch {
		case errors.Is(ex.readErr, context.Canceled), ex.readErr == nil && !ex.eof:
			// The server cancels the upstream request when the agent leaves.
			msgs = append(msgs, "the agent closed the connection before the response ended")
		case ex.readErr != nil:
			msgs = append(msgs, "reading the upstream response: "+ex.readErr.Error())
		}
		if len(msgs) > 0 {
			msg := strings.Join(msgs, "; ")
			done.Error = &msg
		}
		ex.p.writer.CompleteInteraction(ex.id, done, blobs...)
	})
}

// incompleteNote joins notes, the parts of the exchange that were forwarded
// but not recorded, under the cassette.RecordingIncomplete marker, so that
// replay and reindex know the cassette does not hold what the agent received
// (spec §7.2). It returns nil when every part was recorded.
func incompleteNote(notes []string) []string {
	if len(notes) == 0 {
		return nil
	}
	return []string{cassette.RecordingIncomplete + ": " + strings.Join(notes, "; ")}
}

// fail completes the interaction with no response.
func (ex *exchange) fail(msg string) {
	ex.once.Do(func() {
		full := strings.Join(append(incompleteNote(ex.notes), msg), "; ")
		ex.p.writer.CompleteInteraction(ex.id, cassette.Completion{
			DurationMillis: time.Since(ex.start).Milliseconds(), Error: &full,
		})
	})
}

// recordingBody tees the upstream response into the exchange as the reverse
// proxy copies it to the agent. It adds no buffering: each Read is passed on
// as it is.
type recordingBody struct {
	io.ReadCloser
	ex *exchange
}

func (b *recordingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.ex.observe(p[:n])
	}
	switch {
	case errors.Is(err, io.EOF):
		b.ex.eof = true
		// Complete now, before these last bytes reach the agent: a request it
		// sends after reading the response is then always queued after this
		// completion, and its events after this response's (spec §7.4).
		// The transport often returns the last bytes together with EOF.
		// The cost is deliberate: for a JSON body, finish redacts and hashes
		// it before those bytes are forwarded, which is about a millisecond
		// per megabyte with redaction rules, and far less without (spec §3.5).
		b.ex.finish()
	case err != nil:
		b.ex.readErr = err
	}
	return n, err
}

func (b *recordingBody) Close() error {
	err := b.ReadCloser.Close()
	b.ex.finish()
	return err
}
