package httpxtest

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// LockedBuffer is a bytes buffer that logging goroutines and a test can use at
// the same time.
type LockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

// Write appends p.
func (l *LockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// String returns what was written.
func (l *LockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// LimitsParams are the shortened connection limits of a CheckLimits case
// (spec §3.6).
type LimitsParams struct {
	BodyTimeout time.Duration
	Idle        time.Duration
	MaxInFlight int
}

// LimitsEnv is a running server with the limits of LimitsParams, in front of
// a mode of retrace that answers the LLM route: record or exact.
type LimitsEnv struct {
	// Addr is the server's host:port, and Path the LLM path to post to.
	Addr, Path string
	// OKBody is a request body, at least 10 bytes, that is answered 200.
	OKBody string
	// SSEBody is a request body answered with an SSE response whose events are
	// SSEGap apart, and SSEWant the events, in order.
	SSEBody string
	SSEGap  time.Duration
	SSEWant []string
	// Forwarded counts the requests that reached an upstream. Exact mode has
	// none.
	Forwarded func() int
	// Recorded ends the session and counts the interactions it recorded. It
	// is called last.
	Recorded func() int
	// Logs returns what the server logged.
	Logs func() string
}

// CheckLimits runs the cases of AC-53 against a server that newEnv builds
// with the given limits: a stalled body, a slow body that keeps arriving, the
// in-flight limit, the idle timeout and a long SSE response. Each case checks
// that the rejected requests were neither forwarded nor recorded, and that
// the warning of a rejection leaves out the path and query.
func CheckLimits(t *testing.T, newEnv func(t *testing.T, p LimitsParams) LimitsEnv) {
	t.Helper()
	cases := []struct {
		name   string
		params LimitsParams
		run    func(t *testing.T, e LimitsEnv, p LimitsParams)
		served int // requests answered 200, the only ones forwarded and recorded
	}{
		{"stalled body", LimitsParams{BodyTimeout: 300 * time.Millisecond}, checkStalled, 0},
		{"slow body that keeps arriving", LimitsParams{BodyTimeout: 600 * time.Millisecond}, checkSlow, 1},
		{"in-flight limit", LimitsParams{BodyTimeout: 2 * time.Second, MaxInFlight: 2}, checkInFlight, 1},
		{"idle connection", LimitsParams{BodyTimeout: time.Second, Idle: 300 * time.Millisecond}, checkIdle, 1},
		{"SSE longer than the deadline", LimitsParams{BodyTimeout: 300 * time.Millisecond}, checkSSE, 1},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if tt.params.Idle == 0 {
				tt.params.Idle = time.Minute
			}
			if tt.params.MaxInFlight == 0 {
				tt.params.MaxInFlight = 64
			}
			e := newEnv(t, tt.params)
			tt.run(t, e, tt.params)
			if n := e.Forwarded(); n > tt.served {
				t.Errorf("%d requests forwarded, want at most %d", n, tt.served)
			}
			if n := e.Recorded(); n != tt.served {
				t.Errorf("%d interactions recorded, want %d", n, tt.served)
			}
			for _, line := range strings.Split(e.Logs(), "\n") {
				if !strings.Contains(line, "request rejected") {
					continue
				}
				for _, leak := range []string{limitsSecret, "/v1/messages", "token="} {
					if strings.Contains(line, leak) {
						t.Errorf("rejection warning %q leaks %q", line, leak)
					}
				}
			}
		})
	}
}

// limitsSecret stands for a credential in the query of a rejected request.
const limitsSecret = "s3cr3t-token"

// dial opens a connection that fails the test if it stays silent too long.
func dial(t *testing.T, e LimitsEnv) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", e.Addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, bufio.NewReader(conn)
}

// send writes a POST whose headers announce body, then the first n bytes of it.
func send(t *testing.T, conn net.Conn, e LimitsEnv, query, body string, n int) {
	t.Helper()
	path := e.Path
	if query != "" {
		path += "?" + query
	}
	_, err := fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
		path, e.Addr, len(body), body[:n])
	if err != nil {
		t.Fatal(err)
	}
}

// reply is what a test needs of a response.
type reply struct {
	status int
	close  bool // the server announced Connection: close
}

// answer reads the response to the last request, within 10 seconds.
func answer(t *testing.T, conn net.Conn, br *bufio.Reader) (reply, string) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response body: %v", err)
	}
	return reply{resp.StatusCode, resp.Close}, string(b)
}

// wantRejected checks a 408 or 503 of spec §3.6: the status, the Anthropic
// error type, Connection: close and a connection that the server closed.
func wantRejected(t *testing.T, conn net.Conn, br *bufio.Reader, status int, errType string) {
	t.Helper()
	resp, body := answer(t, conn, br)
	if resp.status != status || !resp.close {
		t.Errorf("got %d, close %v; want %d with Connection: close", resp.status, resp.close, status)
	}
	var got struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil || got.Type != "error" || got.Error.Type != errType ||
		!strings.HasPrefix(got.Error.Message, "retrace: ") {
		t.Errorf("body %q (%v), want an Anthropic %s error", body, err, errType)
	}
	wantClosed(t, conn, br)
}

