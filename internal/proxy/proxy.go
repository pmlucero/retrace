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
	var blobs [][]byte
	if len(body) > 0 {
		stored := p.redact.Body(body) // spec §3.5; the upstream gets body itself
		key := cassette.BlobKey(stored)
		in.RequestBlob = &key
		blobs = append(blobs, stored)
	}
	ex := &exchange{p: p, start: start}
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
	tooLarge bool         // the body, or one SSE event, exceeded maxBody: stop recording it
	eof      bool
	readErr  error
	once     sync.Once
}

// observe records bytes of the response as they pass to the agent.
func (ex *exchange) observe(b []byte) {
	if ex.tooLarge {
		return
	}
	if !ex.streamed {
		if int64(ex.body.Len()+len(b)) > ex.p.maxBody {
			ex.tooLarge = true
			ex.body = bytes.Buffer{} // release it; the agent still gets every byte
			return
		}
		ex.body.Write(b)
		return
	}
	for _, event := range ex.sse.feed(b) {
		if int64(len(event)) > ex.p.maxBody {
			ex.tooLarge = true
			ex.sse = sseSplitter{}
			return
		}
		ex.appendChunk(event)
	}
	if int64(len(ex.sse.rest())) > ex.p.maxBody {
		ex.tooLarge = true // no event boundary in sight: a malformed or binary stream
		ex.sse = sseSplitter{}
	}
}

func (ex *exchange) appendChunk(event []byte) {
	ex.p.writer.AppendChunk(cassette.Chunk{
		InteractionID: ex.id, Index: ex.chunkIdx,
		OffsetMillis: time.Since(ex.respStart).Milliseconds(), Data: ex.p.redact.SSEEvent(event),
	})
	ex.chunkIdx++
}

// finish completes the interaction once the body is closed.
func (ex *exchange) finish() {
	ex.once.Do(func() {
		ttfb := ex.respStart.Sub(ex.start).Milliseconds()
		done := cassette.Completion{
			StatusCode: &ex.status, ResponseHeaders: ex.respHeaders, Streamed: ex.streamed,
			TTFBMillis: &ttfb, DurationMillis: time.Since(ex.start).Milliseconds(),
		}
		var blobs [][]byte
		switch {
		case ex.tooLarge:
		case ex.streamed:
			if rest := ex.sse.rest(); len(rest) > 0 {
				ex.appendChunk(rest) // an unterminated last event, kept as received
			}
		default:
			stored := ex.p.redact.Body(ex.body.Bytes())
			key := cassette.BlobKey(stored)
			done.ResponseBlob = &key
			blobs = append(blobs, stored)
		}
		var msg string
		switch {
		case ex.tooLarge && ex.streamed:
			msg = fmt.Sprintf("an SSE event exceeded %d bytes; the rest of the stream was forwarded but not recorded", ex.p.maxBody)
		case ex.tooLarge:
			msg = fmt.Sprintf("the response body exceeded %d bytes; it was forwarded but not recorded", ex.p.maxBody)
		case errors.Is(ex.readErr, context.Canceled), ex.readErr == nil && !ex.eof:
			// The server cancels the upstream request when the agent leaves.
			msg = "the agent closed the connection before the response ended"
		case ex.readErr != nil:
			msg = "reading the upstream response: " + ex.readErr.Error()
		}
		if msg != "" {
			done.Error = &msg
		}
		ex.p.writer.CompleteInteraction(ex.id, done, blobs...)
	})
}

// fail completes the interaction with no response.
func (ex *exchange) fail(msg string) {
	ex.once.Do(func() {
		ex.p.writer.CompleteInteraction(ex.id, cassette.Completion{
			DurationMillis: time.Since(ex.start).Milliseconds(), Error: &msg,
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
