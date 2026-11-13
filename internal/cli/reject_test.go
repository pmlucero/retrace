package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pmlucero/retrace/internal/proxy"
)

// rejectAgentEnv makes TestHelperRejectAgent run.
const rejectAgentEnv = "RETRACE_TEST_REJECT_AGENT"

// rejectToken is in the query of every request the proxy must reject: the
// log must not hold it (spec §3.6).
const rejectToken = "AC34-SECRET-QUERY-TOKEN"

// rejectCase is a request of the reject agent, and the answer it must get.
type rejectCase struct {
	target  string // the request target, sent byte for byte
	size    int64  // the body size; bodies above the limit are zeros
	noLen   bool   // send the body chunked, without a Content-Length
	status  int
	errType string // the Anthropic error type on the LLM route; "" for plain text
}

// rejectCases are the requests of AC-34, plus one with ../ in its query,
// which is not a dot segment and is answered as usual.
var rejectCases = []rejectCase{
	{target: "/mcp/orders/../x?token=" + rejectToken, status: 400},
	{target: "/mcp/../llm/anthropic/v1/messages?token=" + rejectToken, status: 400},
	{target: "/mcp/orders/%2e%2e/x?token=" + rejectToken, status: 400},
	{target: "/llm/anthropic/%2e%2e/llm/anthropic/v1/messages?token=" + rejectToken, status: 400, errType: "invalid_request_error"},
	{target: "/llm/anthropic/v1/messages?token=" + rejectToken, size: proxy.MaxRequestBytes + 1, status: 413, errType: "request_too_large"},
	{target: "/llm/anthropic/v1/messages?token=" + rejectToken, size: proxy.MaxRequestBytes + 1, noLen: true, status: 413, errType: "request_too_large"},
	{target: "/mcp/orders?token=" + rejectToken, size: proxy.MaxRequestBytes + 1, status: 413},
	{target: "/mcp/orders?token=" + rejectToken, size: proxy.MaxRequestBytes + 1, noLen: true, status: 413},
	{target: "/llm/anthropic/v1/messages?next=../x", status: 200},
}

// rejectBody is the body of the requests within the limit.
const rejectBody = `{"model":"claude-sonnet-5-5","max_tokens":16,"messages":[{"role":"user","content":"Weather in Paris?"}]}`

// TestHelperRejectAgent is the fake agent of the AC-34 tests. It sends
// rejectCases over raw connections, so that no client cleans a path or
// gives up on a large body when the answer comes early, and prints one line
// per answer: its status, then its body.
func TestHelperRejectAgent(t *testing.T) {
	if os.Getenv(rejectAgentEnv) == "" {
		t.Skip("subprocess of the AC-34 tests")
	}
	base, err := url.Parse(os.Getenv("ANTHROPIC_BASE_URL"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "parsing ANTHROPIC_BASE_URL:", err)
		os.Exit(99)
	}
	for _, c := range rejectCases {
		status, body, err := sendRaw(base.Host, c)
		if err != nil {
			fmt.Fprintf(os.Stderr, "request %s: %v\n", c.target, err)
			os.Exit(99)
		}
		fmt.Printf("%d %s\n", status, strings.ReplaceAll(body, "\n", " "))
	}
	os.Exit(0)
}

// sendRaw sends c to addr and returns the answer. The body is written while
// the answer is read, and a write error is ignored: retrace may answer and
// close the connection before it has read the whole body.
func sendRaw(addr string, c rejectCase) (int, string, error) {
	conn, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", addr)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = conn.Close() }()
	var body io.Reader = strings.NewReader(rejectBody)
	size := int64(len(rejectBody))
	if c.size > 0 {
		body, size = io.LimitReader(zeroReader{}, c.size), c.size
	}
	head := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nConnection: close\r\n", c.target, addr)
	if c.noLen {
		head += "Transfer-Encoding: chunked\r\n\r\n"
	} else {
		head += fmt.Sprintf("Content-Length: %d\r\n\r\n", size)
	}
	go func() {
		w := bufio.NewWriterSize(conn, 1<<16)
		_, _ = io.WriteString(w, head)
		if c.noLen {
			cw := httputil.NewChunkedWriter(w)
			_, _ = io.Copy(cw, body)
			_ = cw.Close()
			_, _ = io.WriteString(w, "\r\n")
		} else {
			_, _ = io.Copy(w, body)
		}
		_ = w.Flush()
	}()
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if loc := resp.Header.Get("Location"); loc != "" {
		return 0, "", fmt.Errorf("redirected to %s", loc)
	}
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), err
}

