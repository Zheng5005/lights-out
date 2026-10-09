# Lights Out — Phase 6: CLI Entry Point (structured logging + `--dry-run`)

## Objective

Turn `./lights-out` into the real CLI: structured step-transition logging with
stdlib `log/slog` (every step: gate, capacity, daily, claim, lock, worktree, agent,
push, PR, cleanup) and a `--dry-run` flag that walks gate → capacity → daily →
candidate selection, logs exactly what it *would* do, and mutates nothing.

Phase 6 task 1 of the plan (load config → init clients → `Run` → exit 0/1) is
**already done** by T6.5 (`dba5284`). This phase is the remaining plan items
(2)–(4): slog logging, log every step transition, and `--dry-run`.

## Source of truth

- `doc/implementation-plan.md` §Phase 6 (line 224) — "CLI Entry Point"
- `doc/V1.md` §4 End-to-End Workflow (line 21), §8 FR1–FR7

## Constraints (non-negotiable, inherited)

- **FR1 intact** — `enabled: false` still exits before any API/agent call. Logging
  is not dispatch. The T6.5 observation "binary prints nothing with
  `enabled: false`" is **superseded**: Phase 6's mandate is "log every step
  transition", so the disabled gate now prints one log line and still exits 0.
  The FR1 unit tests (no `Client` method dispatched) remain valid unchanged.
- **No exported signature changes** — decision D1: logging uses `slog.Default()`;
  `pipeline.Run`/`RunWithRunner`/`run` signatures stay frozen.
- **FR4 exact-sequence tests stay untouched** — `AddLabel` precedes `Increment`;
  the defer/cleanup machinery in `execute` is not restructured by this phase.
- **`DryRun` mutates nothing**: no `AddLabel`, no `daily.Increment()`, no
  `CommentOnIssue`, no `CreatePR`, no `RemoveLabel`. Only read-only calls:
  `CountIssuesByLabel`, `ListClaimCandidates`, `daily.Current()` (file read; a
  stale date rolls to 0 without writing).
- **`DryRun` never touches herdr or the worktree** — the signature takes no
  `herdr.HerdrClient`, which makes the constraint structural.
- Logs go to **stderr**, stdlib `slog` only, text format, `slog.LevelInfo`.
  stdout stays clean for program output. The token is never logged.
- No secrets, no absolute host paths in logs.

## Delivery

- **Strategy:** `ask-on-risk` (default). **Forecast: ~350 authored lines**
  (logging ~100, DryRun + shared selection ~160, CLI ~50, docs/verification ~40).
  Under the ~400 line budget; if the running count crosses it, ask once for the
  chain strategy (repo precedent: Phase 4/5 shipped as single phase-PRs).
- Branch `feat/phase-6-cli` from `main` (`7ca6edc`), work-unit commits per task
  (Conventional Commits), human merges the PR at the end. `config.yaml` template
  keeps `enabled: false`.
- RDD is **off** (user-owned). Per-commit review assessment is skipped; the parent
  verifies independently (gofmt/build/vet/test/race) and the live dry-run check in
  T4 is read-only.

## Tasks

### T1 — step-transition logging in `internal/pipeline` (via `slog.Default()`) ✅ DONE — `78ec9ae`
- [x] Transitions logged at `Info` in `run()`: `gate=passed` / `gate=disabled` (FR1,
      still exit 0, still no dispatch), `capacity=ok (n/m)` / `capacity=full (n/m)`,
      `daily=ok (n/m)` / `daily=reached (n/m)`, `candidate=#N` (head of list),
      `candidate=none`.
- [x] Transitions logged in `execute()`: `lock=applied`, `worktree=ready
      (workspace <id>)`, `agent=started`, `agent=settled <status>`, `push=ok
      (branch <b>)`, `pr=opened (#<n>)`, `cleanup=ok (worktree removed, lock released)`.
      `cleanup=ok` is guarded by `err == nil` inside the defer — success path only;
      the failure path reports through the returned error and the FR6 comment.
- [x] No signature changes; each statement is pure logging next to the existing
      decisions. The FR1/FR4/FR6 test suites pass with their assertions UNCHANGED.
      Diff is insertions + import only (21 lines in `pipeline.go`, zero
      reordering — parent verified the full diff).
