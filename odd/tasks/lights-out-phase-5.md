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

### T1 — config: `worktree.path` (required, no default) ✅ DONE — `cbbe939`
- [x] Add `Path string \`yaml:"path"\`` to `Worktree`.
- [x] No pointer mirror in `configFile` — absent and explicit-empty are both invalid,
      so there is no default case to distinguish. (Contrast the limits/timeout, where
      the pointer separates absent from explicit-non-positive.)
- [x] `Validate`: non-blank, `~`-expanded, absolute. Error names the key.
- [x] No existence/git-ness check at load — Phase 4 precedent holds.
- [x] Documented in `config.yaml` as the local clone of the **target** repo.
- [x] Tests: absent/empty/blank/relative/non-expanded-tilde all rejected; absolute
      accepted; `~/...` expanded; `TestShippedConfigExampleIsValid` green.

Verified independently by the parent: `gofmt` clean, build OK, vet OK, 227 tests in
5 packages. `ResolveDefaults` confirmed to never touch `Path`; `validateWorktreePath`
confirmed to perform no `os.Stat`.

### T2 + T3 — gate, capacity, claim, lock ✅ DONE — `7c4ba8c`
> Merged into one work unit: gate→capacity→claim→lock is the smallest unit that is
> actually meaningful. A gate-only pipeline would check capacity and exit without
> doing anything.

- [x] `internal/pipeline.Run(ctx, cfg, gh, hd, daily) error`.
      Signature takes `herdr.HerdrClient` from day one so T4 widens the body, never
      the signature — no caller breaks. `nil` is valid until execution lands.
- [x] Gate: `!cfg.Enabled` → silent `nil` (FR1).
- [x] Concurrency: `CountIssuesByLabel(in_progress) >= cfg.ConcurrencyLimit` → silent.
- [x] Daily: `daily.Current() >= cfg.DailyLimit` → silent (FR3; UTC boundary stays
      owned by `daily.Counter`, not reimplemented).
- [x] `ListClaimCandidates` → empty is a silent exit; head of list is the oldest.
- [x] `AddLabel` **before** `daily.Increment()` (FR4). AddLabel failure returns the
      error with no Increment, no comment, no work.
- [x] Tests: FR1 asserts all six `Client` methods individually plus a shared call
      sequence, with `nil` herdr so any dispatch panics. FR4 uses **exact sequence
      equality**, not presence — a reordering regression cannot pass silently.

Verified independently by the parent: `gofmt` clean, build OK, vet OK, 238 tests in
6 packages. `pipeline.go` read in full — a straight-line translation of PRD §4 steps
2–4 with no invented logging or premature abstraction.

### T4 + T5 + T6 — execute, land, and clean up ✅ DONE — `aff1c90`
> Merged into one work unit: the defer cleanup and the FR6 single-comment rule are
> cross-cutting concerns that must wrap the whole post-lock section from the first
> line. Splitting them would ship an intermediate commit that leaks worktrees and
> strands issues in `in_progress`.

- [x] `herdr.Probe` first, fail fast. Down server reaches the error path, never a panic.
- [x] `CreateWorktree` → validates `Path`/`RootPaneID`/`WorkspaceID` all non-empty.
- [x] `SplitPane(RootPaneID, "down", info.Path)` → `StartAgent` → `PromptAgent`.
- [x] **`PromptAgent` alone** — it already sends `--wait`. `WaitAgent` is never called;
      the happy-path test asserts `waitCalls` is empty to pin this.
- [x] Success status is **`done` only**, never `idle` — see the correction note below.
- [x] On failure: `ReadAgent` output is carried into the single failure comment.
- [x] git push via `os/exec` in `info.Path`, branch `factory/issue-<N>`, injectable
      exec seam (`RunWithRunner`) mirroring `internal/herdr`'s `NewWithRunner`.
- [x] `CreatePR` with `Closes #<N>` in the body.
- [x] Defer cleanup on ALL paths including panic: worktree removed, `in_progress`
      released, `blocked` applied on failure. Cleanup errors never mask the original.
- [x] FR6: one posting site, flag set *before* the attempt — no failure sequence can
      produce a second comment.
- [x] Tests: 23 in the package, including probe-down, incomplete `WorktreeInfo`,
      blocked settle, push failure, PR failure, panic, and the double-failure
      single-comment case.

Verified independently by the parent: `gofmt` clean, build OK, vet OK, 250 tests in
6 packages, `-race` clean. `pipeline.go` read in full.

### T6.5 — minimal CLI wiring (a Phase 6 prerequisite, not Phase 6 itself)
> T7 cannot run the live loop with `main.go` still the Phase 1 stub. This is the
> smallest wiring that makes one cycle executable. Structured step logging and
> `--dry-run` remain Phase 6 work.

- [x] `cmd/lights-out/main.go`: load config → build clients → `pipeline.Run` → exit code.
- [x] SIGINT/SIGTERM wired to the context so an interrupted cycle unwinds through the
      deferred cleanup rather than being killed (FR4).
- [x] Exit 0 for a completed cycle *including the silent no-ops*; 1 only on failure.
- [x] **Verified against the real binary:** with `enabled: false` the binary prints
      nothing and exits 0 — FR1 proven live, not just in tests.

