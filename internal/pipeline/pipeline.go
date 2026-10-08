// Package pipeline runs one factory cycle: the gate, the capacity checks,
// the claim, and the lock of doc/V1.md §4. Execution, push, and PR creation
// belong to the later Phase 5 tasks; Run currently stops as soon as the
// issue is locked.
package pipeline

import (
	"context"

	"github.com/Zheng5005/lights-out/internal/config"
	"github.com/Zheng5005/lights-out/internal/daily"
	"github.com/Zheng5005/lights-out/internal/github"
	"github.com/Zheng5005/lights-out/internal/herdr"
)

// Run executes one factory cycle up to and including the claim lock —
// doc/V1.md §4 steps 2 through 4 (FR1–FR4).
//
// Silent nil exits are specified behaviour, not omissions: the disabled
// gate, both capacity limits, and an empty candidate list return nil with
// no log, no comment, and no side effect. Errors from the capacity, claim,
// and lock steps are returned unchanged; each client already wraps its own
// context, and a failed lock ends the cycle before any work starts.
//
// hd is the Herdr dispatcher the later execution step will drive. This unit
// accepts it but never calls it — nil is valid until execution lands — so
// adding execution later widens Run's body, never its signature.
func Run(ctx context.Context, cfg config.Config, gh github.Client, hd herdr.HerdrClient, daily *daily.Counter) error {
	// FR1 — gate. Checked before anything else; false means no action at all.
	if !cfg.Enabled {
		return nil
	}

	// FR2 — concurrency capacity: open locks against the limit. The
	// in-progress label doubles as the concurrency counter (§6), so the
	// same label later applied as the lock is the one counted here.
	inProgress, err := gh.CountIssuesByLabel(ctx, cfg.Labels.InProgress)
	if err != nil {
		return err
	}
	if inProgress >= cfg.ConcurrencyLimit {
		return nil
	}

	// FR3 — daily capacity. The UTC day boundary belongs to daily.Counter;
	// Run only compares its reading against the configured limit.
	claimedToday, err := daily.Current()
	if err != nil {
		return err
	}
	if claimedToday >= cfg.DailyLimit {
		return nil
	}

	// Claim — candidates arrive oldest first (the github.Client contract),
	// so the head of the list is the oldest eligible issue.
	candidates, err := gh.ListClaimCandidates(ctx)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		return nil
	}
	issue := candidates[0]

	// FR4 — the label is the lock, and the lock precedes the charge. If
	// AddLabel fails the claim never took, so Increment must not run and
	// the error ends the cycle before any work starts.
	if err := gh.AddLabel(ctx, issue.Number, cfg.Labels.InProgress); err != nil {
		return err
	}
	if _, err := daily.Increment(); err != nil {
		return err
	}
	return nil
}
