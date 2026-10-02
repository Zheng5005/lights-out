# Phase 1 — Scaffolding & Config

## Objective
Runnable `lights-out` binary that loads, validates and prints its configuration.

## Problem
No module, no configuration contract, no proof of life. Phases 2–7 all depend on
the config struct and the load/validate path being correct and stable.

## Why this scope
`doc/implementation-plan.md` §Phases → Phase 1 defines 5 tasks and an exit
criterion: `./lights-out` prints the parsed config and exits 0.

## Authorized scope
- `go.mod` / `go.sum`
- `internal/config/**`
- `cmd/lights-out/**`
- `config.yaml` (example/default)

Nothing else. Phases 2–8 (github, herdr, pipeline, daily, workflow) are NOT in scope.

## Design decisions taken (from plan's Open Decisions table)

| # | Decision | Resolution for Phase 1 | Rationale |
|---|---|---|---|
| 1 | Agent kind | Config field `agent.kind` | Plan recommends config field; already in Phase 1 struct |
| 4 | Worktree base ref | Config field, defaults to `main` | Plan marks impact as "Phase 1 config" |
| 5 | Agent timeout | DEFERRED to Phase 4 | Plan marks impact as "Phase 4" |

Additional judgment calls (plan did not specify):

- **Validation is unconditional.** Validation runs at load time regardless of
  `enabled`. The gate check (`if !cfg.Enabled { return }`) is a Phase 5 run-time
  concern. Consequence: a disabled factory still requires a fully valid config.
- **Defaults are applied before validation.** Missing `concurrency_limit`,
  `daily_limit`, label names, `agent.dispatcher`, `agent.mode`, `agent.kind`,
  `worktree.base` get defaults; then validation runs on the resolved struct.
- **`disabled` is a valid, loadable configuration.** It is not an error.
- `go` directive pinned to `1.22` (plan floor) instead of the local toolchain version,
  so a self-hosted runner on an older patch release can build it.

## Tasks

- [x] T1 `go mod init github.com/Zheng5005/lights-out` → `go.mod` created, `go` directive set to 1.22
- [x] T2 `internal/config/config.go` — `Config`, `Labels`, `Agent`, `Worktree` types; `Load`, `Parse`, `ResolveDefaults`, `Validate`, `ResolvePath`
- [x] T3 `internal/config/config_test.go` — 47 cases, RED observed before T2 implementation
- [x] T4 `config.yaml` — commented example config
- [x] T5 `cmd/lights-out/main.go` — load, print, exit 0

## Acceptance criteria

1. `go build ./...` succeeds.
2. `go vet ./...` is clean.
3. `go test ./...` passes.
4. `./lights-out` with a valid config prints the parsed config and exits 0.
5. `./lights-out` with a missing file, bad `repo`, zero limit, or empty label exits non-zero with a clear error.

## Verification commands
```
go build ./...
go vet ./...
go test ./...
go run ./cmd/lights-out
```

## Route
T2–T5 delegated to one writer (4 non-trivial files → writer trigger fired).

## Progress log

### T1–T5 complete (writer: `complete`, parent spot check: clean)

Verification evidence (parent re-ran, real output):
```
go build ./...                 -> exit=0
go vet ./...                   -> exit=0
go test -count=1 ./...         -> 47 passed, 2 packages, exit=0
go run ./cmd/lights-out        -> exit=0, prints parsed config
missing config file            -> exit=1
concurrency_limit: 0 (explicit)-> exit=1, "must be greater than 0, got 0"
```

Real bug found and fixed during T3 (second RED/GREEN cycle): an explicit
`concurrency_limit: 0` was indistinguishable from an absent key with plain `int`
YAML fields, so `ResolveDefaults` promoted a deliberate zero to the default and
the binary exited 0. Fixed by decoding into an unexported `configFile` mirror
with `*int` limits. The public struct shape from the plan is unchanged.

Known test coupling (accepted for now): `TestShippedConfigExampleIsValid` reads
`../../config.yaml`, and the default-path test uses `os.Chdir` (`t.Chdir` needs
Go 1.24, module pins 1.22). Both must stay non-parallel.

## Git state

```
a7848e8  chore: bootstrap repository with design docs and ignore rules   (main)
1bc171d  feat(config): load, validate and prove out the factory configuration  (feat/phase-1-config)
```

Authored lines in `1bc171d`: 904 (over the ~400 planning heuristic; driven by the
417-line test suite and this tracking doc — not cut, per the no-shrinking policy).
`review_due: true`, `review_due_reason: slice_budget_reached`.

## Review transaction (in flight, NOT acknowledged)

| Field | Value |
|---|---|
| Lineage | `review-7fcbda282eefd06f` |
| Target | `sha256:1cdbf16756d24d040f4fa189155f37c111f05c0dcb879768cbe0732e7cd63ed5` |
| Base ref | `ce73965b6a6e358147a43b5478e21060942cfab5` |
| Risk | medium, 7 files, 904 lines |
| State | `action: consent_required`, `blocking: true` |

START was executed verbatim and returned the `gentle-ai.review-integration.consent/v3`
envelope. The envelope is awaiting a human decision.

**BLOCKED:** this runtime exposes no `question` tool, so the classified native
consent UI is unavailable. The v3 contract forbids using chat text as consent and
forbids a chat-token fallback. No provider continuation may be invoked from chat.

Both provider-owned invocations are preserved verbatim in
`/tmp/opencode/review-start.json`. To unblock, run one of them directly in a
terminal (this is a human action, not an agent action):

- Grant: the invocation under `choices[answer="granted"].invocation`
- Decline: the invocation under `choices[answer="declined"].invocation`

Then re-query with the exact bound STATUS (lineage + target tokens above) and
follow its returned `next_transition`.

## Next step
Human decides the review consent. Phase 1 code itself is complete and verified;
delivery remains a separate decision under ordinary repository policy.