package mcptest

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

var echo = Tool{
	Name: "echo",
	Handler: func(_ context.Context, args json.RawMessage) (string, error) {
		var a struct{ Text string }
		if err := json.Unmarshal(args, &a); err != nil {
			return "", err
		}
		if a.Text == "" {
			return "", errors.New("text is required")
		}
		return a.Text, nil
	},
}

// reply is a response with its body read.
type reply struct {
	StatusCode int
	Header     http.Header
}

// post sends body with headers h and returns the response and its body.
func post(t *testing.T, url, body string, h map[string]string) (reply, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range h {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return reply{resp.StatusCode, resp.Header}, string(b)
}

func send(t *testing.T, method, url string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(SessionHeader, "mcptest-session-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

const initReq = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`

func TestInitialize(t *testing.T) {
	s := New(t, Config{ProtocolVersion: "2025-03-26", Name: "weather", Version: "1.2.3"})
	resp, body := post(t, s.URL, initReq, nil)
	if got := resp.Header.Get(SessionHeader); got != "mcptest-session-1" {
		t.Errorf("session header = %q", got)
	}
	want := `{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"tools":{}},"protocolVersion":"2025-03-26","serverInfo":{"name":"weather","version":"1.2.3"}}}`
	if body != want {
		t.Errorf("body = %s, want %s", body, want)
	}
}

func TestMessages(t *testing.T) {
	tests := []struct {
		name, body string
		status     int
		want       string
	}{
		{"tools/list", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, 200,
			`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"echo","inputSchema":{"type":"object"}}]}}`},
		{"tool result", `{"jsonrpc":"2.0","id":"a","method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`, 200,
			`{"jsonrpc":"2.0","id":"a","result":{"content":[{"text":"hi","type":"text"}],"isError":false}}`},
		{"tool error", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{}}}`, 200,
			`{"jsonrpc":"2.0","id":3,"result":{"content":[{"text":"text is required","type":"text"}],"isError":true}}`},
		{"unknown tool", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"nope"}}`, 200,
			`{"jsonrpc":"2.0","id":4,"error":{"code":-32602,"message":"unknown tool: nope"}}`},
		{"unknown method", `{"jsonrpc":"2.0","id":5,"method":"resources/list"}`, 200,
			`{"jsonrpc":"2.0","id":5,"error":{"code":-32601,"message":"method not found: resources/list"}}`},
		{"ping", `{"jsonrpc":"2.0","id":6,"method":"ping"}`, 200, `{"jsonrpc":"2.0","id":6,"result":{}}`},
		{"notification", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, 202, ``},
		{"client response", `{"jsonrpc":"2.0","id":7,"result":{}}`, 202, ``},
		{"batch", `[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/initialized"},{"jsonrpc":"2.0","id":2,"method":"ping"}]`, 200,
			`[{"jsonrpc":"2.0","id":1,"result":{}},{"jsonrpc":"2.0","id":2,"result":{}}]`},
		{"notification batch", `[{"jsonrpc":"2.0","method":"notifications/initialized"}]`, 202, ``},
		{"parse error", `{`, 400, `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"parse error"}}`},
		{"empty batch", `[]`, 400, `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"empty batch"}}`},
	}
	s := New(t, Config{Tools: []Tool{echo}})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := post(t, s.URL, tt.body, nil)
			if resp.StatusCode != tt.status || body != tt.want {
				t.Errorf("got %d %s, want %d %s", resp.StatusCode, body, tt.status, tt.want)
			}
		})
	}
}

// events parses an SSE body into its data payloads.
func events(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			out = append(out, data)
		}
	}
	return out
}

func TestSSE(t *testing.T) {
	s := New(t, Config{Mode: SSE, LogEvents: 1, EventDelay: 10 * time.Millisecond})
	start := time.Now()
	resp, body := post(t, s.URL, `[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","id":2,"method":"ping"}]`, nil)
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content type = %q", ct)
	}
	got := events(t, body)
	log := `{"jsonrpc":"2.0","method":"notifications/message","params":{"data":"event 1","level":"info"}}`
	want := []string{log, `{"jsonrpc":"2.0","id":1,"result":{}}`, log, `{"jsonrpc":"2.0","id":2,"result":{}}`}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("events = %q, want %q", got, want)
	}
	if d := time.Since(start); d < 30*time.Millisecond {
		t.Errorf("stream took %v, want at least three delays", d)
	}
}

