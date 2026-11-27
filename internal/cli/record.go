package cli

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/pmlucero/retrace/internal/agentrun"
	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/derive"
	"github.com/pmlucero/retrace/internal/mcpmeta"
	"github.com/pmlucero/retrace/internal/provider/anthropic"
	"github.com/pmlucero/retrace/internal/proxy"
	"github.com/pmlucero/retrace/internal/redact"
	"github.com/pmlucero/retrace/internal/replay/session"
)

var recordCommand = command{
	name:    "record",
	summary: "record an agent session into a new cassette",
	run:     runRecord,
}

const recordUsage = `Usage: retrace record [flags] -- <agent command...>

Starts a local proxy, runs the agent with ANTHROPIC_BASE_URL and
RETRACE_MCP_<NAME>_URL pointing at it, and records every exchange into
<dir>/<session>.cassette until the agent exits.

WARNING: cassettes store prompts, model outputs and tool results verbatim.
Auth headers are always redacted, but review a cassette before sharing it.

Flags:
`

type recordOptions struct {
	session      string
	mcp          []mcpmeta.Server
	listen       string
	listenUnsafe bool
	anthropic    *url.URL
	redactHeader []string
	redactBody   []redact.Path
	labels       map[string]string
	force        bool
	command      []string
}

func parseRecordFlags(env *Env, args []string) (recordOptions, int, bool) {
	opts := recordOptions{labels: map[string]string{}}
	var anthropic string
	fs := flag.NewFlagSet("record", flag.ContinueOnError)
	fs.StringVar(&opts.session, "session", "", "session id (generated if omitted)")
	fs.Func("mcp", "register an MCP server as `name=url` (repeatable)", mcpFlag(&opts.mcp))
	fs.StringVar(&opts.listen, "listen", "127.0.0.1:4790", "proxy address; port 0 picks a free port")
	fs.BoolVar(&opts.listenUnsafe, "listen-unsafe", false, "allow a non-loopback --listen address")
	fs.StringVar(&anthropic, "anthropic-upstream", "https://api.anthropic.com", "Anthropic API base URL (plain http only for loopback hosts)")
	fs.Func("redact-header", "also redact the request or response header with this literal `name` (repeatable)", redactHeaderFlag(&opts.redactHeader))
	fs.Func("redact", "redact the values this `jsonpath` selects in stored JSON bodies (repeatable)", func(v string) error {
		p, err := redact.ParsePath(v)
		if err != nil {
			return err
		}
		opts.redactBody = append(opts.redactBody, p)
		return nil
	})
	fs.Func("label", "attach a `key=value` label to the session (repeatable)", func(v string) error {
		k, val, ok := strings.Cut(v, "=")
		if !ok || k == "" {
			return fmt.Errorf("want key=value")
		}
		opts.labels[k] = val
		return nil
	})
	fs.BoolVar(&opts.force, "force", false, "replace an existing cassette with the same session id")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), recordUsage)
		fs.PrintDefaults()
	}

	if code, ok := parseCommandFlags(fs, args, env); !ok {
		return opts, code, false
	}
	opts.command = fs.Args()
	if len(opts.command) == 0 {
		return opts, usageError(env.Stderr, "record needs an agent command after '--'"), false
	}
	if opts.session != "" {
		if err := cassette.ValidateSessionID(opts.session); err != nil {
			return opts, usageError(env.Stderr, err.Error()), false
		}
	}
	u, err := upstreamURL(anthropic)
	if err != nil {
		return opts, usageError(env.Stderr, "--anthropic-upstream: "+err.Error()), false
	}
	opts.anthropic = u
	if err := checkListen(opts.listen, opts.listenUnsafe); err != nil {
		return opts, usageError(env.Stderr, err.Error()), false
	}
	return opts, 0, true
}

// redactHeaderFlag adds one --redact-header name to names, for record and
// run alike (spec §9.2, §9.3). The name is literal: a leading "*" is
// rejected, because a user who writes "*-x" expects a pattern, and the stored
// rules (spec §7.2) could not tell a literal "*-x" from a pattern.
func redactHeaderFlag(names *[]string) func(string) error {
	return func(v string) error {
		if strings.HasPrefix(v, "*") {
			return fmt.Errorf("%q: header patterns are not accepted on the command line; give a literal header name", v)
		}
		*names = append(*names, v)
		return nil
	}
}

// mcpFlag parses one name=url value of --mcp into servers (spec §9.2).
func mcpFlag(servers *[]mcpmeta.Server) func(string) error {
	return func(v string) error {
		name, raw, ok := strings.Cut(v, "=")
		if !ok || !mcpmeta.ValidName(name) {
			return fmt.Errorf("want name=url, with a name of letters, digits, '_' or '-'")
		}
		if _, err := absoluteHTTPURL(raw); err != nil {
			return err
		}
		for _, s := range *servers {
			if s.Name == name {
				return fmt.Errorf("MCP server %q registered twice", name)
			}
		}
		*servers = append(*servers, mcpmeta.Server{Name: name, URL: raw})
		return nil
	}
}

func absoluteHTTPURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%q is not an http or https URL", raw)
	}
	return u, nil
}

