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

Verified CLI facts (herdr 0.9.3): `worktree create --cwd --branch --base` / `worktree remove <id>` / `workspace close <id>` / `agent start <name> --kind KIND --pane ID` / `agent prompt <t> <text> --wait --timeout MS` / `agent wait --until A --until B` (repeatable) / `agent read --source recent-unwrapped` — all exact fits. `agy` is an installed kind (config default valid). Agent names `[a-z][a-z0-9_-]{0,31}` → `factory-issue-<N>` fits. Headless agents report `done`, never `idle` (needs focused-UI "seen") — wait set is {done, blocked}. `--trust-repository` flag exists on worktree create, likely needed headless.

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
- [ ] **T2 — Unit B: herdr client core.** `internal/herdr/client.go` — exec seam (`runCommand` func defaulting to os/exec), `HerdrClient` interface (CreateWorktree / SplitPane(paneID,direction,cwd) / StartAgent / PromptAgent / WaitAgent / ReadAgent / CloseWorkspace / RemoveWorktree), argv built as `herdr --session <cfg> <subcommand> ...`, JSON result + JSON error envelope parsing, typed errors (ErrServerUnavailable from `server_not_running`), bare-herdr guard (always a subcommand). Unit tests with a fake exec — no server needed. Route: delegated writer.
- [ ] **T3 — Unit C: probe + smoke test.** Fail-fast connectivity probe against the configured session (fail fast with actionable message) + env-gated live smoke test (worktree create → pane split → agent start → prompt → wait done → read → cleanup) + docs. Route: delegated writer.

## Acceptance criteria

- **T1:** `agent.timeout: 15m` parses to 15*time.Minute; omitted defaults to 15m; `0s` / `-5m` / `banana` rejected with key-named errors; `agent.session` defaults to `default`, non-blank validation; all existing + new config tests green; module-wide suite stays green.
- **T2:** client builds exact herdr argv with the configured session injected; parses JSON result and error envelopes; returns typed errors; fake-exec unit tests green with no live server.
- **T3:** probe fails fast with an actionable message when the configured session is absent (matches `--session nonexistent` → `server_not_running`); live smoke passes under the gate env var on a machine with a running Herdr session.

## Verification evidence (per task)

- **T1 (cd30333):** writer self-report — RED observed at compile (undefined `Agent.Timeout`/`Session`), GREEN after; `go test ./internal/config/` 61 passed (baseline 47, +14), `go test ./...` 142 passed (baseline 128), vet/gofmt clean, go.mod/go.sum byte-identical. Parent structural readback of the full diff (spec-exact: agentFile mirror, assignTimeout key-named errors, defensive Validate, config.yaml only the two new keys), parent spot check re-ran `go test ./internal/config/` → 61 passed, vet/gofmt clean. Assess (untracked excluded): risk `medium` (configuration_change), `review_due: false` (`under_budget`) — recorded `disabled/unmanaged` (RDD off, user-owned). Writer runs runtime default model (not small-profile) → medium tier satisfied by writer self-verification + spot check; no separate verifier.

## Delivery

RDD: off (global, user-owned) → per-task `gentle-ai review assess` recorded as `disabled/unmanaged`. Work-unit commits on `feat/phase-4-herdr-client`; push, PR, and merge remain user decisions.