func TestMethodModes(t *testing.T) {
	s := New(t, Config{Mode: SSE, MethodModes: map[string]Mode{"ping": JSON, "tools/list": SSE}})
	resp, _ := post(t, s.URL, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, nil)
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("ping content type = %q", ct)
	}
	resp, body := post(t, s.URL, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, nil)
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" || len(events(t, body)) != 1 {
		t.Errorf("tools/list = %q %q", ct, body)
	}
}

func TestStrictSession(t *testing.T) {
	s := New(t, Config{Strict: true})
	ping := `{"jsonrpc":"2.0","id":2,"method":"ping"}`
	resp, _ := post(t, s.URL, initReq, nil)
	id := resp.Header.Get(SessionHeader)
	tests := []struct {
		name   string
		h      map[string]string
		status int
	}{
		{"no session", map[string]string{ProtocolVersionHeader: DefaultProtocolVersion}, 400},
		{"unknown session", map[string]string{SessionHeader: "other", ProtocolVersionHeader: DefaultProtocolVersion}, 404},
		{"no protocol version", map[string]string{SessionHeader: id}, 400},
		{"valid", map[string]string{SessionHeader: id, ProtocolVersionHeader: DefaultProtocolVersion}, 200},
	}
	for _, tt := range tests {
		resp, _ := post(t, s.URL, ping, tt.h)
		if resp.StatusCode != tt.status {
			t.Errorf("%s: status = %d, want %d", tt.name, resp.StatusCode, tt.status)
		}
	}
	if got := send(t, http.MethodDelete, s.URL); got != http.StatusOK {
		t.Errorf("DELETE = %d", got)
	}
	resp, _ = post(t, s.URL, ping, map[string]string{SessionHeader: id, ProtocolVersionHeader: DefaultProtocolVersion})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("after DELETE: status = %d, want 404", resp.StatusCode)
	}
	if got := send(t, http.MethodDelete, s.URL); got != http.StatusNotFound {
		t.Errorf("second DELETE = %d, want 404", got)
	}
}

func TestGet(t *testing.T) {
	s := New(t, Config{})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, s.URL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "POST, DELETE" {
		t.Errorf("GET = %d, Allow %q, want 405, POST, DELETE", resp.StatusCode, resp.Header.Get("Allow"))
	}
}

// Parallel tool calls run concurrently and are all recorded (AC-13).
func TestConcurrentCalls(t *testing.T) {
	const n = 8
	var started sync.WaitGroup
	started.Add(n)
	block := Tool{Name: "block", Handler: func(context.Context, json.RawMessage) (string, error) {
		started.Done()
		started.Wait() // returns only once every call is in flight
		return "ok", nil
	}}
	s := New(t, Config{Tools: []Tool{block}})
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"block"}}`)
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, s.URL, body)
			if err != nil {
				t.Error(err)
				return
			}
			req.Header.Set("Accept", "application/json, text/event-stream")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			_ = resp.Body.Close()
		})
	}
	wg.Wait()
	got := s.Received()
	if len(got) != n {
		t.Fatalf("received %d requests, want %d", len(got), n)
	}
	for _, r := range got {
		if r.Method != http.MethodPost || r.Header.Get("Accept") == "" || !strings.Contains(string(r.Body), `"block"`) {
			t.Errorf("unexpected record %+v", r)
		}
	}
}

// One server holds several sessions at once, each with its own id, and ending
// one leaves the other usable.
func TestTwoSessions(t *testing.T) {
	s := New(t, Config{Strict: true})
	r1, _ := post(t, s.URL, initReq, nil)
	r2, _ := post(t, s.URL, initReq, nil)
	id1, id2 := r1.Header.Get(SessionHeader), r2.Header.Get(SessionHeader)
	if id1 != "mcptest-session-1" || id2 != "mcptest-session-2" {
		t.Fatalf("session ids = %q, %q", id1, id2)
	}
	if got := send(t, http.MethodDelete, s.URL); got != http.StatusOK {
		t.Fatalf("DELETE = %d", got)
	}
	ping := `{"jsonrpc":"2.0","id":2,"method":"ping"}`
	for id, want := range map[string]int{id1: 404, id2: 200} {
		resp, _ := post(t, s.URL, ping, map[string]string{SessionHeader: id, ProtocolVersionHeader: DefaultProtocolVersion})
		if resp.StatusCode != want {
			t.Errorf("session %s: status = %d, want %d", id, resp.StatusCode, want)
		}
	}
}
