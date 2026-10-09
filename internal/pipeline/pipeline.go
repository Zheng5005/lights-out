// Package pipeline runs one factory cycle: the gate, the capacity checks,
// the claim and lock, the agent execution in an isolated worktree, the pull
// request, and the cleanup — doc/V1.md §4 steps 2 through 7 (FR1–FR6).
package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/Zheng5005/lights-out/internal/config"
	"github.com/Zheng5005/lights-out/internal/daily"
	"github.com/Zheng5005/lights-out/internal/github"
	"github.com/Zheng5005/lights-out/internal/herdr"
)

// agentStatusDone is the ONLY success token accepted from PromptAgent.
// internal/herdr's package doc pins it: a headless agent reports "done",
// never "idle" — idle requires the agent's tab to be seen by a focused UI,
// which a factory session never provides. Every other token is a failure.
const agentStatusDone = "done"

// worktreeToken is the exact token in a configured agent.args element that is
// replaced by the run's absolute worktree path before StartAgent. Only a whole
// element matching the token is expanded; a token merely embedded in a larger
// argument is left alone.
const worktreeToken = "{worktree}"

// execFunc runs one external command named name with args in the working
// directory dir and returns its combined output, plus a non-nil error when
// the process could not start or exited non-zero. It is the pipeline's
// injectable-exec seam — the same approach as internal/herdr's execFunc:
// the production implementation (runGit) shells out with os/exec, tests pass
// a recorder instead, and no test ever touches a real binary.
type execFunc func(ctx context.Context, dir, name string, args ...string) (output []byte, err error)

// Run executes one factory cycle — doc/V1.md §4 steps 2 through 7 (FR1–FR6) —
// through the real git binary. Tests call RunWithRunner to inject the exec
// seam instead.
//
// Silent nil exits are specified behaviour, not omissions: the disabled
// gate, both capacity limits, and an empty candidate list return nil with
// no comment and no side effect (each announces itself with one Info log).
// Errors from the capacity, claim, and lock steps are returned unchanged;
// each client already wraps its own context, and a failed lock ends the
// cycle before any work starts.
//
// Everything after the lock belongs to execute: the agent run in an
// isolated worktree, the git push, the PR, and — on every exit path
// including panic — the worktree removal, the lock release, and at most
// one failure comment (FR4, FR6).
func Run(ctx context.Context, cfg config.Config, gh github.Client, hd herdr.HerdrClient, daily *daily.Counter) error {
	return run(ctx, cfg, gh, hd, daily, runGit)
}

// RunWithRunner is Run's test seam — the same approach as
// herdr.NewWithRunner: runner replaces the executor that performs the git
// push, so tests never touch a real binary. Production code calls Run.
func RunWithRunner(ctx context.Context, cfg config.Config, gh github.Client, hd herdr.HerdrClient, daily *daily.Counter, runner execFunc) error {
	return run(ctx, cfg, gh, hd, daily, runner)
}

// run is the shared body of Run and RunWithRunner: a straight-line
// translation of doc/V1.md §4. The read-only decisions of steps 2–4 (gate,
// capacity, claim) live in resolveClaim, which DryRun shares; the FR4 lock
// and the post-lock steps 5–7 (execute, success, error) live here and in
// execute, whose single defer wraps the whole post-lock section.
func run(ctx context.Context, cfg config.Config, gh github.Client, hd herdr.HerdrClient, daily *daily.Counter, runner execFunc) error {
	issue, ok, err := resolveClaim(ctx, cfg, gh, daily)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	// FR4 — the label is the lock, and the lock precedes the charge. If
	// AddLabel fails the claim never took, so Increment must not run and
	// the error ends the cycle before any work starts.
	if err := gh.AddLabel(ctx, issue.Number, cfg.Labels.InProgress); err != nil {
		return err
	}
	slog.Info(fmt.Sprintf("lock=applied (#%d)", issue.Number))
	if _, err := daily.Increment(); err != nil {
		return err
	}

	// The lock holds: §4 steps 5–7 — execute, land, clean up.
	return execute(ctx, cfg, gh, hd, issue, runner)
}

