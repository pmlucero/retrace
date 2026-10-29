package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/derive"
	"github.com/pmlucero/retrace/internal/provider/anthropic"
)

// The agent in these tests is this test binary, run again as
// TestHelperAgent with RETRACE_TEST_AGENT set to what it should do.
const (
	agentModeEnv = "RETRACE_TEST_AGENT"
	agentExitEnv = "RETRACE_TEST_AGENT_EXIT"
	agentAPIKey  = "sk-ant-test-0123456789-SECRET" //nolint:gosec // G101: a fake key that AC-06 looks for
)

func agentCommand() []string {
	return []string{os.Args[0], "-test.run=^TestHelperAgent$"}
}

// TestHelperAgent is the fake agent. It only runs as a subprocess of the
// record tests, and exits without returning so the testing framework prints
// nothing to its stdout.
func TestHelperAgent(t *testing.T) {
	mode := os.Getenv(agentModeEnv)
	if mode == "" {
		t.Skip("subprocess of the record tests")
	}
	if mode == "wait" {
		signal.Notify(make(chan os.Signal, 1), os.Interrupt) // survive until the test is ready
	}
	base := os.Getenv("ANTHROPIC_BASE_URL")
	// The URL is the proxy that retrace injected: calling it is the point.
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, base+"/v1/messages", //nolint:gosec // G704
		strings.NewReader(`{"model":"claude-sonnet-5-5","max_tokens":16,"messages":[{"role":"user","content":"Weather in Paris?"}]}`))
	req.Header.Set("X-Api-Key", agentAPIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: see above
	if err != nil {
		fmt.Fprintln(os.Stderr, "agent request failed:", err)
		os.Exit(99)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if mode == "mcp" {
		runMCPAgent()
	}

	vars := map[string]string{}
	for _, k := range []string{"ANTHROPIC_BASE_URL", "RETRACE_SESSION_ID", "RETRACE_MODE", "RETRACE_MCP_ORDERS_DB_URL"} {
		vars[k] = os.Getenv(k)
	}
	_ = json.NewEncoder(os.Stdout).Encode(vars)

	if mode == "wait" {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, os.Interrupt)
		fmt.Println("ready")
		<-sigs
		os.Exit(130)
	}
	code, _ := strconv.Atoi(os.Getenv(agentExitEnv))
	os.Exit(code)
}

// syncBuffer is a bytes.Buffer that the agent's output copier and the test
// can use at the same time.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// fakeAnthropic answers /v1/messages with a fixed message.
func fakeAnthropic(t *testing.T) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("X-Api-Key") != agentAPIKey {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","role":"assistant","model":"claude-sonnet-5-5",`+
			`"content":[{"type":"text","text":"Checking."},{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Paris"}}],`+
			`"stop_reason":"tool_use","usage":{"input_tokens":100,"output_tokens":10}}`)
	}))
	t.Cleanup(up.Close)
	return up
}

type recordRun struct {
	code           int
	stdout, stderr *syncBuffer
}

// runRecordCmd runs retrace with --dir dir, then record and args.
func runRecordCmd(t *testing.T, dir string, args ...string) recordRun {
	t.Helper()
	return runRecordInto(recordRun{stdout: &syncBuffer{}, stderr: &syncBuffer{}}, dir, args...)
}

// runRecordInto is runRecordCmd with output buffers the caller can watch.
func runRecordInto(r recordRun, dir string, args ...string) recordRun {
	all := append([]string{"--dir", dir, "record"}, args...)
	r.code = run(context.Background(), all, nil, r.stdout, r.stderr, fakeEnv(nil), commands)
	return r
}

