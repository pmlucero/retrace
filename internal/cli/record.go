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
	"regexp"
	"strings"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
	"github.com/pmlucero/retrace/internal/derive"
	"github.com/pmlucero/retrace/internal/provider/anthropic"
	"github.com/pmlucero/retrace/internal/proxy"
	"github.com/pmlucero/retrace/internal/redact"
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

var mcpNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

type recordOptions struct {
	session      string
	mcp          []mcpServer
	listen       string
	listenUnsafe bool
	anthropic    *url.URL
	redactHeader []string
	redactBody   []redact.Path
	labels       map[string]string
	force        bool
	command      []string
}

type mcpServer struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

func parseRecordFlags(env *Env, args []string) (recordOptions, int, bool) {
	opts := recordOptions{labels: map[string]string{}}
	var anthropic string
	fs := flag.NewFlagSet("record", flag.ContinueOnError)
	fs.StringVar(&opts.session, "session", "", "session id (generated if omitted)")
	fs.Func("mcp", "register an MCP server as `name=url` (repeatable)", func(v string) error {
		name, raw, ok := strings.Cut(v, "=")
		if !ok || !mcpNamePattern.MatchString(name) {
			return fmt.Errorf("want name=url, with a name of letters, digits, '_' or '-'")
		}
		if _, err := absoluteHTTPURL(raw); err != nil {
			return err
		}
		for _, s := range opts.mcp {
			if s.Name == name {
				return fmt.Errorf("MCP server %q registered twice", name)
			}
		}
		opts.mcp = append(opts.mcp, mcpServer{Name: name, URL: raw})
		return nil
	})
	fs.StringVar(&opts.listen, "listen", "127.0.0.1:4790", "proxy address; port 0 picks a free port")
	fs.BoolVar(&opts.listenUnsafe, "listen-unsafe", false, "allow a non-loopback --listen address")
	fs.StringVar(&anthropic, "anthropic-upstream", "https://api.anthropic.com", "Anthropic API base URL")
	fs.Func("redact-header", "also redact this request or response `header` (repeatable)", func(v string) error {
		opts.redactHeader = append(opts.redactHeader, v)
		return nil
	})
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
	u, err := absoluteHTTPURL(anthropic)
	if err != nil {
		return opts, usageError(env.Stderr, "--anthropic-upstream: "+err.Error()), false
	}
	opts.anthropic = u
	if err := checkListen(opts.listen, opts.listenUnsafe); err != nil {
		return opts, usageError(env.Stderr, err.Error()), false
	}
	return opts, 0, true
}

func absoluteHTTPURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%q is not an http or https URL", raw)
	}
	return u, nil
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
	res, err := runSession(ctx, env, sessionConfig{
		listener: ln,
		handler:  proxy.New(proxy.Config{Routes: routes, Writer: w, Redact: rules, Logger: log}),
		cassette: c,
		writer:   w,
		id:       id,
		mode:     "record",
		mcp:      names,
		command:  opts.command,
	})
	if err != nil {
		return ExitError
	}
	log.Info("session recorded", "session", id, "status", res.status, "cassette", path, "records", res.stats.Written)
	return res.exitCode()
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
	if opts.force {
		for _, p := range []string{path, path + "-wal", path + "-shm"} {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, "", fmt.Errorf("replacing session %s: %w", id, err)
			}
		}
	}
	c, err := cassette.Create(ctx, path)
	if errors.Is(err, cassette.ErrExists) {
		return nil, "", fmt.Errorf("session %q already exists in %s (use --force to replace it)", id, dir)
	}
	if err != nil {
		return nil, "", err
	}

	cwd, err := os.Getwd()
	if err != nil {
		_ = c.Close()
		return nil, "", fmt.Errorf("reading the working directory: %w", err)
	}
	servers := make([]mcpServer, len(opts.mcp))
	for i, sv := range opts.mcp {
		u, _ := absoluteHTTPURL(sv.URL)                          // validated by the flag
		servers[i] = mcpServer{Name: sv.Name, URL: rules.URL(u)} // spec §3.5: no credentials from the URL
	}
	serversJSON, _ := json.Marshal(servers)
	redaction, _ := json.Marshal(map[string]any{"headers": rules.HeaderNames(), "body": rules.BodyRules()})
	err = c.Write(ctx, func(tx *cassette.Tx) error {
		return tx.InsertSession(ctx, cassette.Session{
			ID: id, Mode: "record", Status: "recording", CreatedAt: time.Now(),
			Command: opts.command, Cwd: cwd, OnMiss: "strict",
			MCPServers: serversJSON, Redaction: redaction, RetraceVersion: Version(),
			Labels: opts.labels, DeriverVersion: derive.Version, PricingVersion: anthropic.PricingVersion(),
		})
	})
	if err != nil {
		_ = c.Close()
		return nil, "", err
	}
	return c, id, nil
}
