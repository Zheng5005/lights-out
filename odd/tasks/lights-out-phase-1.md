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

### Blocker: no git repository

`gentle-ai review status` returns `applicability: "unrelated"`, `paths: []`, and
`base_tree: 4b825dc...` (git's empty-tree hash). Cause: the project has no git
repo, so all 9 files are untracked and there is no tracked diff to review. RDD is
enabled (global), and ODD closes each task with a work-unit commit — both are
blocked until the user decides on git initialization.

## Next step
Await user decision on git init, then either commit Phase 1 as a work unit and
run the native review, or declare Phase 1 delivered outside git.