// resolveClaim runs the read-only decisions of doc/V1.md §4 steps 2–4 — the
// FR1 gate, the FR2 concurrency capacity, the FR3 daily capacity, and the
// oldest-candidate head selection — and returns the issue to claim, whether
// the claim should proceed (ok), and the first error encountered.
//
// It mutates nothing: no label, no counter, no comment, no worktree. Run
// continues to the FR4 lock after ok is true; DryRun stops there and reports
// what a real run would have done.
func resolveClaim(ctx context.Context, cfg config.Config, gh github.Client, daily *daily.Counter) (github.Issue, bool, error) {
	// FR1 — gate. Checked before anything else; false means no action at all.
	if !cfg.Enabled {
		slog.Info("gate=disabled")
		return github.Issue{}, false, nil
	}
	slog.Info("gate=passed")

	// FR2 — concurrency capacity: open locks against the limit. The
	// in-progress label doubles as the concurrency counter (§6), so the
	// same label later applied as the lock is the one counted here.
	inProgress, err := gh.CountIssuesByLabel(ctx, cfg.Labels.InProgress)
	if err != nil {
		return github.Issue{}, false, err
	}
	if inProgress >= cfg.ConcurrencyLimit {
		slog.Info(fmt.Sprintf("capacity=full (%d/%d)", inProgress, cfg.ConcurrencyLimit))
		return github.Issue{}, false, nil
	}
	slog.Info(fmt.Sprintf("capacity=ok (%d/%d)", inProgress, cfg.ConcurrencyLimit))

	// FR3 — daily capacity. The UTC day boundary belongs to daily.Counter;
	// resolveClaim only compares its reading against the configured limit.
	claimedToday, err := daily.Current()
	if err != nil {
		return github.Issue{}, false, err
	}
	if claimedToday >= cfg.DailyLimit {
		slog.Info(fmt.Sprintf("daily=reached (%d/%d)", claimedToday, cfg.DailyLimit))
		return github.Issue{}, false, nil
	}
	slog.Info(fmt.Sprintf("daily=ok (%d/%d)", claimedToday, cfg.DailyLimit))

	// Claim — candidates arrive oldest first (the github.Client contract),
	// so the head of the list is the oldest eligible issue.
	candidates, err := gh.ListClaimCandidates(ctx)
	if err != nil {
		return github.Issue{}, false, err
	}
	if len(candidates) == 0 {
		slog.Info("candidate=none")
		return github.Issue{}, false, nil
	}
	issue := candidates[0]
	slog.Info(fmt.Sprintf("candidate=#%d", issue.Number))
	return issue, true, nil
}

// DryRun walks the read-only decisions of one factory cycle — the FR1 gate,
// the FR2 concurrency capacity, the FR3 daily capacity, and the
// oldest-candidate head selection — and logs what a real run would do,
// mutating nothing.
//
// It is read-only by construction and by signature: it has no Herdr client,
// so it can never create a worktree, start an agent, or run a push; it calls
// only the read-only github.Client methods (CountIssuesByLabel and
// ListClaimCandidates) through resolveClaim; and it never applies a label,
// charges the daily counter, posts a comment, or opens a PR. A claim that
// would proceed is reported, never taken.
func DryRun(ctx context.Context, cfg config.Config, gh github.Client, daily *daily.Counter) error {
	slog.Info("mode=dry-run")
	issue, ok, err := resolveClaim(ctx, cfg, gh, daily)
	if err != nil {
		return err
	}
	if !ok {
		slog.Info("would exit 0, no candidate")
		return nil
	}
	slog.Info(fmt.Sprintf("would claim #%d (AddLabel, Increment, agent, push, PR skipped)", issue.Number))
	return nil
}

