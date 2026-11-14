package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pmlucero/retrace/internal/cassette"
)

// The credentials the secret agent and its upstreams exchange. AC-06 looks
// for each of them in the cassette files.
//
//nolint:gosec // G101: fake credentials that AC-06 looks for
const (
	secretAgentEnv = "RETRACE_TEST_SECRET_AGENT"

	sessionToken  = "SESSION-TOKEN-SECRET"  // a *-token request header
	queryToken    = "QUERY-TOKEN-SECRET"    // access_token in the LLM request's query
	mcpAuth       = "MCP-AUTH-SECRET"       // the MCP Authorization header
	locationPass  = "LOCATION-PASS-SECRET"  // userinfo of a response Location
	locationToken = "LOCATION-TOKEN-SECRET" // a token in a response Location's query
	fragmentToken = "FRAGMENT-TOKEN-SECRET" // a token in a response Location's fragment
	routeToken    = "ROUTE-TOKEN-SECRET"    // a token in a fragment route, #/cb?access_token=…
	rawQToken     = "RAWQ-TOKEN-SECRET"     // a fragment token followed by a raw "?"
	setCookie     = "SET-COOKIE-SECRET"     // a response Set-Cookie
	runOnly       = "RUN-ONLY-SECRET"       // a header redacted only with run --redact-header
	runOnlyHeader = "X-Tenant-Ref"          // on no default deny-list
)

func secretAgentCommand() []string {
	return []string{os.Args[0], "-test.run=^TestHelperSecretAgent$"}
}