// zeroReader is an endless body.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// countingUpstream answers every request with an Anthropic message and
// counts them.
func countingUpstream(t *testing.T, n *atomic.Int64) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","role":"assistant","model":"claude-sonnet-5-5",`+
			`"content":[{"type":"text","text":"Sunny."}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2}}`)
	}))
	t.Cleanup(up.Close)
	return up
}

// checkRejectAgent checks the agent's answers and the log of the run that
// served them (spec §3.6).
func checkRejectAgent(t *testing.T, r recordRun) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(r.stdout.String(), "\n"), "\n")
	if len(lines) != len(rejectCases) {
		t.Fatalf("agent printed %d answers, want %d:\n%s\nstderr:\n%s", len(lines), len(rejectCases), r.stdout, r.stderr)
	}
	for i, c := range rejectCases {
		status, body, _ := strings.Cut(lines[i], " ")
		if status != fmt.Sprint(c.status) {
			t.Errorf("%s (body %d bytes, chunked %v): status %s, want %d: %s", c.target, c.size, c.noLen, status, c.status, body)
			continue
		}
		switch {
		case c.status == 200:
		case c.errType != "":
			if want := `{"type":"error","error":{"type":"` + c.errType + `","message":"retrace: `; !strings.HasPrefix(body, want) {
				t.Errorf("%s: body %s, want an Anthropic %s error", c.target, body, c.errType)
			}
		case !strings.HasPrefix(body, "retrace: "):
			t.Errorf("%s: body %q, want plain text starting with retrace:", c.target, body)
		}
	}
	logs := r.stderr.String()
	if got, want := strings.Count(logs, "request rejected"), len(rejectCases)-1; got != want {
		t.Errorf("%d rejection warnings, want %d:\n%s", got, want, logs)
	}
	for _, leak := range []string{rejectToken, "orders/", "%2e", "/x"} {
		if strings.Contains(logs, leak) {
			t.Errorf("the log holds %q:\n%s", leak, logs)
		}
	}
}

// AC-34: in record mode, rejected requests get 400 or 413, never reach an
// upstream and are not recorded.
func TestRecordRejectsDotSegmentsAndLargeBodies(t *testing.T) {
	recordRejectAgent(t, t.TempDir(), "s")
}

// recordRejectAgent records session id in dir with the reject agent, checks
// it as AC-34 says, and returns the agent's output.
func recordRejectAgent(t *testing.T, dir, id string) string {
	t.Helper()
	t.Setenv(rejectAgentEnv, "1")
	var llmCalls, mcpCalls atomic.Int64
	llm, mcpUp := countingUpstream(t, &llmCalls), countingUpstream(t, &mcpCalls)

	r := runRecordCmd(t, dir, "--session", id, "--listen", "127.0.0.1:0", "--anthropic-upstream", llm.URL,
		"--mcp", "orders="+mcpUp.URL+"/mcp", "--", os.Args[0], "-test.run=^TestHelperRejectAgent$")
	if r.code != ExitOK {
		t.Fatalf("record: exit %d\n%s", r.code, r.stderr)
	}
	checkRejectAgent(t, r)
	// Only the request with ../ in its query reached an upstream.
	if llmCalls.Load() != 1 || mcpCalls.Load() != 0 {
		t.Errorf("upstream calls: %d LLM, %d MCP; want 1 and 0", llmCalls.Load(), mcpCalls.Load())
	}
	if _, ins := sessionOf(t, dir, id); len(ins) != 1 || ins[0].Path != "/v1/messages?next=../x" {
		t.Errorf("recorded interactions = %+v, want only the request with ../ in its query", ins)
	}
	return r.stdout.String()
}

// AC-34: in exact mode, rejected requests get the same answers as while
// recording, are not matched (no cassette miss) and are not recorded.
func TestRunRejectsDotSegmentsAndLargeBodies(t *testing.T) {
	dir := t.TempDir()
	want := recordRejectAgent(t, dir, "s")

	failOutboundDials(t)
	r := runRunCmd(t, dir, "s", "--as", "replay", "--listen", "127.0.0.1:0", "--", os.Args[0], "-test.run=^TestHelperRejectAgent$")
	if r.code != ExitOK {
		t.Fatalf("run: exit %d\n%s", r.code, r.stderr)
	}
	checkRejectAgent(t, r)
	if got := r.stdout.String(); got != want {
		t.Errorf("replayed answers:\n%s\nwant the recorded ones:\n%s", got, want)
	}
	s, ins := sessionOf(t, dir, "replay")
	if s.Status != "completed" || len(ins) != 1 || ins[0].Provenance != "replayed" {
		t.Errorf("replay session %q with interactions %+v, want completed with only the replayed one", s.Status, ins)
	}
}