func openCassette(t *testing.T, path string) *cassette.Cassette {
	t.Helper()
	c, err := cassette.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// AC-01, AC-02, AC-05, AC-06
func TestRecordRunsTheAgentAndRecordsItsCalls(t *testing.T) {
	t.Setenv(agentModeEnv, "call")
	dir := t.TempDir()
	up := fakeAnthropic(t)

	r := runRecordCmd(t, dir, append([]string{"--session", "checkout", "--listen", "127.0.0.1:0",
		"--anthropic-upstream", up.URL, "--mcp", "orders-db=http://user:pw@127.0.0.1:9/mcp?api_key=mcp-SECRET",
		"--label", "case=happy", "--redact", "$..max_tokens", "--"}, agentCommand()...)...)
	if r.code != ExitOK {
		t.Fatalf("exit code %d, stderr:\n%s", r.code, r.stderr)
	}

	var vars map[string]string
	if err := json.Unmarshal([]byte(strings.SplitN(r.stdout.String(), "\n", 2)[0]), &vars); err != nil {
		t.Fatalf("agent output %q: %v", r.stdout, err)
	}
	base := vars["ANTHROPIC_BASE_URL"]
	if !strings.HasPrefix(base, "http://127.0.0.1:") || !strings.HasSuffix(base, "/llm/anthropic") {
		t.Errorf("ANTHROPIC_BASE_URL = %q", base)
	}
	if vars["RETRACE_SESSION_ID"] != "checkout" || vars["RETRACE_MODE"] != "record" {
		t.Errorf("agent environment = %v", vars)
	}
	if want := strings.TrimSuffix(base, "/llm/anthropic") + "/mcp/orders-db"; vars["RETRACE_MCP_ORDERS_DB_URL"] != want {
		t.Errorf("RETRACE_MCP_ORDERS_DB_URL = %q, want %q", vars["RETRACE_MCP_ORDERS_DB_URL"], want)
	}

	path := cassette.Path(dir, "checkout")
	c := openCassette(t, path)
	s, err := c.Session(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != "completed" || s.AgentExitCode == nil || *s.AgentExitCode != 0 || s.Mode != "record" {
		t.Errorf("session = %+v, want completed with exit code 0", s)
	}
	if s.Labels["case"] != "happy" || len(s.Command) != 2 {
		t.Errorf("session labels %v, command %v", s.Labels, s.Command)
	}
	all, err := c.Interactions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].StatusCode == nil || *all[0].StatusCode != 200 || all[0].Path != "/v1/messages" {
		t.Errorf("interactions = %+v, want one 200 POST /v1/messages", all)
	}

	// AC-05 (LLM): the call's events, with tokens, cost and participants.
	if s.DeriverVersion != derive.Version || s.PricingVersion != anthropic.PricingVersion() {
		t.Errorf("session deriver v%d, pricing %q; want v%d, %q", s.DeriverVersion, s.PricingVersion, derive.Version, anthropic.PricingVersion())
	}
	events, err := c.Events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range events {
		got = append(got, fmt.Sprintf("%s %s->%s", e.Kind, e.Source, e.Target))
	}
	if want := []string{"user_message user->model", "model_response model->agent", "tool_use model->agent"}; !slices.Equal(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
	resp := events[1]
	if resp.Model == nil || *resp.Model != "claude-sonnet-5-5" || resp.TokensIn == nil || *resp.TokensIn != 100 ||
		resp.TokensOut == nil || *resp.TokensOut != 10 || resp.CostUSD == nil || *resp.CostUSD != 0.0003 {
		t.Errorf("model_response = %+v, want model, 100/10 tokens and $0.0003", resp)
	}
	if tu := events[2]; tu.Tool == nil || *tu.Tool != "get_weather" || string(tu.Arguments) != `{"city":"Paris"}` ||
		tu.ParentSeq == nil || *tu.ParentSeq != resp.Seq {
		t.Errorf("tool_use = %+v, want get_weather with its arguments, under the model_response", tu)
	}

	if !strings.Contains(string(s.Redaction), `"body":["$..max_tokens"]`) {
		t.Errorf("stored redaction = %s, want the body rule (spec §7.2)", s.Redaction)
	}
	if !strings.Contains(string(s.MCPServers), "http://REDACTED@127.0.0.1:9/mcp?api_key=[REDACTED:") {
		t.Errorf("stored MCP servers = %s, want the URL credentials redacted (spec §3.5)", s.MCPServers)
	}

	// AC-06: no credential reaches the file, in any form SQLite might store it.
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the cassette this test created
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{agentAPIKey, "mcp-SECRET", "user:pw"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Errorf("%q appears in the cassette file", secret)
		}
	}
}

func TestRecordExitsWithFourWhenTheAgentFails(t *testing.T) {
	t.Setenv(agentModeEnv, "call")
	t.Setenv(agentExitEnv, "3")
	dir := t.TempDir()
	up := fakeAnthropic(t)

	r := runRecordCmd(t, dir, append([]string{"--session", "s", "--listen", "127.0.0.1:0",
		"--anthropic-upstream", up.URL, "--"}, agentCommand()...)...)
	if r.code != ExitAgentFailed {
		t.Fatalf("exit code %d, want %d; stderr:\n%s", r.code, ExitAgentFailed, r.stderr)
	}
	s, _ := openCassette(t, cassette.Path(dir, "s")).Session(context.Background())
	if s.Status != "completed" || s.AgentExitCode == nil || *s.AgentExitCode != 3 {
		t.Errorf("session status %q, exit code %v; want completed and 3 (spec §3.1)", s.Status, s.AgentExitCode)
	}
}

// AC-07
func TestRecordInterruptedBySIGINT(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGINT cannot be sent to a process on Windows")
	}
	t.Setenv(agentModeEnv, "wait")
	dir := t.TempDir()
	up := fakeAnthropic(t)

	sigs := make(chan chan<- os.Signal, 1)
	saved := notifySignals
	notifySignals = func(c chan<- os.Signal) func() {
		sigs <- c
		return func() {}
	}
	t.Cleanup(func() { notifySignals = saved })

	r := recordRun{stdout: &syncBuffer{}, stderr: &syncBuffer{}}
	result := make(chan recordRun, 1)
	go func() {
		result <- runRecordInto(r, dir, append([]string{"--session", "s", "--listen", "127.0.0.1:0",
			"--anthropic-upstream", up.URL, "--"}, agentCommand()...)...)
	}()
	select {
	case c := <-sigs:
		// The agent has made its call and waits for the signal.
		waitUntil(t, func() bool { return strings.Contains(r.stdout.String(), "ready") })
		c <- os.Interrupt
	case r = <-result:
		t.Fatalf("record ended early: %d\n%s", r.code, r.stderr)
	}
	select {
	case r = <-result:
	case <-time.After(30 * time.Second):
		t.Fatal("record did not end after SIGINT")
	}

	if r.code != ExitAgentFailed {
		t.Errorf("exit code %d, want %d (the agent exited 130); stderr:\n%s", r.code, ExitAgentFailed, r.stderr)
	}
	c := openCassette(t, cassette.Path(dir, "s"))
	s, _ := c.Session(context.Background())
	if s.Status != "interrupted" || s.AgentExitCode == nil || *s.AgentExitCode != 130 {
		t.Errorf("session status %q, exit code %v; want interrupted and 130", s.Status, s.AgentExitCode)
	}
	if all, _ := c.Interactions(context.Background()); len(all) != 1 {
		t.Errorf("got %d interactions, want the one made before the signal", len(all))
	}
}