- [x] Tests: `TestMain` in the package silences the default logger suite-wide
      (`slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))`); new
      `logging_test.go` has a strict-RED capture test (4 subtests: silent-exit
      lines, disabled-gate-only, full-cycle line set through cleanup, failure
      path with `cleanup=ok` absent). RED observed: `"" does not contain
      "gate=passed"` (5 failed); GREEN: 36 passed. `pipeline_test.go` untouched.
- [x] Work-unit commit `78ec9ae` — suite green (gofmt clean, build/vet OK,
      268 tests in 6 packages = 263 base + 5 new, `-race` green).
- [x] RDD off (user-owned) → parent verification gate: native assess over the
      writer diff = **medium** (`executable_change` on `logging_test.go`),
      `review_due: false` (`under_budget`); tier medium → writer self-verification
      + parent spot check (gofmt re-run clean, `go test ./internal/pipeline/...`
      → 36 passed). No independent verifier required (no small-model profile).
      Note: assess needed the canonical untracked inventory digest from the
      selectorless preflight STATUS (`eligible_untracked_inventory`); the START
      it returned was NOT executed (RDD off).

### T2 — CLI: slog handler setup + `--dry-run` flag in `cmd/lights-out/main.go` ✅ DONE — `ec6a647`
> T2 and T3 were delegated as ONE writer unit (the flag routes to
> `pipeline.DryRun`, which T3 creates — they only compile together) and
> committed as two separate reviewable work units: pipeline first (`5b5a163`),
> then the CLI (`ec6a647`). Precedent: Phase 5 T2+T3.

- [x] `slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, ...)))` at
      `LevelInfo`, first line of `run()`, before any work; failures stay
      `Error` (existing `fail()`).
- [x] `flag.Bool("dry-run", ...)` + `flag.Parse()`; `dry-run` routes to
      `pipeline.DryRun(ctx, cfg, gh, counter)` with fail step
      `"run factory cycle (dry-run)"`; otherwise the existing `pipeline.Run`
      path. Exit 0 on nil from either; 1 on error. SIGINT/SIGTERM context
      wiring untouched. In dry-run mode the herdr client is never constructed.
- [x] File-header comment rewritten — it *is* the full CLI now.
- [x] Work-unit commit `ec6a647` — build/vet green; full suite green (276 tests).

### T3 — shared candidate selection + `pipeline.DryRun` ✅ DONE — `5b5a163`
- [x] `resolveClaim(ctx, cfg, gh, daily) (github.Issue, bool, error)` extracted
      from `run()` — the FR1 gate, FR2 capacity, FR3 daily, and head-of-list
      selection, preserving every log line and client-call order byte for byte.
      `run()` keeps the pinned FR4 sequence (`AddLabel` → `lock=applied` →
      `Increment` → `execute`); existing exact-sequence tests pass UNMODIFIED.
- [x] `pipeline.DryRun(ctx, cfg, gh, daily) error`: logs `mode=dry-run`, walks
      `resolveClaim`, logs `would exit 0, no candidate` or `would claim #N
      (AddLabel, Increment, agent, push, PR skipped)`; propagates errors.
      Read-only by signature — no herdr parameter, so no worktree/agent/push is
      structurally possible.
- [x] Tests (`internal/pipeline/dryrun_test.go`, package pipeline_test, reusing
      fakeGitHub/callSeq/captureLog/newTestCounter): disabled gate (zero gh
      calls), capacity full, daily reached, empty candidates, candidate
      selected (event list stops at `ListClaimCandidates` — no AddLabel, no
      charge), plus CountIssuesByLabel and ListClaimCandidates error
      propagation. TDD: RED = `undefined: pipeline.DryRun` build failure;
      GREEN = 44 passed in the package.
- [x] Work-unit commit `5b5a163` — suite green (gofmt clean, build/vet OK,
      276 tests in 6 packages = 268 + 8 new, `-race` green).
- [x] RDD off (user-owned) → parent verification gate: native assess over the
      writer diff = **medium** (`executable_change` on `cmd/lights-out/main.go`),
      `review_due: false` (`under_budget`); tier medium → writer
      self-verification + parent spot check (gofmt re-run clean, 44 pipeline
      tests re-run green; full diff read back). No independent verifier (no
      small-model profile). Preflight STATUS inventory digest consumed
      read-only; its START was NOT executed (RDD off).