// execute owns doc/V1.md §4 steps 5 through 7 — probe, worktree, agent,
// push, PR, and the error path — plus ALL cleanup for that section.
//
// The deferred function is registered before the first fallible step and
// therefore runs on every exit: normal return, error, or panic (FR4). It
// recovers a panic into the returned error, removes the worktree, posts at
// most one failure comment (FR6), and releases the in_progress lock —
// replacing it with blocked on failure (§4 step 7). Cleanup errors never
// mask an earlier error; with no earlier error, the first cleanup failure
// becomes the returned error.
func execute(ctx context.Context, cfg config.Config, gh github.Client, hd herdr.HerdrClient, issue github.Issue, runner execFunc) (err error) {
	branch := fmt.Sprintf("factory/issue-%d", issue.Number)
	agent := fmt.Sprintf("factory-issue-%d", issue.Number)

	// Captured by the cleanup below and filled in as step 5 progresses: the
	// workspace id arrives with the worktree, BEFORE Path is validated, so
	// even a partially extracted WorktreeInfo is torn down.
	var info herdr.WorktreeInfo
	commentPosted := false

	defer func() {
		// Panic recovery: a panic becomes the run's error and falls through
		// to the full cleanup — a crash must never leak a worktree or strand
		// the lock, and it surfaces as an ordinary error to the caller.
		if r := recover(); r != nil {
			err = fmt.Errorf("pipeline: panic while executing issue #%d: %v", issue.Number, r)
		}

		// FR4(a) — remove the worktree on EVERY exit path, ahead of the
		// GitHub mutations so a removal failure is still described by the
		// comment below. RemoveWorktree also closes the workspace (herdr
		// 0.9.3 contract), so there is no separate close step. No workspace
		// id means CreateWorktree never delivered a handle: nothing to remove.
		if info.WorkspaceID != "" {
			if cerr := hd.RemoveWorktree(ctx, info.WorkspaceID); cerr != nil && err == nil {
				err = fmt.Errorf("cleanup: remove worktree %s: %w", info.WorkspaceID, cerr)
			}
		}

		// FR6 — exactly one failure comment per run: a single posting site,
		// and the flag is set before the attempt, so no sequence of failures
		// (including a failed comment post itself) can ever produce a second.
		if err != nil && !commentPosted {
			commentPosted = true
			_ = gh.CommentOnIssue(ctx, issue.Number, failureComment(err))
		}

		// FR4(b) — release the lock on EVERY exit path. On success step 6
		// already removed it and RemoveLabel is idempotent (a 404 returns
		// nil), so this second release is a harmless no-op; on failure it is
		// THE release. On success the state was already reached by step 6,
		// so this verification call's error cannot change the outcome; on
		// failure err is already set and it cannot mask that error either.
		_ = gh.RemoveLabel(ctx, issue.Number, cfg.Labels.InProgress)

		// §4 step 7 — replace the lock with the terminal blocked label, so
		// the issue is visible, not re-claimed automatically, and left for a
		// human (§6). A failed run is never left un-blocked.
		if err != nil {
			_ = gh.AddLabel(ctx, issue.Number, cfg.Labels.Blocked)
		}

		// Success only: the failure path already reports through the
		// returned error and the single FR6 comment above.
		if err == nil {
			slog.Info("cleanup=ok (worktree removed, lock released)")
		}
	}()

	// Step 5 — execute. Probe first: the plan's fail-fast check (Phase 4
	// IMPORTANT). A down server must reach the error path as an error, never
	// a panic from a call made against a dead session.
	if err = hd.Probe(ctx); err != nil {
		return fmt.Errorf("herdr probe: %w", err)
	}

	// The worktree for this issue: a dedicated branch off the configured
	// base, created from the target repo's local clone.
	info, err = hd.CreateWorktree(ctx, cfg.Worktree.Path, branch, cfg.Worktree.Base)
	if err != nil {
		return fmt.Errorf("create worktree: %w", err)
	}
	// Validate before driving the worktree: the Phase 4 live smoke pins
	// Path, RootPaneID, and WorkspaceID as present, so an empty field means
	// extraction failed and the run must not proceed on partial ids. The
	// deferred cleanup above still removes it when WorkspaceID did arrive.
	var missing []string
	if info.Path == "" {
		missing = append(missing, "Path")
	}
	if info.RootPaneID == "" {
		missing = append(missing, "RootPaneID")
	}
	if info.WorkspaceID == "" {
		missing = append(missing, "WorkspaceID")
	}
	if len(missing) > 0 {
		return fmt.Errorf("create worktree: incomplete WorktreeInfo (missing %s)",
			strings.Join(missing, ", "))
	}
	slog.Info(fmt.Sprintf("worktree=ready (workspace %s)", info.WorkspaceID))

	// Split a pane down from the worktree's root pane, with the worktree
	// checkout as its cwd, and start the agent in it. The agent name is
	// derived from the issue number, so it is unique per run.
	//
	// The configured agent args are passed through agentArgs so the {worktree}
	// token expands to this run's absolute checkout path. They pre-authorize
	// the worktree for an unmanned run: agy `--add-dir <abs path>` suppresses
	// the project-trust dialog (which otherwise blocks startup with
	// agent_not_ready), and `--dangerously-skip-permissions` auto-approves
	// tool-permission requests — both are required.
	paneID, err := hd.SplitPane(ctx, info.RootPaneID, "down", info.Path)
	if err != nil {
		return fmt.Errorf("split pane: %w", err)
	}
	if err = hd.StartAgent(ctx, agent, cfg.Agent.Kind, paneID, agentArgs(cfg.Agent.Args, info.Path)...); err != nil {
		return fmt.Errorf("start agent %s: %w", agent, err)
	}
	slog.Info("agent=started")

	// PromptAgent ALONE: it already sends --wait and blocks until the agent
	// settles, returning the settled status token. A second WaitAgent call
	// would be redundant. Anything but "done" (never "idle" — see
	// agentStatusDone) is a failure carrying the agent's diagnostic output.
	status, err := hd.PromptAgent(ctx, agent, promptText(issue), cfg.Agent.Timeout)
	if err != nil {
		return withAgentOutput(ctx, hd, agent, fmt.Errorf("prompt agent %s: %w", agent, err))
	}
	slog.Info(fmt.Sprintf("agent=settled %s", status))
	if status != agentStatusDone {
		return withAgentOutput(ctx, hd, agent,
			fmt.Errorf("agent %s settled %q, want %q", agent, status, agentStatusDone))
	}

	// The agent edits files but does not commit them, so stage and commit its
	// work BEFORE the push: a bare agy run leaves the branch empty, and
	// CreatePR would then fail with "No commits between...". A `done` settle
	// with neither a dirty tree nor a commit ahead of the base is a failed run
	// — never push an empty branch.
	if err = commitAgentWork(ctx, runner, info.Path, cfg.Worktree.Base, issue.Number); err != nil {
		return fmt.Errorf("commit agent work: %w", err)
	}

	// Step 5b — push the issue branch from the worktree checkout. A push
	// failure is a run failure: step 6 opens the PR only from a branch that
	// exists on the remote, so there is no partial success.
	if err = pushBranch(ctx, runner, info.Path, branch); err != nil {
		return err
	}
	slog.Info(fmt.Sprintf("push=ok (branch %s)", branch))

	// Step 6 — success: open the PR (with the closing keyword GitHub needs
	// to close the issue on merge) and drop the lock label. Any failure here
	// routes to the error path in the deferred cleanup. FR5: this package
	// has no merge — ever.
	if _, err = gh.CreatePR(ctx, github.PullRequestSpec{
		Head:  branch,
		Base:  cfg.Worktree.Base,
		Title: issue.Title,
		Body:  prBody(issue),
	}); err != nil {
		return fmt.Errorf("create PR: %w", err)
	}
	slog.Info(fmt.Sprintf("pr=opened (#%d)", issue.Number))
	if err = gh.RemoveLabel(ctx, issue.Number, cfg.Labels.InProgress); err != nil {
		return fmt.Errorf("release lock: %w", err)
	}
	return nil
}

