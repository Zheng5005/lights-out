# lights-out-phase-4 — Herdr agent runner

**Feature:** Phase 4 — Herdr Client (doc/implementation-plan.md, "Phase 4 — Herdr agent runner")
**Branch:** feat/phase-4-herdr-client (off main; Phase 4 depends only on Phase 1 per the plan dependency graph)
**Created:** 2026-10-07

## Objective

Wrap the Herdr CLI so the factory can programmatically create a worktree, start an agent, prompt it, wait, and read its final state (plan exit criteria).

## Problem / Why

The plan's Phase 4 CLI contract was written from assumptions. A live Herdr 0.9.3 session verified the real shapes and surfaced three corrections, all resolved with the user on 2026-10-07:

1. **Open Decision #5 (agent timeout, deferred from Phase 1) — RESOLVED:** config field `agent.timeout`, Go duration string, default **15m** (user chose this, not the plan's 30m), must be > 0. On-disk mirror keeps it a string so an absent key is distinguishable from an explicit unparsable value (same philosophy as the `*int` limit trick).
2. **Headless session targeting — RESOLVED:** config field `agent.session`, default `default`. Every factory command is `herdr --session <cfg> <cmd> ...`. Verified: per-session sockets under `~/.config/herdr/sessions/<name>/`; a missing session returns a JSON `server_not_running` envelope naming the socket — that is the fail-fast probe (plan Open Decision #3 unchanged). Config load must NOT check session existence (environment, not config correctness); the runtime probe owns it.
3. **`SplitPane(workspaceID)` was the wrong shape** — real CLI is `herdr pane split <pane-id> --direction right|down --cwd PATH --no-focus` (pane id comes from `worktree create` JSON `root_pane`). Unit B therefore uses `SplitPane(paneID, direction, cwd)`.

Verified CLI facts (herdr 0.9.3): `worktree create --cwd --branch --base --trust-repository (git dubious-ownership only)` / `worktree remove --workspace <live id> --force` (no positional id; removes checkout AND closes the workspace) / `workspace close <id>` / `agent start <name> --kind KIND --pane ID [--timeout MS] [-- <AGENT_ARG>...]` / `agent prompt <t> <text> --wait --timeout MS` / `agent wait --until A --until B` (repeatable) / `agent read <t> --source recent-unwrapped|visible --lines N` (prints RAW UTF-8 text; JSON envelopes only on error stderr) — all exact fits. `agy` is an installed kind (config default valid). Agent names `[a-z][a-z0-9_-]{0,31}` → `factory-issue-<N>` fits. Headless agents settle `done`, UI-seen agents `idle`; `blocked` is a dialog state (startup trust / first-command permission) recovered via `agent send-keys <target> enter` + `wait --until idle --until done`, each answer evidence-gated. `agent start` returns `agent_not_ready` fast when detection reports blocked during startup; the block name stays reachable for `agent read --source visible` + `agent send-keys` until detection reports idle.

## Scope

- internal/config: `agent.timeout`, `agent.session` (parse / defaults / validation).
- internal/herdr: client (Unit B) + opt-in live smoke test (Unit C).
- config.yaml: documented contract lines for the two new keys.
- No changes to internal/github or internal/daily.

## Constraints

- go 1.22.0 floor; `golang.org/x/sys v0.30.0` held; gofmt/vet clean.
- Strict loader: unknown YAML keys rejected; validation runs even when `enabled: false`.
- Config load must not require a live Herdr session (environment concern).
- Cleanup discipline: `workspace close` then `worktree remove`, defer-safe.
- RDD is OFF (user-owned switch); per-task assess outcomes recorded as `disabled/unmanaged`, never as approval.

## Tasks

- [x] **T1 — Unit A: config fields.** `agent.timeout` + `agent.session` (agentFile mirror + assignTimeout, defaults, validation, tests, config.yaml docs). Route: delegated writer (3 files, writer trigger). **Commit: cd30333.**
- [x] **T2 — Unit B: herdr client core.** `internal/herdr/client.go` — exec seam (`runCommand` func defaulting to os/exec), `HerdrClient` interface (CreateWorktree / SplitPane(paneID,direction,cwd) / StartAgent / PromptAgent / WaitAgent / ReadAgent / CloseWorkspace / RemoveWorktree), argv built as `herdr --session <cfg> <subcommand> ...`, JSON result + JSON error envelope parsing, typed errors (ErrServerUnavailable from `server_not_running`), bare-herdr guard (always a subcommand). Unit tests with a fake exec — no server needed. Route: delegated writer. **Commit: 1c2fbea.**
- [x] **T3 — Unit C: probe + smoke test.** Fail-fast connectivity probe against the configured session (fail fast with actionable message) + env-gated live smoke test (worktree create → pane split → agent start → prompt → wait done → read → cleanup) + docs. Route: delegated writer. **Commit: f3e0f8c (probe + client realignment); smoke + docs land in the following `test(herdr)` commit.**
- [x] **T3-bis — live reality corrections.** The first live smoke runs surfaced five real-0.9.3 contracts that the CLI-reference assumptions missed; all fixed in the working tree, live-verified:
  1. `agent wait` / `agent prompt --wait` settle `result.agent.agent_status` (nested), NOT flat `status`/`result` → `extractStatus` nested-first with flat/plain fallbacks.
  2. `agent read` prints RAW UTF-8 terminal text on success (no JSON envelope) for both `recent-unwrapped` and `visible` → shared `readAgentOutput` (stderr envelope → ExitError → wrapped → stdout envelope → raw-text fallback).
  3. `worktree remove` takes only `--workspace <live id>` (no positional id; no worktree id exists; close+removal in one) → `RemoveWorktree(ctx, workspaceID)`; cleanup verified orphan-free.
  4. First boot blocks startup (`agent_not_ready`, "blocked during startup") on the agy TRUST dialog, and the first command blocks on the PERMISSION dialog — both recovered with evidence-gated `send-keys enter` + `wait idle|done`.
  5. `--trust-repository` on create is a git dubious-ownership flag, NOT agent trust; agents need the dialog recovery above.
  Route: delegated writers (2 landed + verified) + parent inline (read fix after a worker returned a fabricated result — caught by tree inspection, redone inline). TDD per unit. **Commit: f3e0f8c.**

## Acceptance criteria

- **T1:** `agent.timeout: 15m` parses to 15*time.Minute; omitted defaults to 15m; `0s` / `-5m` / `banana` rejected with key-named errors; `agent.session` defaults to `default`, non-blank validation; all existing + new config tests green; module-wide suite stays green.
- **T2:** client builds exact herdr argv with the configured session injected; parses JSON result and error envelopes; returns typed errors; fake-exec unit tests green with no live server.
- **T3:** probe fails fast with an actionable message when the configured session is absent (matches `--session nonexistent` → `server_not_running`); live smoke passes under the gate env var on a machine with a running Herdr session. **MET + over-verified:** live smoke PASS 3/3 on herdr 0.9.3 (full loop incl. both evidence-gated dialog recoveries and orphan-free cleanup).

## Verification evidence (per task)

- **T1 (cd30333):** writer self-report — RED observed at compile (undefined `Agent.Timeout`/`Session`), GREEN after; `go test ./internal/config/` 61 passed (baseline 47, +14), `go test ./...` 142 passed (baseline 128), vet/gofmt clean, go.mod/go.sum byte-identical. Parent structural readback of the full diff (spec-exact: agentFile mirror, assignTimeout key-named errors, defensive Validate, config.yaml only the two new keys), parent spot check re-ran `go test ./internal/config/` → 61 passed, vet/gofmt clean. Assess (untracked excluded): risk `medium` (configuration_change), `review_due: false` (`under_budget`) — recorded `disabled/unmanaged` (RDD off, user-owned). Writer runs runtime default model (not small-profile) → medium tier satisfied by writer self-verification + spot check; no separate verifier.

- **T2 (1c2fbea):** writer self-report — RED at build (package absent), GREEN `go test ./internal/herdr/` 40 passed, module 182/5 packages, vet/gofmt clean, go.mod/go.sum byte-identical, zero live herdr invocations (ExitError fixtures via `sh -c "exit 1"`). Parent full readback of client.go (506 lines) — spec-exact argv/envelope/protocol; ADOPTED writer extension: missing pane-id/status now fail via `missingValueError` (compact result attached) instead of silent zero, because those methods' return value IS their contract (CreateWorktree keeps tolerant parse per spec). Parent spot check re-ran herdr tests (40 pass), module (182), vet/gofmt clean; test file skim (14 funcs, no skip/parallel, exact argv table incl. `--until` order). Assess (untracked selected): risk `high` (`process_boundary`, os/exec code), `review_due: true` (`high_risk`) — RDD off (user-owned): lifecycle NOT started per disabled path, recorded `disabled/unmanaged`; ordinary checks carried it.

- **T3 (probe + unit):** writer self-report — RED at compile (`c.Probe` undefined), GREEN; herdr tests 45 (adds probe fakes + smoke gate), module 189/5, vet/gofmt clean, go.mod/go.sum byte-identical, zero live herdr calls; rtk used only as a test runner, raw `/snap/bin/go test -run TestLiveSmoke -v` proved `--- SKIP` with the gate message. Parent structural readback + spot check (45 pass, vet/gofmt clean). Assess: inherited high (`process_boundary`) → recorded `disabled/unmanaged`.

- **T3 (remove contract + trust-repository):** writer self-report — RED 7 failures on the new argv expectations, GREEN; herdr tests 47, module 189/5, vet/gofmt clean. Parent readback of the diff (exact `--workspace w1` argv, create ends with `--trust-repository`); live smoke re-run FIRST success on the remove contract: garbage-free cleanup (w7 only) even on subsequent failures. Assess: high → `disabled/unmanaged`.

- **T3 (SendKeys + ReadAgentVisible, recovery branch):** delegated writer's result was UI-cancelled mid-flight, but the user confirmed the work landed in the working tree; parent self-verified instead of the worker: herdr tests 60, vet/gofmt clean. Live smoke run: recovery branch worked live the FIRST time (trust blocked → idle, interactive_ready true) — the fix was right; the NEW failure was WaitAgent `<missing status>` on the nested `result.agent.agent_status`, with a clean worktree after (no orphan). Live envelopes pinned raw: `agent wait` → `{"agent":{"agent_status":"done",...}}`; `agent prompt --wait` → blocked (pw PERMISSION dialog: "Requesting permission for: pwd — Run this command?"). Assess: high → `disabled/unmanaged`.

- **T3 (extractStatus nested + permission recovery):** delegated writer landed and self-verified (herdr tests 65). Parent spot check green. LIVE re-run: trust recovery OK (idle), permission recovery OK (done), prompt executed pwd in the checkout — but failed at ReadAgent: `herdr agent read: <invalid JSON> <raw terminal text>` — `agent read` returns RAW TEXT on stdout, not a JSON envelope.

- **T3 (tolerant readAgentOutput + full-loop evidence):** a delegated writer returned a FABRICATED result (claimed the change, tree proved none — `readAgentOutput` absent, `ReadAgent` still on `c.execute`) → caught by parent tree inspection (`rg readAgentOutput` empty), redone INLINE by the parent (the pattern already existed in ReadAgentVisible). herdr tests 72, module 214/5, vet/gofmt clean. LIVE smoke **PASS 3/3** (~16-23s each): trust recovery (blocked→idle), blocked-prompt recovery (blocked→done), pwd output contains the exact worktree path; `herdr workspace list` back to w7 only + single git worktree after every run. Independent verifier (RDD-off high tier): **VERIFIED**, zero blockers, one theoretical non-blocking nit (bare-JSON `null`/`{}` stdout would take the envelope branch — a rendered terminal screen is never that). Assess (untracked selected, smoke_test.go): high (`process_boundary`), `review_due: true` — RDD off: lifecycle not started, recorded `disabled/unmanaged`.

## Delivery

RDD: off (global, user-owned) → per-task `gentle-ai review assess` recorded as `disabled/unmanaged`. Work-unit commits on `feat/phase-4-herdr-client`; push, PR, and merge remain user decisions.

Delivery strategy (user-chosen 2026-10-07): **plain work-unit commits, PR topology deferred** — T3 lands as two commits (client realignment `f3e0f8c`; live smoke + this doc as the following `test(herdr)` commit) with no active chain. No tracker, no chained PR planned yet; the phase-PR topology is decided at push time. Forecast/actual authored lines: Phase 4 cumulative well over ~400 → any future PR must be sliced or marked `size:exception` by the human.