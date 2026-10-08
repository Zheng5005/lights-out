// Command lights-out is the dark factory entry point.
//
// It loads the configuration, builds the GitHub, Herdr, and daily-counter
// clients, and runs exactly one factory cycle (doc/V1.md §4). Exit status is
// 0 for a completed cycle — including the silent no-op exits, which are
// specified behaviour — and 1 for a cycle that could not start or that failed.
//
// Phase 6 replaces this with the full CLI: structured step logging and the
// --dry-run flag belong there, not here.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Zheng5005/lights-out/internal/config"
	"github.com/Zheng5005/lights-out/internal/daily"
	"github.com/Zheng5005/lights-out/internal/github"
	"github.com/Zheng5005/lights-out/internal/herdr"
	"github.com/Zheng5005/lights-out/internal/pipeline"
)

func main() {
	os.Exit(run())
}

// run performs one cycle and maps its outcome to a process exit code.
//
// context cancellation is wired to SIGINT/SIGTERM so a factory interrupted
// mid-run unwinds through pipeline.Run's deferred cleanup rather than being
// killed: FR4 requires the worktree removal and the lock release to happen
// even when the cycle is stopped early.
func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return fail("load configuration", err)
	}

	gh, err := github.NewClient(cfg)
	if err != nil {
		return fail("build GitHub client", err)
	}

	counterPath, err := daily.Path()
	if err != nil {
		return fail("resolve daily counter path", err)
	}

	if err := pipeline.Run(ctx, cfg, gh, herdr.New(cfg.Agent.Session), daily.New(counterPath)); err != nil {
		return fail("run factory cycle", err)
	}
	return 0
}

// fail reports err on stderr with the step that produced it and returns the
// process exit code. It is the single place a failure becomes a status.
func fail(step string, err error) int {
	fmt.Fprintf(os.Stderr, "lights-out: %s: %v\n", step, err)
	slog.Error("factory cycle failed", "step", step, "err", err)
	return 1
}