// agentArgs expands the configured agent binary arguments for one run: every
// element exactly equal to "{worktree}" is replaced by the run's absolute
// worktree path; every other element passes through unchanged. It returns nil
// when nothing is configured, and never mutates the configured slice.
func agentArgs(configured []string, path string) []string {
	if len(configured) == 0 {
		return nil
	}
	args := make([]string, len(configured))
	for i, arg := range configured {
		if arg == worktreeToken {
			args[i] = path
		} else {
			args[i] = arg
		}
	}
	return args
}

// commitAgentWork stages and commits whatever the agent left in the worktree
// so the pushed branch actually carries its changes. agy edits files without
// committing, so a `done` settle followed directly by a push would push an
// empty branch — the realistic live case is therefore a dirty worktree.
//
// Two cases:
//   - a dirty worktree (modified or untracked files) is staged with
//     `git add -A` and committed with a conventional, attribution-free
//     message;
//   - a clean worktree means the agent committed its own work, so HEAD must
//     differ from the base ref. When HEAD equals base the agent settled
//     `done` without producing any changes, and the run fails instead of
//     pushing an empty branch.
//
// Every command failure is wrapped with the command, the directory, and git's
// trimmed output so the single failure comment (FR6) carries the reason.
func commitAgentWork(ctx context.Context, runner execFunc, dir, base string, issueNum int) error {
	fail := func(command string, out []byte, err error) error {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("%s (in %s): %w: %s", command, dir, err, msg)
		}
		return fmt.Errorf("%s (in %s): %w", command, dir, err)
	}

	statusOut, err := runner(ctx, dir, "git", "status", "--porcelain")
	if err != nil {
		return fail("git status --porcelain", statusOut, err)
	}

	if strings.TrimSpace(string(statusOut)) == "" {
		// Clean tree: the agent must have committed its own work.
		head, err := runner(ctx, dir, "git", "rev-parse", "HEAD")
		if err != nil {
			return fail("git rev-parse HEAD", head, err)
		}
		baseOut, err := runner(ctx, dir, "git", "rev-parse", base)
		if err != nil {
			return fail("git rev-parse "+base, baseOut, err)
		}
		if strings.TrimSpace(string(head)) == strings.TrimSpace(string(baseOut)) {
			return fmt.Errorf(
				"agent settled done for issue #%d but the worktree has no changes (HEAD equals %s); refusing to push an empty branch",
				issueNum, base)
		}
		return nil
	}

	if out, err := runner(ctx, dir, "git", "add", "-A"); err != nil {
		return fail("git add -A", out, err)
	}
	message := fmt.Sprintf("chore: apply changes for issue #%d", issueNum)
	if out, err := runner(ctx, dir, "git", "commit", "-m", message); err != nil {
		return fail("git commit -m "+message, out, err)
	}
	return nil
}