// AC-24
func TestRecordRejectsInvalidSessionIDs(t *testing.T) {
	for _, id := range []string{"../x", ".hidden", "a/b", "NUL", strings.Repeat("a", 129)} {
		parent := t.TempDir()
		dir := filepath.Join(parent, "cassettes")
		r := runRecordCmd(t, dir, "--session", id, "--", "true")
		if r.code != ExitUsage || !strings.Contains(r.stderr.String(), "invalid session id") {
			t.Errorf("--session %q: exit %d, stderr %q; want a usage error", id, r.code, r.stderr)
		}
		entries, _ := os.ReadDir(parent)
		if len(entries) != 0 {
			t.Errorf("--session %q created %v", id, entries)
		}
	}
}

func TestRecordRefusesAnExistingSessionUnlessForced(t *testing.T) {
	t.Setenv(agentModeEnv, "call")
	dir := t.TempDir()
	up := fakeAnthropic(t)
	args := append([]string{"--session", "s", "--listen", "127.0.0.1:0", "--anthropic-upstream", up.URL, "--"}, agentCommand()...)

	if r := runRecordCmd(t, dir, args...); r.code != ExitOK {
		t.Fatalf("first record: %d\n%s", r.code, r.stderr)
	}
	if r := runRecordCmd(t, dir, args...); r.code != ExitError || !strings.Contains(r.stderr.String(), "already exists") {
		t.Errorf("second record: exit %d, stderr %q; want it refused", r.code, r.stderr)
	}
	if r := runRecordCmd(t, dir, append([]string{"--force"}, args...)...); r.code != ExitOK {
		t.Errorf("record --force: exit %d\n%s", r.code, r.stderr)
	}
}