### T7 — full live end-to-end loop (RESOLVED: full loop, both paths)

Target repo: `Zheng5005/Spotify-clone-frontend-focus-`
Local clone (this is `worktree.path`): `~/Documents/Projects/Spotify-clone-frontend-focus-`
Cloned at `3d9b123`, clean, default branch `main` — matches `worktree.base`.

Both test issues already exist and carry `dark-factory`; all three labels already
exist on the target repo, so no label setup is needed.

- [x] Run 1 — **success path**, issue **#2** "Testing Lights out":
      "make a simple 10 line change on the repo, it can comments."
      ✅ **PASSED LIVE (2026-10-08):** PR **#5** opened, head `factory/issue-2`,
      body end `Closes #2`, 1 commit, +11 `src/App.tsx`, mergeable; `in_progress`
      released (labels back to `dark-factory`); no stray worktrees; counter 1.
- [x] **Human merges PR #5 for #2** ✅ user merged → #2 closed (labels: `dark-factory`,
      state closed). This was load-bearing and done by the human, keeping FR5 intact.
- [x] Run 2 — **error path**, issue **#4** "Light outs blocked":
      "Don't do anything, just fail."
      ✅ **PASSED LIVE (2026-10-08):** agent settled `done` but left no changes →
      T8 commit step refused the empty push; FR6 exactly ONE comment carrying the
      reason; `factory-blocked` applied; `in_progress` released; no PR; no strays.
- [x] Confirm after run 2: `in_progress` count back to 0 (both issues),
      no stray worktrees in the clone (only `main` in `git worktree list`),
      daily counter = 2 = limit (FR3 cap reached; a third run would silently no-op).

### T8 — headless agent run: trust dialogs, forced cleanup, and the missing commit
> T7's first live attempt was blocked at `StartAgent` (`agent_not_ready`) by agy's
> project-trust dialog; cleanup then hit a second live defect (`dirty_worktree_requires_force`),
> and a code read exposed a third (no commit between agent work and push). One work unit.

- [x] Discovery (live, this session): `--dangerously-skip-permissions` ALONE does NOT
      suppress agy's trust dialog on a fresh worktree — herdr maps `trust_prompt` →
      `state=blocked` → `agent_not_ready`. Empirically: exact-path pre-trust in
      `settings.json` works but mutates the user's global config; `agy --add-dir <abs-path>`
      suppresses the trust dialog (reproduced 3×); `--add-dir .` does NOT (relative paths
      are not trusted). `--add-dir <abs-path>` + `--dangerously-skip-permissions` = ZERO
      dialogs end-to-end: start `idle`/ready, prompt settled `done`, agent wrote files.
- [x] Design: `agent.args []string` in config; exact token `{worktree}` replaced by the
      run's absolute worktree path; `StartAgent` passes args after `--` (herdr 0.9.3:
      `agent start <name> --kind <kind> --pane <id> [-- [AGENT_ARG]...]`).
- [x] Live defect: `worktree remove` without `--force` fails with `dirty_worktree_requires_force`
      on an agent-dirtied worktree; Phase 4 doc (line 19) already pinned `--force`. Add it.
- [x] Code-read defect: pipeline has NO commit step (plan §197-240 step table cites only
      `git push`); a bare-agent run edits files without committing, so the pushed branch
      would carry nothing and `CreatePR` would 422. Add stage + commit before push; an
      agent that settles `done` with neither a commit nor a dirty tree is a failed run.
- [x] Route: delegated to `gentle-ai-worker` (config.go, client.go, pipeline.go + 3 test
      files); one new same-package test file `internal/pipeline/agentargs_test.go` was
      added (deviation from the listed surfaces, accepted — the standard Go mechanism to
      unit-test the unexported `agentArgs`). Parent verified independently (gofmt/build/
      vet/test/race all green, 263 tests) and committed as `bdf2e51`. Committed config.yaml
      template keeps `enabled: false`; the local live-fire `enabled: true` stays uncommitted.
      **Both T8 behaviors proven live by T7's runs: Run 1's agent started headlessly with
      zero dialogs; Run 2's failure came from the new commit step, not from any dialog.**

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

- ✅ T1 (`cbbe939`) — worktree.path config field
- ✅ T2+T3 (`7c4ba8c`) — gate/capacity/claim/lock
- ✅ T4+T5+T6 (`aff1c90`) — execute/land/cleanup
- ✅ T6.5 (`dba5284`) — CLI wiring (FR1 proven live)
- ✅ T7 — RESOLVED: full loop, both paths, **verified live** — Run 1 success (#2 → PR #5,
  human merged, #2 closed), Run 2 error (#4 → one comment, blocked, no PR, no strays,
  counter = 2 = limit)
- ✅ T8 (`bdf2e51`) — dialog fix (config `agent.args` + `{worktree}` token + StartAgent
  passthrough), `--force` cleanup, commit-before-push; 263 tests green; both behaviors
  proven live by T7's runs

## Next step

Phase 5 is functionally complete. Remaining: deliver to `main` as the single
`size:exception` PR (human-chosen strategy, D2) — the user owns the timing of the push
and PR. Optional tidy-up: delete the stale remote branch `factory/issue-2` (the repo does
not auto-delete head branches on merge). Remember `config.yaml` `enabled: true` remains an
uncommitted local arm.
