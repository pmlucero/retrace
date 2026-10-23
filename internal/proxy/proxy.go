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
	Headers   *redact.Headers
	Logger    *slog.Logger
	Transport http.RoundTripper // nil selects http.DefaultTransport
}

// Proxy is an http.Handler that forwards and records.
type Proxy struct {
	routes  []route
	writer  *cassette.Writer
	headers *redact.Headers
	log     *slog.Logger
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
	p := &Proxy{writer: cfg.Writer, headers: cfg.Headers, log: log}
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
		path += "?" + r.URL.RawQuery
	}
	in := cassette.Interaction{
		Channel: rt.Channel, Upstream: rt.Upstream, HTTPMethod: r.Method, Path: path,
		RequestHeaders: p.headersJSON(r.Header), StartedAt: start, Provenance: "recorded",
	}
	var blobs [][]byte
	if len(body) > 0 {
		key := cassette.BlobKey(body)
		in.RequestBlob = &key
		blobs = append(blobs, body)
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
	b, err := json.Marshal(p.headers.Apply(h))
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
	eof      bool
	readErr  error
	once     sync.Once
}

// observe records bytes of the response as they pass to the agent.
func (ex *exchange) observe(b []byte) {
	if !ex.streamed {
		ex.body.Write(b)
		return
	}
	for _, event := range ex.sse.feed(b) {
		ex.appendChunk(event)
	}
}

func (ex *exchange) appendChunk(data []byte) {
	ex.p.writer.AppendChunk(cassette.Chunk{
		InteractionID: ex.id, Index: ex.chunkIdx,
		OffsetMillis: time.Since(ex.respStart).Milliseconds(), Data: data,
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
		if ex.streamed {
			if rest := ex.sse.rest(); len(rest) > 0 {
				ex.appendChunk(rest) // an unterminated last event, kept as received
			}
		} else {
			key := cassette.BlobKey(ex.body.Bytes())
			done.ResponseBlob = &key
			blobs = append(blobs, ex.body.Bytes())
		}
		switch {
		case ex.readErr != nil:
			msg := "reading the upstream response: " + ex.readErr.Error()
			done.Error = &msg
		case !ex.eof:
			msg := "the agent closed the connection before the response ended"
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