func TestRecordGeneratesASessionID(t *testing.T) {
	t.Setenv(agentModeEnv, "call")
	dir := t.TempDir()
	up := fakeAnthropic(t)

	r := runRecordCmd(t, dir, append([]string{"--listen", "127.0.0.1:0", "--anthropic-upstream", up.URL, "--"}, agentCommand()...)...)
	if r.code != ExitOK {
		t.Fatalf("exit %d\n%s", r.code, r.stderr)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.cassette"))
	if len(files) != 1 {
		t.Fatalf("cassettes = %v, want one", files)
	}
	id := strings.TrimSuffix(filepath.Base(files[0]), ".cassette")
	if _, err := time.Parse("20060102-150405", id[:15]); err != nil || len(id) != 20 {
		t.Errorf("generated id %q, want YYYYMMDD-HHMMSS-xxxx", id)
	}
}

func TestRecordMarksTheSessionFailedWhenTheAgentCannotStart(t *testing.T) {
	dir := t.TempDir()
	r := runRecordCmd(t, dir, "--session", "s", "--listen", "127.0.0.1:0", "--", filepath.Join(dir, "no-such-agent"))
	if r.code != ExitError {
		t.Fatalf("exit %d, want %d", r.code, ExitError)
	}
	s, _ := openCassette(t, cassette.Path(dir, "s")).Session(context.Background())
	if s.Status != "failed" || s.AgentExitCode != nil {
		t.Errorf("session status %q, exit code %v; want failed and none", s.Status, s.AgentExitCode)
	}
}

func TestRecordUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"global flag after the command", []string{"--dir", "x", "--", "true"}, "--dir is a global flag: put it before the command"},
		{"no agent command", []string{"--session", "s"}, "needs an agent command"},
		{"non-loopback listen", []string{"--listen", "0.0.0.0:0", "--", "true"}, "add --listen-unsafe"},
		{"malformed MCP server", []string{"--mcp", "orders", "--", "true"}, "want name=url"},
		{"MCP URL", []string{"--mcp", "orders=ftp://x", "--", "true"}, "not an http or https URL"},
		{"body redaction rule", []string{"--redact", "user.email", "--", "true"}, "must start with $"},
		{"Anthropic URL", []string{"--anthropic-upstream", "api.anthropic.com", "--", "true"}, "--anthropic-upstream"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := runRecordCmd(t, t.TempDir(), tt.args...)
			if r.code != ExitUsage || !strings.Contains(r.stderr.String(), tt.want) {
				t.Errorf("exit %d, stderr %q; want %d and %q", r.code, r.stderr, ExitUsage, tt.want)
			}
		})
	}
}

// spec §10: record --help says what cassettes contain.
func TestRecordHelpWarnsThatCassettesAreVerbatim(t *testing.T) {
	r := runRecordCmd(t, t.TempDir(), "--help")
	if r.code != ExitOK || !strings.Contains(r.stdout.String(), "verbatim") {
		t.Errorf("exit %d, help:\n%s", r.code, r.stdout)
	}
}

func TestMCPEnvName(t *testing.T) {
	for name, want := range map[string]string{
		"orders":    "RETRACE_MCP_ORDERS_URL",
		"orders-db": "RETRACE_MCP_ORDERS_DB_URL",
		"a_b9":      "RETRACE_MCP_A_B9_URL",
	} {
		if got := mcpEnvName(name); got != want {
			t.Errorf("mcpEnvName(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestWithEnvReplacesInheritedValues(t *testing.T) {
	got := withEnv([]string{"PATH=/bin", "ANTHROPIC_BASE_URL=https://api.anthropic.com"},
		map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:1/llm/anthropic"})
	want := []string{"PATH=/bin", "ANTHROPIC_BASE_URL=http://127.0.0.1:1/llm/anthropic"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("withEnv = %v, want %v", got, want)
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A terminal's Ctrl+C reaches the agent and retrace together. Even when the
// agent's exit is seen first, the session counts as interrupted (spec §3.1).
func TestWaitAgentCountsASignalThatRacesTheExit(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^$") //nolint:gosec // G204: this test binary, exiting at once
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sigs := make(chan os.Signal, 1)
	sigs <- os.Interrupt
	res := waitAgent(cmd, sigs, slog.New(slog.DiscardHandler))
	if !res.interrupted {
		t.Error("the session was not marked as interrupted")
	}
}
