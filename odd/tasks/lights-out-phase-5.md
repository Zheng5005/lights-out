# Lights Out — Phase 5: Pipeline Orchestration

## Objective

Implement `pipeline.Run(ctx, cfg, gh, herdr, daily)`: the full end-to-end loop from
PRD §4. One call walks gate → capacity → claim → lock → execute → success, and any
failure routes to exactly one comment plus the blocked label.

## Source of truth

- `doc/implementation-plan.md` §Phase 5 (line 197)
- `doc/V1.md` §4 End-to-End Workflow (line 21), §6 Label Lifecycle, §8 FR1–FR7

## Constraints (non-negotiable, inherited)

- **FR5** — no code path may merge a PR. Ever. (`internal/github` has no Merge.)
- **FR4** — `in_progress` applied before the agent starts; released on every exit
  path, including panic (defer + recover).
- **FR6** — exactly one failure comment per run. Track a posted flag.
- **FR1** — `enabled: false` exits before any API or agent call.
- Label state is the only source of truth. The daily counter only ever *limits*.
- Never write outside the worktree created for the claim.

## Delivery

- **Strategy:** `size:exception` — **chosen by the human, 2026-10-08.**
  Phase 5 lands as plain work-unit commits on one feature branch, delivered as a
  single PR carrying an explicit `size:exception` marking. No chained PR, no tracker.
  Record the marking in the PR description and label at PR time; this document is
  the durable record of the decision.
- **Forecast:** ~700–900 authored lines (pipeline + git helper + config field + tests).
- Phase 4 precedent: it also shipped well over ~400 with the topology deferred, and
  was delivered by phase-PR rather than a chain. Phase 5 follows the same shape.

## Tasks

### T1 — config: `worktree.path` (required, no default)
- [ ] Add `Path string \`yaml:"path"\`` to `Worktree`.
- [ ] Mirror in `configFile`; it needs no pointer trick (absent == "" == validation
      failure, so the strictness is preserved without one).
- [ ] `Validate`: non-blank, `~`-expanded, absolute. Name the key in the error.
- [ ] Do **not** check existence or git-ness at load. Phase 4 precedent: environment
      correctness belongs to the runtime probe, not config load.
- [ ] Document in `config.yaml` that this is the local clone of the **target** repo,
      not `lights-out`.
- [ ] Tests: default-absent rejected, relative rejected, `~/...` accepted and
      expanded, existing `TestShippedConfigExampleIsValid` still green.

### T2 — pipeline skeleton: gate + capacity
- [ ] `internal/pipeline.Run(ctx, cfg, gh, herdr, daily) error`.
- [ ] Step 1 gate: `!cfg.Enabled` → silent return (FR1).
- [ ] Step 2a concurrency: `gh.CountIssuesByLabel(in_progress) >= cfg.ConcurrencyLimit`.
- [ ] Step 2b daily: `daily.Current() >= cfg.DailyLimit` (FR3, UTC day).
- [ ] Both capacity exits are silent, by design.
- [ ] Tests with fakes: disabled makes zero client calls; at-limit makes zero claims.

### T3 — claim + lock
- [ ] `ListClaimCandidates` → empty slice is a silent exit.
- [ ] `AddLabel(in_progress)` **before** `daily.Increment()` (FR4 ordering).
- [ ] AddLabel failure aborts with no work started, no comment (nothing to report).
- [ ] Tests: ordering asserted; AddLabel error stops the run.

### T4 — execute via Herdr
- [ ] `Probe` first, fail fast (plan Phase 4 IMPORTANT).
- [ ] `CreateWorktree(cfg.Worktree.Path, "factory/issue-<N>", cfg.Worktree.Base)`.
- [ ] Split pane → `StartAgent(cfg.Agent.Kind)` → `PromptAgent` → `WaitAgent`
      with `cfg.Agent.Timeout`.
- [ ] Success statuses: `idle` / `done`. Anything else → error path.
- [ ] `ReadAgent` on failure for the diagnostic body.
- [ ] Tests: fake herdr covering done, blocked, timeout, probe-down.

### T5 — git push + success path
- [ ] New `os/exec` git helper; push the worktree branch `factory/issue-<N>`.
- [ ] `CreatePR` with `Closes #<N>` in the body (PRD §4 step 6).
- [ ] `RemoveLabel(in_progress)`.
- [ ] Push/PR failure → error path, never a partial success.
- [ ] Tests: command construction, PR body contains the closing keyword.

### T6 — error path, FR6, defer cleanup
- [ ] Single-comment guarantee: one bool, one comment, never two (FR6).
- [ ] Swap `in_progress` → `blocked` on terminal failure (PRD §4 step 7).
- [ ] `defer` releases `in_progress` and removes the worktree on ALL paths,
      including panic → recover → report (FR4).
- [ ] Cleanup errors are logged, never mask the original error.
- [ ] Tests: panic path, double-post prevention, cleanup on every exit.

### T7 — full live end-to-end loop (RESOLVED: full loop, both paths)

Target repo: `Zheng5005/Spotify-clone-frontend-focus-`
Local clone (this is `worktree.path`): `~/Documents/Projects/Spotify-clone-frontend-focus-`
Cloned at `3d9b123`, clean, default branch `main` — matches `worktree.base`.

Both test issues already exist and carry `dark-factory`; all three labels already
exist on the target repo, so no label setup is needed.

- [ ] Run 1 — **success path**, issue **#2** "Testing Lights out":
      "make a simple 10 line change on the repo, it can comments."
      Expect: worktree → agent → push → PR with `Closes #2` → `in_progress` released.
- [ ] **Human merges PR for #2.** This is load-bearing, not bookkeeping: FR5 forbids the
      factory from merging, and §6 never removes `dark-factory`, so #2 stays claimable
      until a merge closes it. Without this merge, run 2 re-claims #2 and never reaches #4.
- [ ] Run 2 — **error path**, issue **#4** "Light outs blocked":
      "Don't do anything, just fail."
      Expect: exactly one comment (FR6), `in_progress` → `blocked`, no PR, worktree and
      lock released on the failure path.
- [ ] Confirm after run 2: `in_progress` count back to 0, no stray worktrees in the
      clone, daily counter = 2 (at limit).

## Open decisions

- ~~**D1 — How far is Phase 5 proven?~~ **RESOLVED: full live loop, both paths.**
  See T7 for the exact sequence and the human merge step.
- ~~**D2 — Delivery topology.~~ **RESOLVED: `size:exception`, single PR.** Human chose
  this 2026-10-08, matching Phase 4's phase-PR shape.

## Acceptance criteria

- `pipeline.Run` executes the complete PRD §4 loop against fakes in tests.
- FR1–FR7 each have a test that would fail if the rule were removed.
- No merge capability exists anywhere in the tree (`TestPackageHasNoMergeCapability`).
- `go build`, `go vet`, `go test`, `go test -race` all green.

## Progress

_Not started._

## Next step

Resolve D1, then cut the branch and implement T1.
