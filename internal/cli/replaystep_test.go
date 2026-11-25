package cli

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/pmlucero/retrace/internal/cassette"
)

// AC-33: the cwd override runs the agent there and skips the recorded-cwd
// checks, for the recorded command (spec §9.9.3). Without it the same source
// is a usage error.
func TestReplayStepCwdOverride(t *testing.T) {
	ctx := context.Background()
	t.Setenv(cwdAgentEnv, "1")
	srcDir, outDir, override := t.TempDir(), t.TempDir(), t.TempDir()
	missing := filepath.Join(t.TempDir(), "gone")
	writeSource(t, srcDir, "s", cwdAgentCommand(), missing)

	src, err := cassette.Open(ctx, cassette.Path(srcDir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	sess, err := src.Session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	opts := runOptions{source: "s", recorded: true, tools: "frozen", onMiss: "strict", grace: time.Second}

	if _, perr := prepareReplay(ctx, src, sess, opts); perr == nil || !perr.usage {
		t.Fatalf("without the override: %v, want a usage error", perr)
	}

	opts.cwd = override
	plan, perr := prepareReplay(ctx, src, sess, opts)
	if perr != nil {
		t.Fatal(perr)
	}
	out := &syncBuffer{}
	env := &Env{Stdout: out, Stderr: &syncBuffer{}, Logger: slog.New(slog.DiscardHandler)}
	res, err := executeReplay(ctx, env, plan, replayTarget{dir: outDir, id: "s.check.1", listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	if res.exitCode != ExitOK || res.status != "completed" || res.id != "s.check.1" {
		t.Errorf("result %+v", res)
	}
	if res.path != cassette.Path(outDir, "s.check.1") {
		t.Errorf("cassette %q, want it in %s", res.path, outDir)
	}
	want, _ := filepath.EvalSymlinks(override)
	if got, err := filepath.EvalSymlinks(out.String()); err != nil || got != want {
		t.Errorf("agent ran in %q, want %q", out.String(), want)
	}
}
