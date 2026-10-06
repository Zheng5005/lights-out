# Phase 3 — Daily Counter

## Objective
Track how many issues have been claimed per UTC day, safely across processes.

## Problem
The factory must honor `daily_limit` (already validated > 0 by Phase 1). Without a
persisted counter the limit cannot be enforced, and without concurrency control two
factory runs would both read the same count, both increment, and one claim would be
lost — silently over-running the cap.

## Why this scope
`doc/implementation-plan.md` §Phase 3 defines four tasks and an exit criterion:
"Unit tests covering day rollover and concurrent-safe file access." Phase 3 depends
only on Phase 1 (`P1 → P3`); it needs no network, no `GITHUB_TOKEN`, and no Herdr.

## Authorized scope
- `internal/daily/**` (new)
- `go.mod` / `go.sum`

Nothing else. No changes to `internal/config`, `internal/github`, `cmd/`, or `doc/`.

## Design decisions (confirmed with the user)

| # | Decision | Resolution | Rationale |
|---|---|---|---|
| 1 | Clock seam | `now func() time.Time` field, defaults to `time.Now` | User chose "better safe than sorry"; without it the UTC rollover wiring is untestable |
| 2 | Locking | `flock` via `golang.org/x/sys/unix`, **not** `syscall` | User chose `x/sys/unix`; `syscall` is frozen and soft-deprecated |
| 3 | Lock kind | `LOCK_EX` in `Increment`, `LOCK_SH` in `Current` | Read/write distinction; single-file flock cannot deadlock |
| 4 | Corruption | temp file + `os.Rename` **while holding the lock** | flock prevents lost updates; atomic rename prevents torn writes if the process is killed mid-write |
| 5 | Env var | `LIGHTS_OUT_DAILY_CLAIMS` | Follows the established `LIGHTS_OUT_*` convention from Phases 1–2 |
| 6 | Date comparison | **string equality** against `time.DateOnly` in UTC | A malformed stored date can never equal today, so rollover happens for free — no date parsing, no parse errors |
| 7 | `x/sys` version | pinned **v0.30.0** | v0.31.0+ declares `go 1.23`; v0.48.0 declares `go 1.26`. v0.30.0 is the newest that preserves the plan's Go 1.22 floor |

### The `x/sys` pin is load-bearing

`go get golang.org/x/sys@latest` silently upgraded `go.mod` from `1.22.0` to
`1.26.0`. That was reverted. Measured ceilings:

| x/sys | declares |
|---|---|
| v0.30.0 | `go 1.18` ✅ |
| v0.31.0–v0.34.0 | `go 1.23` ❌ |
| v0.35.0 | `go 1.23` ❌ |
| v0.40.0 | `go 1.24` ❌ |
| v0.48.0 | `go 1.26` ❌ |

Do not run `go get -u golang.org/x/sys` in this repo without re-checking the `go`
directive.

## Contract

```go
// Path resolves the counter file: $LIGHTS_OUT_DAILY_CLAIMS, else
// $HOME/.lights-out/daily-claims.json.
func Path() (string, error)

// New returns a Counter writing to path, using time.Now as its clock.
func New(path string) *Counter

// NewWithClock is the test seam. now must never be nil in production callers.
func NewWithClock(path string, now func() time.Time) *Counter

// Current reads the count for the current UTC day without mutating.
func (c *Counter) Current() (int, error)

// Increment bumps the count for the current UTC day, resetting to 0 when the
// UTC date differs from the stored date, and returns the new count.
func (c *Counter) Increment() (int, error)
```

Storage shape, exactly as specified by the plan:

```json
{"date": "2026-10-02", "count": 1}
```

### File-state semantics

| State of file | Behavior |
|---|---|
| absent | count `0`, no error (first run) |
| present, 0 bytes | count `0`, no error |
| present, valid JSON, `date` != today | reset to `0` (rollover) |
| present, valid JSON, `date` == today | `count + 1` |
| present, valid JSON, `date` missing/empty/garbage | never equals today → resets (safe) |
| present, non-empty, **invalid JSON** | **error**, wrapped with the path — fail loud |

Corrupt JSON is an error, not a silent reset: flock + atomic rename should make it
impossible, so a corrupt file means a real bug and over-claiming is the dangerous
direction for a limit.

### Critical implementation requirement: open the file per call

`flock` locks attach to the **open file description**, not the process. If `Counter`
holds one `*os.File` open for its lifetime, concurrent callers share that description
and `flock` becomes a no-op — the concurrency test would pass **vacuously** while
providing zero protection.

Each `Current`/`Increment` must open the file fresh (its own `Open`, its own fd),
`flock` it, do its work, `FUNLCK`, close. This is what makes the lock real.

## Tasks

- [x] T1 `internal/daily/counter.go` — `Path`, `New`, `NewWithClock`, `Current`, `Increment`, locking, atomic write (209 lines)
- [x] T2 `internal/daily/counter_test.go` — written FIRST (RED before T1) (335 lines, 12 test funcs / 17 cases)
- [x] T3 gofmt / vet / test green, parent spot check
- [x] T4 work-unit commit on a feature branch, then review assess

