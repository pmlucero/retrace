package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/mcp/mcptest"
)

// recordMCPForReplay records session id with the MCP helper agent against
// the two servers of mcpAgentServers, one answering in JSON and one in SSE,
// then shuts every upstream down. It returns what the agent saw.
func recordMCPForReplay(t *testing.T, dir, id string) []mcpExchange {
	t.Helper()
	t.Setenv(agentModeEnv, "mcp")
	up := fakeAnthropic(t)
	servers := []*mcptest.Server{
		mcptest.New(t, mcptest.Config{Name: "orders", Strict: true, Tools: []mcptest.Tool{lookupTool()}}),
		mcptest.New(t, mcptest.Config{Name: "weather", Strict: true, Mode: mcptest.SSE, LogEvents: 1,
			Tools: []mcptest.Tool{lookupTool()}}),
	}
	args := []string{"--session", id, "--listen", "127.0.0.1:0", "--anthropic-upstream", up.URL,
		"--mcp", "orders=" + servers[0].URL, "--mcp", "weather=" + servers[1].URL}
	r := runRecordCmd(t, dir, append(append(args, "--"), agentCommand()...)...)
	if r.code != ExitOK {
		t.Fatalf("record: exit %d\n%s", r.code, r.stderr)
	}
	// AC-08: nothing upstream is reachable during the replay.
	up.Close()
	for _, s := range servers {
		s.Close()
	}
	return agentExchanges(t, r.stdout.String())
}

func agentExchanges(t *testing.T, out string) []mcpExchange {
	t.Helper()
	var seen []mcpExchange
	if err := json.Unmarshal([]byte(out), &seen); err != nil {
		t.Fatalf("agent output %q: %v", out, err)
	}
	return seen
}

// sameExchange reports whether the agent saw the same answer to the same
// request in the replay as while recording. Only the session id may differ
// (spec §5 step 3), and the timing, since the replay is not --realtime.
func sameExchange(t *testing.T, recorded, replayed mcpExchange) {
	t.Helper()
	step := recorded.Server + " " + recorded.Method + " " + recorded.Request
	if replayed.Server != recorded.Server || replayed.Method != recorded.Method || replayed.Request != recorded.Request {
		t.Fatalf("replay sent %s %s %s, want %s", replayed.Server, replayed.Method, replayed.Request, step)
	}
	if replayed.Status != recorded.Status || replayed.ContentType != recorded.ContentType || replayed.Body != recorded.Body {
		t.Errorf("%s: replay got %d %q %q, recording got %d %q %q", step,
			replayed.Status, replayed.ContentType, replayed.Body, recorded.Status, recorded.ContentType, recorded.Body)
	}
	switch {
	case recorded.SessionID == "" && replayed.SessionID != "":
		t.Errorf("%s: replay issued Mcp-Session-Id %q where none was recorded", step, replayed.SessionID)
	case recorded.SessionID != "" && (replayed.SessionID == "" || replayed.SessionID == recorded.SessionID):
		t.Errorf("%s: Mcp-Session-Id %q, want a fresh one, not the recorded %q", step, replayed.SessionID, recorded.SessionID)
	}
}

// AC-08, AC-09, AC-10, AC-11 and AC-32 end to end: with every upstream
// down and outbound connections failing, the agent gets the recorded MCP
// answers, JSON and SSE byte for byte, with fresh session ids.
func TestRunReplaysMCPOffline(t *testing.T) {
	dir := t.TempDir()
	recorded := recordMCPForReplay(t, dir, "s")
	failOutboundDials(t)

	r := runRunCmd(t, dir, "s", "--listen", "127.0.0.1:0")
	if r.code != ExitOK {
		t.Fatalf("run: exit %d\n%s", r.code, r.stderr)
	}
	replayed := agentExchanges(t, r.stdout.String())
	if len(replayed) != len(recorded) || len(recorded) != 14 {
		t.Fatalf("replay saw %d exchanges, recording %d; want 7 per server", len(replayed), len(recorded))
	}
	for i := range recorded {
		sameExchange(t, recorded[i], replayed[i])
	}
	// AC-32: the recorded notifications, GET and DELETE are not left over.
	if strings.Contains(r.stderr.String(), "never replayed") {
		t.Errorf("unexpected unconsumed warning:\n%s", r.stderr)
	}

	s, ins := sessionOf(t, dir, "s.exact.1")
	src, _ := sessionOf(t, dir, "s")
	if s.Status != "completed" || string(s.MCPServers) != string(src.MCPServers) {
		t.Errorf("replay status %s, mcp_servers %s; want completed and the source's %s", s.Status, s.MCPServers, src.MCPServers)
	}
	if len(ins) != 1+len(recorded) {
		t.Fatalf("replay recorded %d interactions, want %d", len(ins), 1+len(recorded))
	}
	c := openCassette(t, cassette.Path(dir, "s.exact.1"))
	for _, in := range ins[1:] {
		// spec §5 step 3: GET, DELETE and notifications are answered without
		// matching, so they have no source interaction.
		unmatched := in.HTTPMethod != "POST" || strings.Contains(blobText(t, c, in.RequestBlob), "notifications/")
		if in.Channel != "mcp" || in.Provenance != "replayed" || (in.SourceInteractionID == nil) != unmatched {
			t.Errorf("interaction %d: %s %s %s source %v", in.ID, in.Channel, in.HTTPMethod, in.Provenance, in.SourceInteractionID)
		}
	}
}