// pushBranch pushes branch from the worktree checkout dir (never from the
// lights-out repo itself) through the injected executor. On failure it
// returns git's trimmed output so the error — and the single failure comment
// built from it — carries the reason the push was rejected.
func pushBranch(ctx context.Context, runner execFunc, dir, branch string) error {
	out, err := runner(ctx, dir, "git", "push", "origin", branch)
	if err == nil {
		return nil
	}
	if msg := strings.TrimSpace(string(out)); msg != "" {
		return fmt.Errorf("git push %s (in %s): %w: %s", branch, dir, err, msg)
	}
	return fmt.Errorf("git push %s (in %s): %w", branch, dir, err)
}

// runGit is the production execFunc: one command through
// exec.CommandContext in the working directory dir, with stdout and stderr
// combined — git push's diagnostics are only ever read as a single message.
func runGit(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}

// promptText is the agent's task: the issue's title and body (§4 step 5).
func promptText(issue github.Issue) string {
	if body := strings.TrimSpace(issue.Body); body != "" {
		return issue.Title + "\n\n" + body
	}
	return issue.Title
}

// prBody is the PR description: the issue body followed by the closing
// keyword, which is what makes GitHub close the issue when a human merges
// the PR (§4 step 6; FR5 keeps the merge itself out of the factory's hands).
func prBody(issue github.Issue) string {
	var b strings.Builder
	if body := strings.TrimSpace(issue.Body); body != "" {
		b.WriteString(body)
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "Closes #%d", issue.Number)
	return b.String()
}

// failureComment is the body of the one comment FR6 allows per run: what
// happened and why the run stopped (§4 step 7).
func failureComment(err error) string {
	return fmt.Sprintf("The dark factory stopped working on this issue:\n\n```\n%s\n```", err)
}

// withAgentOutput attaches the agent's recent output to cause so the single
// failure comment carries the diagnostic body of §4 step 5. A failed read
// degrades the message; it never replaces the original cause.
func withAgentOutput(ctx context.Context, hd herdr.HerdrClient, agent string, cause error) error {
	out, err := hd.ReadAgent(ctx, agent)
	if err != nil {
		return fmt.Errorf("%w (agent output unavailable: %v)", cause, err)
	}
	return fmt.Errorf("%w\nagent output:\n%s", cause, out)
}