// wantClosed checks that the server closed the connection, not just that it
// stayed silent.
func wantClosed(t *testing.T, conn net.Conn, br *bufio.Reader) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := br.ReadByte(); !errors.Is(err, io.EOF) {
		t.Errorf("connection read: %v, want it closed (EOF)", err)
	}
}

func wantOK(t *testing.T, conn net.Conn, br *bufio.Reader) {
	t.Helper()
	if resp, body := answer(t, conn, br); resp.status != http.StatusOK || body == "" {
		t.Errorf("got %d %q, want 200 and a body", resp.status, body)
	}
}

func checkStalled(t *testing.T, e LimitsEnv, p LimitsParams) {
	conn, br := dial(t, e)
	start := time.Now()
	send(t, conn, e, "token="+limitsSecret, e.OKBody, 5)
	wantRejected(t, conn, br, http.StatusRequestTimeout, "invalid_request_error")
	if d := time.Since(start); d < p.BodyTimeout/2 {
		t.Errorf("answered after %v, before the %v deadline", d, p.BodyTimeout)
	}
}

// checkSlow sends a body in ten parts whose total time is longer than the
// deadline, though no gap is.
func checkSlow(t *testing.T, e LimitsEnv, p LimitsParams) {
	conn, br := dial(t, e)
	send(t, conn, e, "", e.OKBody, 0)
	step := (len(e.OKBody) + 9) / 10
	start := time.Now()
	for i := 0; i < len(e.OKBody); i += step {
		time.Sleep(p.BodyTimeout / 6)
		if _, err := io.WriteString(conn, e.OKBody[i:min(i+step, len(e.OKBody))]); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d <= p.BodyTimeout {
		t.Fatalf("the body took %v, no longer than the %v deadline", d, p.BodyTimeout)
	}
	wantOK(t, conn, br)
}

func checkInFlight(t *testing.T, e LimitsEnv, p LimitsParams) {
	// Stalled bodies hold their places. Open them until one is refused at
	// once: exactly MaxInFlight must have kept silent.
	type held struct {
		conn net.Conn
		br   *bufio.Reader
	}
	var holders []held
	refused := false
	for i := 0; i < p.MaxInFlight+3 && !refused; i++ {
		conn, br := dial(t, e)
		send(t, conn, e, "token="+limitsSecret, e.OKBody, 1)
		_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		if _, err := br.Peek(1); err == nil {
			wantRejected(t, conn, br, http.StatusServiceUnavailable, "overloaded_error")
			refused = true
		} else if errors.Is(err, os.ErrDeadlineExceeded) {
			holders = append(holders, held{conn, br})
		} else {
			t.Fatalf("connection read: %v", err)
		}
	}
	if !refused || len(holders) != p.MaxInFlight {
		t.Fatalf("%d requests held a place, refused %v; want %d and true", len(holders), refused, p.MaxInFlight)
	}

	// A holder is rejected after the deadline and frees its place.
	wantRejected(t, holders[0].conn, holders[0].br, http.StatusRequestTimeout, "invalid_request_error")
	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, br := dial(t, e)
		send(t, conn, e, "", e.OKBody, len(e.OKBody))
		resp, body := answer(t, conn, br)
		if resp.status == http.StatusOK && body != "" {
			return
		}
		if resp.status != http.StatusServiceUnavailable || time.Now().After(deadline) {
			t.Fatalf("after a rejection got %d %q, want 200", resp.status, body)
		}
		time.Sleep(20 * time.Millisecond) // the holder's handler is still returning
	}
}

func checkIdle(t *testing.T, e LimitsEnv, p LimitsParams) {
	conn, br := dial(t, e)
	send(t, conn, e, "", e.OKBody, len(e.OKBody))
	wantOK(t, conn, br)
	start := time.Now()
	wantClosed(t, conn, br)
	if d := time.Since(start); d < p.Idle/2 {
		t.Errorf("closed after %v, before the %v idle timeout", d, p.Idle)
	}
}

// checkSSE reads a response whose events are further apart than the body-read
// deadline: the first must arrive on its own, before the second exists.
func checkSSE(t *testing.T, e LimitsEnv, p LimitsParams) {
	if e.SSEGap <= p.BodyTimeout {
		t.Fatalf("SSE gap %v, want longer than the %v deadline", e.SSEGap, p.BodyTimeout)
	}
	conn, br := dial(t, e)
	start := time.Now()
	send(t, conn, e, "", e.SSEBody, len(e.SSEBody))
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("response %v, error %v, want 200", resp, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var got strings.Builder
	var first time.Duration
	for sc := bufio.NewScanner(resp.Body); sc.Scan(); {
		got.WriteString(sc.Text() + "\n")
		if first == 0 && sc.Text() == "" {
			first = time.Since(start) // the first event is complete
		}
	}
	if first == 0 || first >= e.SSEGap/2 {
		t.Errorf("first event after %v, want it unbuffered, well before %v", first, e.SSEGap)
	}
	if total := time.Since(start); total < e.SSEGap {
		t.Errorf("the stream took %v, less than its %v gap", total, e.SSEGap)
	}
	for _, want := range e.SSEWant {
		if !strings.Contains(got.String(), want) {
			t.Errorf("stream %q lacks %q", got.String(), want)
		}
	}
}