// AC-13: requests that arrive in another order than recorded replay without
// a divergence: the two tool calls to each server swapped, as parallel calls
// may arrive, and the two servers' sessions swapped too.
func TestRunReplaysMCPInAnotherOrder(t *testing.T) {
	dir := t.TempDir()
	recorded := recordMCPForReplay(t, dir, "s")

	t.Setenv(mcpReverseEnv, "1")
	t.Setenv(mcpSwapEnv, "1")
	r := runRunCmd(t, dir, "s", "--listen", "127.0.0.1:0")
	if r.code != ExitOK {
		t.Fatalf("run: exit %d\n%s", r.code, r.stderr)
	}
	replayed := agentExchanges(t, r.stdout.String())
	if len(replayed) != len(recorded) {
		t.Fatalf("replay saw %d exchanges, want %d", len(replayed), len(recorded))
	}
	// Per server: initialize, notification, the two calls, GET, DELETE.
	half := len(recorded) / 2
	swapped := func(i int) int {
		switch i % half {
		case 3:
			return i + 1
		case 4:
			return i - 1
		}
		return i
	}
	for i := range recorded {
		sameExchange(t, recorded[i], replayed[swapped((i+half)%len(recorded))])
	}
}

// AC-31 end to end: a tool call with arguments that were not recorded gets
// the JSON-RPC error -32099 with its id, and the replay ends diverged.
func TestRunEndsDivergedOnAnMCPMiss(t *testing.T) {
	dir := t.TempDir()
	recordMCPForReplay(t, dir, "s")

	t.Setenv(mcpKeyEnv, "Z9")
	r := runRunCmd(t, dir, "s", "--as", "z9", "--listen", "127.0.0.1:0")
	if r.code != ExitDiverged {
		t.Fatalf("exit %d, want %d\n%s", r.code, ExitDiverged, r.stderr)
	}
	replayed := agentExchanges(t, r.stdout.String())
	if len(replayed) != 14 {
		t.Fatalf("the agent saw %d exchanges, want 7 per server", len(replayed))
	}
	call := replayed[3] // orders, tools/call with the new key
	if call.Status != 200 || !strings.Contains(call.Body, `"code":-32099`) || !strings.Contains(call.Body, `"id":3`) ||
		!strings.Contains(call.Body, "retrace: cassette miss") {
		t.Errorf("tools/call got %d %s, want a -32099 cassette miss with id 3", call.Status, call.Body)
	}

	s, _ := sessionOf(t, dir, "z9")
	if s.Status != "diverged" {
		t.Errorf("session status %q, want diverged", s.Status)
	}
	events, err := openCassette(t, cassette.Path(dir, "z9")).Events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var upstreams []string
	for _, e := range events {
		if e.Kind != "divergence" {
			continue
		}
		var res struct{ Channel, Upstream string }
		if err := json.Unmarshal(e.Result, &res); err != nil || res.Channel != "mcp" {
			t.Errorf("divergence result %s, want channel mcp", e.Result)
		}
		upstreams = append(upstreams, res.Upstream)
	}
	if strings.Join(upstreams, ",") != "mcp:orders,mcp:weather" {
		t.Errorf("divergences on %v, want one per server's changed call", upstreams)
	}
}

// The source's mcp_servers is untrusted input: each name becomes a route and
// an environment variable, so it must be one that record accepts.
func TestMCPServerNames(t *testing.T) {
	long := strings.Repeat("a", 65)
	for _, tc := range []struct {
		stored  string
		want    string // names joined with ","
		wantErr bool
	}{
		{stored: ``, want: ""},
		{stored: `[]`, want: ""},
		{stored: `null`, want: ""},
		{stored: `[{"name":"orders","url":"http://x"},{"name":"db_2-x"}]`, want: "orders,db_2-x"},
		{stored: `{}`, wantErr: true},
		{stored: `[{"name":""}]`, wantErr: true},
		{stored: `[{"name":"../x"}]`, wantErr: true},
		{stored: `[{"name":"a/b"}]`, wantErr: true},
		{stored: `[{"name":"` + long + `"}]`, wantErr: true},
		{stored: `[{"name":"a"},{"name":"a"}]`, wantErr: true},
	} {
		got, err := mcpServerNames(json.RawMessage(tc.stored))
		if (err != nil) != tc.wantErr || strings.Join(got, ",") != tc.want {
			t.Errorf("mcpServerNames(%s) = %v, %v; want %q, error %t", tc.stored, got, err, tc.want, tc.wantErr)
		}
	}
}