## Required test coverage

Must include at least:

1. First run, file absent → `0`, no error.
2. Same UTC day → increments monotonically.
3. **Rollover** across UTC midnight with a fake clock → resets to `0`.
4. **UTC correctness** — the test that catches a missing `.UTC()`:
   fake clock at `2026-10-07T00:30:00+02:00` resolves to UTC date `2026-10-06`.
   Assert the stored date string, not just the count.
5. Non-UTC time zone does not shift the boundary.
6. Invalid JSON → error; assert the path appears in the message.
7. 0-byte file → `0`, no error.
8. Env var override via `t.Setenv` (implies **not** `t.Parallel`).
9. Default path when the env var is unset.
10. **Concurrency:** N goroutines each calling `Increment` → final count == N.
    This only passes if each call opens its own fd.
11. `Current` does not mutate the file (byte-identical before/after).

## Acceptance criteria

1. `go build ./...`, `go vet ./...`, `gofmt -l .` all clean.
2. `go test -count=1 ./...` passes.
3. `go.mod` still declares `go 1.22.0` and `golang.org/x/sys v0.30.0`.
4. No network access and no `GITHUB_TOKEN` needed by any test.
5. Every test that touches `t.Setenv` is non-parallel.

## Verification commands
```
go build ./...
go vet ./...
gofmt -l .
go test -count=1 ./...
grep -E '^go |golang.org/x/sys' go.mod
```

## Known environmental failures
None.

## Route
T1–T2 delegated to one writer (2 non-trivial files → writer trigger fired).
Skills: `/home/zheng005/.agents/skills/go-testing/SKILL.md`.

## Delivery
Branch from current `main`. One work-unit commit. Authored-line budget is a
planning heuristic (~400); do not cut tests, comments, or blank lines to fit it —
report the overage instead.
## Progress log

- `x/sys` pinned to v0.30.0, `go` directive restored to 1.22.0 after `go get`
  silently bumped it to 1.26.0.
- Baseline verified green before any source write: 111 tests / 3 packages.

### T1–T3 complete (writer: `complete`, parent spot check: clean)

Parent re-ran independently:
```
go build ./...        -> OK
go vet ./...          -> OK
gofmt -l .            -> empty
go test -count=1 ./...-> 128 passed, 4 packages
go.mod                -> go 1.22.0, golang.org/x/sys v0.30.0 (direct)
```
Added: 17 tests / 12 functions covering all 11 required cases plus the
malformed-stored-date rollover case.

### Design finding the plan missed — flock vs os.Rename

`flock` binds to the **inode**. Taking the lock on the counter file and then
`os.Rename`-ing a temp file over it repoints the path at a NEW inode while the
lock still guards the old one. The next caller locks the new (uncontended) inode
and reads stale data — a lost update where every caller believes it held the lock.

Fix: lock a **sidecar** `<path>.lock` whose inode never changes; rename only the
data file. Both traps are documented at `withLock` in `counter.go`.

Two traps were therefore load-bearing and are both present:
1. open a fresh fd per call (flock binds to the file *description*);
2. lock the sidecar, never the renamed file.

### Parent verification beyond the test suite

| Check | Method | Result |
|---|---|---|
| Does the UTC seam actually matter? | Mutation: removed `.UTC()` from `today()` | **2 tests failed** (`2026-10-06` expected, `2026-10-07` actual). Restored, green. |
| First run on a missing directory | Live probe, `Increment()` into 4-level missing path | `(1, nil)`, file created |
| `Current` stays read-only | Live probe asserting no directory side effect | `(0, nil)`, **no directories created** |
| `t.Setenv` vs `t.Parallel` clash | Regex scan — **false positive**: matched the phrase inside `// No t.Parallel` comments | No `t.Parallel()` anywhere in the file; constraint satisfied |
| `// indirect` mislabel after adding the import | `go mod tidy` | Relabelled to direct, `go 1.22.0` **held** |

Probe files were created, run, and deleted; they are not part of the commit.

## Next step
Work-unit commit on `feat/phase-3-daily-counter`, then `review assess` against
`main`. Push remains a human decision.

## Review outcome

`review assess --base-ref 7525af6 --committed-only` returned:

| Field | Value |
|---|---|
| risk | `medium` |
| review_due | `true` |
| review_due_reason | `slice_budget_reached` |
| changed_lines | 751 |

The preflight STATUS then returned `kind: stop`, `reason_code: rdd_disabled`.
`gentle-ai review mode status` confirms receipt-driven development is
`off (decided by global)` — it was `on` during Phase 1, so the switch changed
between Phase 2 and Phase 3.

Per the switch contract the mode was **not** reactivated: it is user-owned and
may only be turned on explicitly by the human. Phase 3 therefore closes under
ordinary repository policy. Assessment tier recorded as **`disabled/unmanaged`**,
not as an approval — a disabled review gate is never a passing review.

Baseline before this phase: `7525af6`. Authored lines: 751 (over the ~400
planning heuristic, driven by 335 lines of tests and 204 lines of this document;
not cut).
