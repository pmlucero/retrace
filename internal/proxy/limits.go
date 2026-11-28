package proxy

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/pmlucero/retrace/internal/httpx"
)

// The connection limits of spec §3.6. They are not configurable by users in
// the MVP; Limits exists so tests can shorten them.
const (
	// MaxInFlight is how many requests may be in flight at once.
	MaxInFlight = 64
	// IdleTimeout is how long a connection may carry no request.
	IdleTimeout = 120 * time.Second
	// HeaderTimeout is how long a client has to send a request's headers.
	HeaderTimeout = 30 * time.Second
)

// Limits holds the values behind the connection limits of spec §3.6. The zero
// value of a field selects the spec's value: httpx.BodyReadTimeout, MaxInFlight
// and IdleTimeout. It is an internal option for tests, not a flag.
type Limits struct {
	BodyReadTimeout time.Duration // inactivity allowed while a request body is read
	MaxInFlight     int           // requests in flight before the next gets 503
	IdleTimeout     time.Duration // how long a connection may carry no request
}

func (l Limits) withDefaults() Limits {
	if l.BodyReadTimeout <= 0 {
		l.BodyReadTimeout = httpx.BodyReadTimeout
	}
	if l.MaxInFlight <= 0 {
		l.MaxInFlight = MaxInFlight
	}
	if l.IdleTimeout <= 0 {
		l.IdleTimeout = IdleTimeout
	}
	return l
}

// NewServer returns the HTTP server of a session: next behind GuardLimits,
// with a header timeout and an idle timeout. It sets no overall ReadTimeout or
// WriteTimeout, which would cut a long stream or tool call (spec §3.6, AC-03).
func NewServer(next http.Handler, log *slog.Logger, limits Limits, listenHosts ...string) *http.Server {
	limits = limits.withDefaults()
	return &http.Server{
		Handler:           GuardLimits(next, log, limits, listenHosts...),
		ReadHeaderTimeout: HeaderTimeout,
		IdleTimeout:       limits.IdleTimeout,
	}
}
