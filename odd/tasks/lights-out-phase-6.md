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

### T1 — step-transition logging in `internal/pipeline` (via `slog.Default()`) ⬜ PENDING
- [ ] Transitions logged at `Info` in `run()`: `gate=passed` / `gate=disabled` (FR1,
      still exit 0, still no dispatch), `capacity=ok (n/m)` / `capacity=full (n/m)`,
      `daily=ok (n/m)` / `daily=reached (n/m)`, `candidate=#N` (head of list),
      `candidate=none`.
- [ ] Transitions logged in `execute()`: `lock=applied`, `worktree=ready
      (workspace <id>)`, `agent=started`, `agent=settled <status>`, `push=ok
      (branch <b>)`, `pr=opened (#<n>)`, `cleanup=ok (worktree removed, lock released)`.
- [ ] No signature changes; each statement is pure logging next to the existing
      decisions. The FR1/FR4/FR6 test suites are not altered in their assertions.
- [ ] Tests: silence the default logger in `pipeline_test.go` (TestMain →
      `slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))`) so the
      suite output stays clean; add one capture test that points `slog.Default()`
      at a buffer and asserts a transition line (e.g. `capacity=ok (1/2)`).
- [ ] Work-unit commit: `feat(pipeline): log every step transition` — suite green
      (gofmt/build/vet/test/race).

### T2 — CLI: slog handler setup + `--dry-run` flag in `cmd/lights-out/main.go` ⬜ PENDING
- [ ] `slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, ...)))` at
      `LevelInfo`, before any work; failures stay `Error` (existing `fail()`).
- [ ] `flag.Bool("dry-run", ...)` → route to `pipeline.DryRun(ctx, cfg, gh, daily)`;
      otherwise `pipeline.Run(...)`. Exit 0 on nil from either; 1 on error.
      SIGINT/SIGTERM context wiring untouched.
- [ ] Update the file-header comment that currently says "Phase 6 replaces this
      with the full CLI" — it now *is* the CLI.
- [ ] Work-unit commit: `feat(cli): structured logging and --dry-run flag` —
      build/vet green; full suite still green.

### T3 — shared candidate selection + `pipeline.DryRun` ⬜ PENDING
- [ ] Extract `resolveClaim(ctx, cfg, gh, daily) (github.Issue, bool, error)` — the
      gate + capacity + daily + head-of-list body of `run()` (lines 68–103 today).
      `run()` calls it, then keeps its pinned sequence: `AddLabel` → `Increment` →
      `execute`. Existing exact-sequence tests must pass unmodified.
- [ ] `pipeline.DryRun(ctx, cfg, gh, daily) error`: run `resolveClaim`, log
      `mode=dry-run` and what would happen — candidate: `would claim #N (AddLabel,
      Increment, agent, push, PR skipped)`; silent exit: `would exit 0, no candidate`;
      propagate errors like `run()` does.
- [ ] Tests (same-package, fake `github.Client` like the existing suite): disabled
      gate (no gh calls at all), capacity full, daily reached, empty candidates,
      candidate selected (asserts `AddLabel` and `Increment` are **never** called
      and the would-claim line is logged), error propagation. The signature
      (no herdr) makes the "never touches herdr" constraint structural.
- [ ] Work-unit commit: `feat(pipeline): dry-run mode sharing candidate selection` —
      suite green.

### T4 — exit criteria: full verification + live read-only dry-run ⬜ PENDING
- [ ] Full suite: `gofmt -l` clean, `go build ./...`, `go vet ./...`,
      `go test ./...`, `go test -race ./...` — all green, count recorded.
- [ ] Committed state: `./lights-out --dry-run` with `enabled: false` logs
      `gate=disabled` and exits 0.
- [ ] Full-path live dry-run (read-only): flip local `enabled: true` (uncommitted,
      Phase 5 T8 precedent), run `./lights-out --dry-run` against the target repo
      (`Zheng5005/Spotify-clone-frontend-focus-`), verify the logged transitions
      (gate=passed, capacity, daily rolled to today, candidate=oldest or none) and
      **zero mutation**: no label change on the candidate, counter file untouched,
      no worktree in the clone. Restore `enabled: false`.
- [ ] Update this document to ✅ with evidence, commit
      `docs: record Phase 6 live dry-run verification`.

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

- ⬜ T1 — step logging in pipeline (slog.Default)
- ⬜ T2 — CLI: handler setup + `--dry-run` flag
- ⬜ T3 — `resolveClaim` extraction + `pipeline.DryRun`
- ⬜ T4 — exit criteria: full verification + live read-only dry-run

## Next step

Cut `feat/phase-6-cli` from `main` (`7ca6edc`) and implement T1 (step logging),
then T2 → T3 → T4 with a work-unit commit per task. Housekeeping (optional): the
stale remote branch `factory/issue-2` on the target repo is unrelated to this
phase; `config.yaml` stays `enabled: false` in the committed template, and the
live dry-run arm (T4) stays uncommitted like Phase 5 T8.