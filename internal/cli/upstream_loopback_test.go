package cli

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// AC-51: plain http upstreams are accepted only for loopback hosts, in
// record, a swap-model run and a swap-model check.
func TestUpstreamLoopbackRefused(t *testing.T) {
	var hits atomic.Int32
	fake := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer fake.Close()

	dir := t.TempDir()
	writeSource(t, dir, "s", cwdAgentCommand(), "/")
	before := dirNames(t, dir)

	cmds := map[string]func(up string) recordRun{
		"record": func(up string) recordRun {
			return runRecordCmd(t, dir, "--session", "r", "--anthropic-upstream", up, "--", "true")
		},
		"run": func(up string) recordRun {
			return runRunCmd(t, dir, "s", "--as", "r", "--model", "m", "--anthropic-upstream", up, "--", "true")
		},
		"check": func(up string) recordRun {
			return runCheckCmd(t, dir, "--out", dir+"/out", "--model", "m", "--anthropic-upstream", up, "--", "true")
		},
	}
	for _, up := range []string{
		"http://api.example.com",
		"http://10.0.0.5:8080",
		"http://127.0.0.1@api.example.com",
		"http://localhost.:8080",
		"http://api.localhost:8080",
	} {
		for name, f := range cmds {
			t.Run(name+" "+up, func(t *testing.T) {
				r := f(up)
				if r.code != ExitUsage {
					t.Fatalf("exit %d, want %d\n%s", r.code, ExitUsage, r.stderr)
				}
				if !strings.Contains(r.stderr.String(), "plain http is allowed only for loopback hosts; use https") {
					t.Errorf("stderr = %q", r.stderr.String())
				}
				if got := dirNames(t, dir); got != before {
					t.Errorf("files changed: %q, was %q", got, before)
				}
			})
		}
	}
	if hits.Load() != 0 {
		t.Errorf("%d requests reached the fake upstream", hits.Load())
	}
}

// AC-51: loopback http hosts and any https host pass the validation.
func TestUpstreamLoopbackAccepted(t *testing.T) {
	ipv6 := false
	if ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp6", "[::1]:0"); err == nil {
		_ = ln.Close()
		ipv6 = true
	}
	for _, up := range []string{
		"http://127.0.0.1:8080",
		"http://127.0.0.2:8080",
		"http://[::1]:8080",
		"http://[::ffff:127.0.0.1]:8080",
		"http://LOCALHOST:8080",
		"https://api.example.com",
		"https://10.0.0.5:8443",
	} {
		t.Run(up, func(t *testing.T) {
			if strings.Contains(up, "[::1]") && !ipv6 {
				t.Skip("IPv6 loopback unavailable")
			}
			var stderr bytes.Buffer
			env := &Env{Stderr: &stderr}
			if _, code, ok := parseRecordFlags(env, []string{"--anthropic-upstream", up, "--", "true"}); !ok {
				t.Errorf("record: exit %d: %s", code, stderr.String())
			}
			if _, code, ok := parseRunFlags(env, []string{"s", "--model", "m", "--anthropic-upstream", up, "--", "true"}); !ok {
				t.Errorf("run: exit %d: %s", code, stderr.String())
			}
			if _, code, ok := parseCheckFlags(env, []string{"s", "--model", "m", "--anthropic-upstream", up, "--", "true"}); !ok {
				t.Errorf("check: exit %d: %s", code, stderr.String())
			}
		})
	}
}

func dirNames(t *testing.T, dir string) string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	return strings.Join(names, ",")
}
