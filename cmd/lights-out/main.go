// Command lights-out is the dark factory entry point.
//
// Phase 1 scope: prove of life. It loads and validates the configuration,
// prints it, and exits. Phase 6 replaces this with the real CLI.
package main

import (
	"fmt"
	"os"

	"github.com/Zheng5005/lights-out/internal/config"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "lights-out: %v\n", err)
		os.Exit(1)
	}

	printConfig(cfg, config.ResolvePath())
	os.Exit(0)
}

// printConfig writes the resolved configuration as an indented field list.
func printConfig(cfg config.Config, path string) {
	out := os.Stdout
	fmt.Fprintf(out, "lights-out: configuration loaded\n")
	fmt.Fprintf(out, "  source                  %s\n", path)
	fmt.Fprintf(out, "  enabled                 %t\n", cfg.Enabled)
	fmt.Fprintf(out, "  repo                    %s\n", cfg.Repo)
	fmt.Fprintf(out, "  concurrency_limit       %d\n", cfg.ConcurrencyLimit)
	fmt.Fprintf(out, "  daily_limit             %d\n", cfg.DailyLimit)
	fmt.Fprintf(out, "  labels.trigger          %s\n", cfg.Labels.Trigger)
	fmt.Fprintf(out, "  labels.in_progress      %s\n", cfg.Labels.InProgress)
	fmt.Fprintf(out, "  labels.blocked          %s\n", cfg.Labels.Blocked)
	fmt.Fprintf(out, "  agent.dispatcher        %s\n", cfg.Agent.Dispatcher)
	fmt.Fprintf(out, "  agent.mode              %s\n", cfg.Agent.Mode)
	fmt.Fprintf(out, "  agent.kind              %s\n", cfg.Agent.Kind)
	fmt.Fprintf(out, "  worktree.base           %s\n", cfg.Worktree.Base)
}
