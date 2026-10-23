package cli

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
)

// fakeEnv returns a getenv function backed by a map.
func fakeEnv(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

func TestRunExitCodesAndOutput(t *testing.T) {
	echo := command{
		name:    "echo",
		summary: "test command",
		run: func(_ context.Context, env *Env, args []string) int {
			fmt.Fprint(env.Stdout, strings.Join(args, " "))
			return ExitOK
		},
	}

	tests := []struct {
		name       string
		args       []string
		env        map[string]string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "version", args: []string{"--version"}, wantCode: ExitOK, wantStdout: "retrace dev\n"},
		{name: "help goes to stdout", args: []string{"--help"}, wantCode: ExitOK, wantStdout: "Usage: retrace"},
		{name: "help lists commands", args: []string{"-h"}, wantCode: ExitOK, wantStdout: "echo       test command"},
		{name: "no command prints usage to stderr", args: nil, wantCode: ExitUsage, wantStderr: "Usage: retrace"},
		{name: "unknown command", args: []string{"nope"}, wantCode: ExitUsage, wantStderr: `unknown command "nope"`},
		{name: "unknown flag", args: []string{"--nope"}, wantCode: ExitUsage, wantStderr: "flag provided but not defined: -nope"},
		{name: "invalid log level from flag", args: []string{"--log-level", "loud", "echo"}, wantCode: ExitUsage, wantStderr: `invalid log level "loud": want debug, info, warn or error`},
		{name: "invalid log level from env names the variable", args: []string{"echo"}, env: map[string]string{"RETRACE_LOG_LEVEL": "loud"}, wantCode: ExitUsage, wantStderr: `(from RETRACE_LOG_LEVEL)`},
		{name: "flags after the command belong to the command", args: []string{"echo", "--dir", "x"}, wantCode: ExitOK, wantStdout: "--dir x"},
		{name: "dispatches with remaining args", args: []string{"echo", "a", "--b"}, wantCode: ExitOK, wantStdout: "a --b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), tt.args, nil, &stdout, &stderr, fakeEnv(tt.env), []command{echo})

			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d (stderr: %q)", code, tt.wantCode, stderr.String())
			}
			if !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tt.wantStdout)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

func TestRunResolvesGlobals(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		args []string
		want Globals
	}{
		{
			name: "defaults",
			want: Globals{Dir: ".retrace/cassettes", LogLevel: slog.LevelInfo},
		},
		{
			name: "environment",
			env:  map[string]string{"RETRACE_DIR": "/env/dir", "RETRACE_LOG_LEVEL": "debug", "NO_COLOR": "1"},
			want: Globals{Dir: "/env/dir", LogLevel: slog.LevelDebug, NoColor: true},
		},
		{
			name: "--no-color=false cannot re-enable color disabled by NO_COLOR",
			env:  map[string]string{"NO_COLOR": "yes"},
			args: []string{"--no-color=false"},
			want: Globals{Dir: ".retrace/cassettes", LogLevel: slog.LevelInfo, NoColor: true},
		},
		{
			name: "flags override environment",
			env:  map[string]string{"RETRACE_DIR": "/env/dir", "RETRACE_LOG_LEVEL": "debug"},
			args: []string{"--dir", "/flag/dir", "--log-level", "ERROR", "--no-color"},
			want: Globals{Dir: "/flag/dir", LogLevel: slog.LevelError, NoColor: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got Globals
			capture := command{name: "capture", run: func(_ context.Context, env *Env, _ []string) int {
				got = env.Globals
				return ExitOK
			}}
			var stdout, stderr bytes.Buffer
			args := slices.Concat(tt.args, []string{"capture"})

			if code := run(context.Background(), args, nil, &stdout, &stderr, fakeEnv(tt.env), []command{capture}); code != ExitOK {
				t.Fatalf("exit code = %d, want %d (stderr: %q)", code, ExitOK, stderr.String())
			}
			if got != tt.want {
				t.Errorf("globals = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// The "version" case in TestRunExitCodesAndOutput expects "dev" because test
// binaries carry no stamped module version ("(devel)" or empty). If a toolchain
// change starts stamping one, that case fails first: update its expectation.
func TestVersionUsesBuildValue(t *testing.T) {
	saved := version
	t.Cleanup(func() { version = saved })

	version = "v9.9.9"
	if got := Version(); got != "v9.9.9" {
		t.Errorf("Version() = %q, want %q", got, "v9.9.9")
	}
}