// TestHelperSecretAgent is the fake agent of the AC-06 test. It sends one
// LLM request and one MCP initialize request, each carrying credentials.
// Only in a replay does it send runOnlyHeader. It only runs as a subprocess.
func TestHelperSecretAgent(t *testing.T) {
	if os.Getenv(secretAgentEnv) == "" {
		t.Skip("subprocess of the redaction tests")
	}
	extra := http.Header{}
	if os.Getenv("RETRACE_MODE") != "record" {
		extra.Set(runOnlyHeader, runOnly)
	}
	send := func(url, body string, h http.Header) {
		// The URL is the proxy that retrace injected: calling it is the point.
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader(body)) //nolint:gosec // G704
		req.Header = h
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		for k, v := range extra {
			req.Header[k] = v
		}
		resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: see above
		if err != nil {
			fmt.Fprintln(os.Stderr, "agent request failed:", err)
			os.Exit(99)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			fmt.Fprintln(os.Stderr, "agent request got", resp.Status)
			os.Exit(98)
		}
	}
	send(os.Getenv("ANTHROPIC_BASE_URL")+"/v1/messages?access_token="+queryToken,
		`{"model":"claude-sonnet-5-5","max_tokens":16,"messages":[{"role":"user","content":"Hi"}]}`,
		http.Header{"X-Api-Key": {agentAPIKey}, "X-Session-Token": {sessionToken}})
	send(os.Getenv(mcpEnvName("tools")),
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"helper","version":"1"}}}`,
		http.Header{"Authorization": {"Bearer " + mcpAuth}})
	os.Exit(0)
}

// secretUpstream answers like the LLM (path /v1/messages) or like an MCP
// server, with a Set-Cookie and a Location that carry credentials. It counts
// the requests that carried runOnlyHeader, which proves that the agent sent
// it through the proxy.
type secretUpstream struct {
	*httptest.Server
	runOnlySeen atomic.Int64
}

func newSecretUpstream(t *testing.T) *secretUpstream {
	t.Helper()
	u := &secretUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(runOnlyHeader) == runOnly {
			u.runOnlySeen.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Add("Set-Cookie", "session="+setCookie+"; HttpOnly")
		w.Header().Set("Location", "https://agent:"+locationPass+"@app.example.com/cb?access_token="+locationToken+"&state=s"+
			"#access_token="+fragmentToken+"&state=/orders?tab=1")
		w.Header().Set("Content-Location", "https://app.example.com/#/cb?access_token="+routeToken)
		w.Header().Set("Referer", "https://app.example.com/#access_token="+rawQToken+"?tab=1")
		if r.URL.Path == "/v1/messages" {
			_, _ = io.WriteString(w, `{"type":"message","role":"assistant","model":"claude-sonnet-5-5",`+
				`"content":[{"type":"text","text":"Hello."}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
			return
		}
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{},`+
			`"serverInfo":{"name":"tools","version":"1"}}}`)
	}))
	t.Cleanup(u.Close)
	return u
}

// marker is what redaction stores in place of v (spec §3.5).
func marker(v string) string {
	sum := sha256.Sum256([]byte(v))
	return "[REDACTED:" + hex.EncodeToString(sum[:4]) + "]"
}

// scanCassette fails the test if any credential appears in the raw bytes of
// session id's cassette, in any form SQLite might store it, or if the marker
// of a value in masked is missing: a value that was never stored would pass
// the scan too.
func scanCassette(t *testing.T, dir, id string, secrets, masked []string) {
	t.Helper()
	raw, err := os.ReadFile(cassette.Path(dir, id)) //nolint:gosec // G304: a cassette this test created
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range secrets {
		if bytes.Contains(raw, []byte(s)) {
			t.Errorf("%s: %q appears in the cassette file", id, s)
		}
	}
	for _, v := range masked {
		if !bytes.Contains(raw, []byte(marker(v))) {
			t.Errorf("%s: the marker of %q is not in the cassette file", id, v)
		}
	}
}

// AC-06: no credential reaches the cassette of a recording, nor that of a
// replay that forwards requests, in headers, queries or URL-carrying
// response headers, fragments included. The replay also redacts a header
// named only with run --redact-header.
func TestRedactionKeepsCredentialsOutOfEveryCassette(t *testing.T) {
	t.Setenv(secretAgentEnv, "1")
	dir := t.TempDir()
	secrets := []string{agentAPIKey, sessionToken, queryToken, mcpAuth, locationPass, locationToken,
		fragmentToken, routeToken, rawQToken, setCookie, runOnly}
	// What each cassette stores masked: header values whole, query and
	// fragment values alone, a fragment value only up to a raw "?". The
	// Location userinfo becomes REDACTED, with no hash.
	masked := []string{agentAPIKey, sessionToken, queryToken, "Bearer " + mcpAuth, locationToken,
		fragmentToken, routeToken, rawQToken, "session=" + setCookie + "; HttpOnly"}

	llm, tools := newSecretUpstream(t), newSecretUpstream(t)
	r := runRecordCmd(t, dir, append([]string{"--session", "s", "--listen", "127.0.0.1:0",
		"--anthropic-upstream", llm.URL, "--mcp", "tools=" + tools.URL, "--"}, secretAgentCommand()...)...)
	if r.code != ExitOK {
		t.Fatalf("record: exit %d\n%s", r.code, r.stderr)
	}
	scanCassette(t, dir, "s", secrets, masked)

	for _, tc := range []struct {
		mode string
		args []string
		live *secretUpstream // the upstream the replay forwards to
	}{
		{"swap-model", []string{"--model", "claude-opus-5-5", "--anthropic-upstream", llm.URL}, llm},
		{"live-tools", []string{"--tools", "live", "--mcp", "tools=" + tools.URL}, tools},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			before := tc.live.runOnlySeen.Load()
			r := runRunCmd(t, dir, append([]string{"s", "--recorded-command", "--listen", "127.0.0.1:0", "--redact-header", runOnlyHeader}, tc.args...)...)
			if r.code != ExitOK {
				t.Fatalf("run: exit %d\n%s", r.code, r.stderr)
			}
			if tc.live.runOnlySeen.Load() == before {
				t.Fatalf("the upstream never got %s; the replay forwarded nothing", runOnlyHeader)
			}
			id := "s." + tc.mode + ".1"
			s, _ := sessionOf(t, dir, id)
			if !strings.Contains(string(s.Redaction), `"`+strings.ToLower(runOnlyHeader)+`"`) {
				t.Errorf("stored redaction = %s, want it to include %s (spec §4.4)", s.Redaction, runOnlyHeader)
			}
			scanCassette(t, dir, id, secrets, append(masked, runOnly))
		})
	}
}