// upstreamURL validates an --anthropic-upstream value (spec §9.2): plain http
// is refused unless the host is loopback, because the requests carry the real
// API key. The host is the parsed one, and names are never resolved.
func upstreamURL(raw string) (*url.URL, error) {
	u, err := absoluteHTTPURL(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return nil, fmt.Errorf("%q: plain http is allowed only for loopback hosts; use https", raw)
	}
	return u, nil
}

// isLoopbackHost reports whether host is localhost or a loopback IP literal.
// Names are never resolved, so no other name counts.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// checkListen keeps the proxy on loopback unless the user opts out: it has no
// authentication (spec §10).
func checkListen(addr string, unsafe bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--listen %q: %w", addr, err)
	}
	if ip := net.ParseIP(host); host == "localhost" || (ip != nil && ip.IsLoopback()) || unsafe {
		return nil
	}
	return fmt.Errorf("--listen %q is not a loopback address; add --listen-unsafe to expose the proxy", addr)
}

func runRecord(ctx context.Context, env *Env, args []string) int {
	opts, code, ok := parseRecordFlags(env, args)
	if !ok {
		return code
	}
	log := env.Logger

	// Listen before creating the cassette, so a busy port leaves nothing behind.
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", opts.listen)
	if err != nil {
		log.Error("cannot start the proxy", "listen", opts.listen, "error", err)
		return ExitError
	}
	defer ln.Close()

	rules := redact.NewRules(opts.redactHeader, opts.redactBody)
	c, id, err := createCassette(ctx, env, opts, rules)
	if err != nil {
		log.Error("cannot create the cassette", "error", err)
		return ExitError
	}
	path := c.Path()
	failed := func(msg string, err error) int {
		log.Error(msg, "error", err)
		if err := c.Close(); err != nil {
			log.Error("closing the cassette", "path", path, "error", err)
		}
		return ExitError
	}
	w, err := cassette.NewWriter(ctx, c, cassette.WriterOptions{Logger: log, Deriver: derive.New()})
	if err != nil {
		return failed("cannot record into the cassette", err)
	}

	routes := []proxy.Route{{Prefix: "/llm/anthropic", Channel: "llm", Upstream: "anthropic", Target: opts.anthropic}}
	names := make([]string, len(opts.mcp))
	for i, s := range opts.mcp {
		target, _ := absoluteHTTPURL(s.URL) // validated by the flag
		routes = append(routes, proxy.Route{Prefix: "/mcp/" + s.Name, Channel: "mcp", Upstream: "mcp:" + s.Name, Target: target})
		names[i] = s.Name
	}
	res, err := agentrun.Run(ctx, agentrun.Config{
		Logger: env.Logger, Stdin: env.Stdin, Stdout: env.Stdout, Stderr: env.Stderr, NotifySignals: env.notifySignals,
		Listener: ln,
		Listen:   opts.listen,
		Handler:  proxy.New(proxy.Config{Routes: routes, Writer: w, Redact: rules, Logger: log}),
		Cassette: c,
		Writer:   w,
		ID:       id,
		Mode:     string(cassette.ModeRecord),
		StartMsg: "recording",
		MCP:      names,
		Command:  opts.command,
		Finish:   mcpmeta.StoreServerInfo,
	})
	if err != nil {
		return ExitError
	}
	log.Info("session recorded", "session", id, "status", res.Status, "cassette", path, "records", res.Stats.Written)
	return sessionExitCode(res)
}

// createCassette creates the session's cassette and its session row.
func createCassette(ctx context.Context, env *Env, opts recordOptions, rules *redact.Rules) (*cassette.Cassette, string, error) {
	dir := env.Globals.Dir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", fmt.Errorf("creating %s: %w", dir, err)
	}
	id := opts.session
	if id == "" {
		var err error
		if id, err = cassette.NewSessionID(dir, time.Now(), rand.Reader); err != nil {
			return nil, "", err
		}
	}
	path := cassette.Path(dir, id)
	cwd, err := os.Getwd()
	if err != nil {
		return nil, "", fmt.Errorf("reading the working directory: %w", err)
	}
	servers := make([]mcpmeta.Server, len(opts.mcp))
	for i, sv := range opts.mcp {
		u, _ := absoluteHTTPURL(sv.URL)                               // validated by the flag
		servers[i] = mcpmeta.Server{Name: sv.Name, URL: rules.URL(u)} // spec §3.5: no credentials from the URL
	}
	serversJSON, _ := json.Marshal(servers)

	create := session.CreateCassette
	if opts.force {
		create = session.ReplaceCassette
	}
	c, err := create(ctx, path, cassette.Session{
		ID: id, Mode: cassette.ModeRecord, Status: "recording", CreatedAt: time.Now(),
		Command: opts.command, Cwd: cwd, OnMiss: "strict",
		MCPServers: serversJSON, Redaction: rules.Metadata(), RetraceVersion: Version(),
		Labels: opts.labels, DeriverVersion: derive.Version, PricingVersion: anthropic.PricingVersion(),
	}, env.insertSession)
	if errors.Is(err, cassette.ErrExists) {
		return nil, "", fmt.Errorf("session %q already exists in %s (use --force to replace it)", id, dir)
	}
	if err != nil {
		return nil, "", err
	}
	return c, id, nil
}
