package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/mcp/mcptest"
)

// The MCP servers the MCP helper agent talks to, by registered name.
var mcpAgentServers = []string{"orders", "weather"}

// mcpExchange is one HTTP exchange of the MCP helper agent, as it saw it.
type mcpExchange struct {
	Server      string
	Method      string
	Request     string
	Status      int
	ContentType string
	SessionID   string // Mcp-Session-Id of the response
	Body        string // the response body, exactly as received
	// SpreadMillis is the time between the first bytes of the body and its
	// end: a buffered SSE response arrives all at once.
	SpreadMillis int64
}

// runMCPAgent is the MCP part of the helper agent. Against each server it
// initializes a session, lists and calls tools (also in a batch), opens the
// GET stream and ends the session, then prints what it saw as JSON.
func runMCPAgent() {
	var seen []mcpExchange
	for _, name := range mcpAgentServers {
		url := os.Getenv(mcpEnvName(name))
		var session, version string
		send := func(method, body string) {
			ex, err := mcpAgentCall(url, method, body, session, version)
			if err != nil {
				fmt.Fprintln(os.Stderr, "agent MCP request failed:", err)
				os.Exit(99)
			}
			ex.Server = name
			seen = append(seen, ex)
		}
		send(http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"helper","version":"1"}}}`)
		session = seen[len(seen)-1].SessionID
		if m := regexp.MustCompile(`"protocolVersion":"([^"]+)"`).FindStringSubmatch(seen[len(seen)-1].Body); m != nil {
			version = m[1]
		}
		send(http.MethodPost, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
		send(http.MethodPost, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
		send(http.MethodPost, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"lookup","arguments":{"key":"A1"}}}`)
		send(http.MethodPost, `[{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"lookup","arguments":{"key":"B2"}}},{"jsonrpc":"2.0","id":"p","method":"ping"}]`)
		send(http.MethodGet, "")
		send(http.MethodDelete, "")
	}
	_ = json.NewEncoder(os.Stdout).Encode(seen)
	os.Exit(0)
}

func mcpAgentCall(url, method, body, session, version string) (mcpExchange, error) {
	ex := mcpExchange{Method: method, Request: body}
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	// The URL is the proxy that retrace injected: calling it is the point.
	req, err := http.NewRequestWithContext(context.Background(), method, url, rd) //nolint:gosec // G704
	if err != nil {
		return ex, err
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if session != "" {
		req.Header.Set(mcptest.SessionHeader, session)
		req.Header.Set(mcptest.ProtocolVersionHeader, version)
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: see above
	if err != nil {
		return ex, err
	}
	defer resp.Body.Close()
	ex.Status, ex.ContentType = resp.StatusCode, resp.Header.Get("Content-Type")
	ex.SessionID = resp.Header.Get(mcptest.SessionHeader)
	var got bytes.Buffer
	var first time.Time
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 && first.IsZero() {
			first = time.Now()
		}
		got.Write(buf[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			return ex, err
		}
	}
	if !first.IsZero() {
		ex.SpreadMillis = time.Since(first).Milliseconds()
	}
	ex.Body = got.String()
	return ex, nil
}

func lookupTool() mcptest.Tool {
	return mcptest.Tool{Name: "lookup", Handler: func(_ context.Context, args json.RawMessage) (string, error) {
		return "found " + string(args), nil
	}}
}

// AC-04
func TestRecordMCPOverStreamableHTTP(t *testing.T) {
	t.Setenv(agentModeEnv, "mcp")
	dir := t.TempDir()
	up := fakeAnthropic(t)
	const eventDelay = 100 * time.Millisecond
	servers := map[string]*mcptest.Server{
		"orders": mcptest.New(t, mcptest.Config{Name: "orders", Version: "1.0.0", Strict: true,
			Tools: []mcptest.Tool{lookupTool()}}),
		"weather": mcptest.New(t, mcptest.Config{Name: "weather", Version: "2.1.0", ProtocolVersion: "2025-03-26",
			Strict: true, Mode: mcptest.SSE, LogEvents: 1, EventDelay: eventDelay, Tools: []mcptest.Tool{lookupTool()}}),
	}

	args := []string{"--session", "mcp", "--listen", "127.0.0.1:0", "--anthropic-upstream", up.URL}
	for _, name := range mcpAgentServers {
		args = append(args, "--mcp", name+"="+servers[name].URL)
	}
	args = append(args, "--mcp", "idle=http://127.0.0.1:9/mcp") // registered, never called
	r := runRecordCmd(t, dir, append(append(args, "--"), agentCommand()...)...)
	if r.code != ExitOK {
		t.Fatalf("exit code %d, stderr:\n%s", r.code, r.stderr)
	}
	var seen []mcpExchange
	if err := json.Unmarshal([]byte(r.stdout.String()), &seen); err != nil {
		t.Fatalf("agent output %q: %v", r.stdout, err)
	}

	ctx := context.Background()
	c := openCassette(t, cassette.Path(dir, "mcp"))
	all, err := c.Interactions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1+len(seen) || len(seen) != 14 {
		t.Fatalf("got %d interactions and %d agent exchanges, want 1 LLM call and 7 exchanges per server", len(all), len(seen))
	}

	// spec §3.4, §7.2: each server's initialize result, from a JSON and from
	// an SSE response, is in the session metadata.
	s, err := c.Session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(`[{"name":"orders","url":%q,"protocol_version":"2025-06-18","server_info":{"name":"orders","version":"1.0.0"}},`+
		`{"name":"weather","url":%q,"protocol_version":"2025-03-26","server_info":{"name":"weather","version":"2.1.0"}},`+
		`{"name":"idle","url":"http://127.0.0.1:9/mcp"}]`, servers["orders"].URL, servers["weather"].URL)
	if string(s.MCPServers) != want {
		t.Errorf("mcp_servers = %s\nwant %s", s.MCPServers, want)
	}
	for _, name := range mcpAgentServers {
		if got := len(servers[name].Received()); got != 7 {
			t.Errorf("server %s received %d requests, want 7", name, got)
		}
	}

	wantStatus := []int{200, 202, 200, 200, 200, 405, 200} // per step, for both servers
	for i, ex := range seen {
		in := all[i+1] // the agent's LLM call comes first
		step := fmt.Sprintf("%s step %d (%s)", ex.Server, i%7, ex.Method)
		if ex.Status != wantStatus[i%7] {
			t.Errorf("%s: agent got status %d, want %d", step, ex.Status, wantStatus[i%7])
		}
		if in.Channel != "mcp" || in.Upstream != "mcp:"+ex.Server || in.HTTPMethod != ex.Method || in.Path != "/" {
			t.Errorf("%s: recorded %s %s %s %s", step, in.Channel, in.Upstream, in.HTTPMethod, in.Path)
		}
		if in.StatusCode == nil || *in.StatusCode != ex.Status || in.Error != nil || in.Incomplete() {
			t.Errorf("%s: recorded status %v, error %v; want %d and complete", step, in.StatusCode, in.Error, ex.Status)
		}

		// The request body and the response, exactly as the agent sent and saw them.
		if got := blobText(t, c, in.RequestBlob); got != ex.Request {
			t.Errorf("%s: stored request %q, want %q", step, got, ex.Request)
		}
		streamed := strings.HasPrefix(ex.ContentType, "text/event-stream")
		if in.Streamed != streamed {
			t.Errorf("%s: streamed = %v, want %v", step, in.Streamed, streamed)
		}
		stored := blobText(t, c, in.ResponseBlob)
		if streamed {
			chunks, err := c.Chunks(ctx, in.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, ch := range chunks {
				stored += string(ch.Data)
			}
			// AC-03 for MCP: each event reached the agent as it arrived, and
			// was stored as a chunk with its offset.
			if len(chunks) < 2 {
				t.Errorf("%s: %d chunks, want a log event before each response", step, len(chunks))
			} else if spread := chunks[1].OffsetMillis - chunks[0].OffsetMillis; spread < eventDelay.Milliseconds()/2 ||
				ex.SpreadMillis < eventDelay.Milliseconds()/2 {
				t.Errorf("%s: chunks %d ms apart, agent saw the body over %d ms; want both about %v",
					step, spread, ex.SpreadMillis, eventDelay)
			}
		}
		if stored != ex.Body {
			t.Errorf("%s: stored response %q, agent saw %q", step, stored, ex.Body)
		}

		var reqH, respH http.Header
		_ = json.Unmarshal(in.RequestHeaders, &reqH)
		_ = json.Unmarshal(in.ResponseHeaders, &respH)
		session := seen[i-i%7].SessionID
		if i%7 == 0 {
			if session == "" || respH.Get(mcptest.SessionHeader) != session {
				t.Errorf("%s: response %s = %q, want the agent's %q", step, mcptest.SessionHeader, respH.Get(mcptest.SessionHeader), session)
			}
		} else if reqH.Get(mcptest.SessionHeader) != session || reqH.Get(mcptest.ProtocolVersionHeader) == "" {
			t.Errorf("%s: request headers %v, want %s %q and %s", step, reqH, mcptest.SessionHeader, session, mcptest.ProtocolVersionHeader)
		}
	}
	if seen[0].ContentType != "application/json" || !strings.Contains(seen[4].Body, `"id":"p"`) || !strings.HasPrefix(seen[4].Body, "[") {
		t.Errorf("orders answered initialize as %q and the batch with %q, want JSON and a JSON array", seen[0].ContentType, seen[4].Body)
	}
	if !strings.Contains(seen[11].Body, `"id":4`) || !strings.Contains(seen[11].Body, `"id":"p"`) {
		t.Errorf("weather answered the batch with %q, want both responses in the stream", seen[11].Body)
	}
}

// blobText returns the blob stored under key, or "" when key is nil.
func blobText(t *testing.T, c *cassette.Cassette, key *string) string {
	t.Helper()
	if key == nil {
		return ""
	}
	b, err := c.Blob(context.Background(), *key)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