### T4 — exit criteria: full verification + live read-only dry-run ✅ DONE (2026-10-09)
- [x] Full suite in committed state: `gofmt -l` clean, `go build ./...` OK,
      `go vet ./...` OK, `go test ./...` → **276 passed in 6 packages**,
      `go test -race ./...` → **276 passed in 6 packages**.
- [x] Committed-state `--dry-run` (template `enabled: false`): logs
      `mode=dry-run` → `gate=disabled` → `would exit 0, no candidate`, **exit 0** —
      Phase 6 semantics live: the disabled gate now announces itself and still
      exits 0 with no dispatch.
- [x] Full-path live dry-run (read-only): local `enabled: true` flip (uncommitted,
      Phase 5 T8 precedent), binary against the target repo
      (`Zheng5005/Spotify-clone-frontend-focus-`):
      `mode=dry-run` → `gate=passed` → `capacity=ok (0/2)` → `daily=ok (0/2)`
      (stale 10-08 counter rolled to 2026-10-09 **in memory**) → `candidate=none`
      (verified live: #2 closed, #4 blocked, #3 lacks `dark-factory`) →
      `would exit 0, no candidate`, **exit 0**.
- [x] **Zero mutation proven (before/after):** labels unchanged on #4
      (`dark-factory` + `factory-blocked`) and #3 (`good first issue`) — no
      `in_progress` anywhere; daily counter content AND mtime byte-identical
      (rolled date never persisted); target clone worktree list unchanged
      (only `main`); no new PRs (only pre-existing #1, untouched).
- [x] `enabled: false` restored; `git status` clean after the check.
      Doc update committed with the evidence below.

## Open decisions

- ~~**D1 — Where does step logging live?~~ **RESOLVED (2026-10-09): `slog.Default()`,
  no injected logger.** Measured blast radius of injection: `Run`/`RunWithRunner`
  have 20 inline call sites (13 + 6 in tests, 1 in main.go) — a logger parameter
  means 3 signature changes plus ~20 mechanical edits for zero behavioral gain.
  For an application main (Phase 7 runs the binary, never reuses pipeline as a
  library), stdlib's default logger is the idiomatic seam; `main.go` already uses
  `slog.Error(...)` directly. Testability is preserved: tests silence the default
  (T1) or capture it into a buffer.
- ~~**D2 — Where does `--dry-run` live?~~ **RESOLVED (2026-10-09): separate
  `pipeline.DryRun` sharing an extracted `resolveClaim`.** Threading a mode flag
  through `run()` risks the FR4 pinned sequences; a separate function with no
  herdr parameter is structurally incapable of mutating or dispatching agents.
- ~~**D3 — Does the disabled gate stay silent?~~ **RESOLVED (2026-10-09): it logs
  one line and exits 0.** The plan mandates "log every step transition"; FR1's
  no-dispatch contract is untouched. Supersedes the T6.5 "prints nothing" note.

## Acceptance criteria

- `go run ./cmd/lights-out` (enabled) walks the full loop with every step
  transition logged on stderr.
- `go run ./cmd/lights-out --dry-run` shows exactly what it would do and makes
  zero mutating calls (verified against fakes in T3 and live, read-only, in T4).
- Existing 263 tests stay green; new tests cover every transition and every
  dry-run outcome; no exported pipeline signature changed.
- `gofmt`, `go build`, `go vet`, `go test`, `go test -race` all green.

## Progress

- ✅ T1 (`78ec9ae`) — step logging in pipeline (slog.Default; 268 tests green)
- ✅ T2 (`ec6a647`) — CLI: slog handler (text, stderr, Info) + `--dry-run` flag
- ✅ T3 (`5b5a163`) — `resolveClaim` extraction + `pipeline.DryRun` (276 tests green)
- ✅ T4 — exit criteria verified live (2026-10-09): 276 + race green; both
  dry-run paths proven against the real binary; zero mutation demonstrated

## Next step

Phase 6 is functionally complete. **Delivery decision pending (ask-on-risk):**
the running count of authored lines from the work-unit commits (~435 code + docs)
exceeds the ~400 budget, so the strategy asks once — single phase-PR like
Phase 4/5 (Phase 5 carried `size:exception`), or a chained-PR split. The user
owns the timing of the push and PR. Housekeeping (optional): stale remote branch
`factory/issue-2` on the target repo is unrelated to this phase.