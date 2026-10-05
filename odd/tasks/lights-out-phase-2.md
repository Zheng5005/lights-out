# Phase 2 — GitHub Client

## Objective
A `GitHubClient` implementation that lists claim candidates, counts in-progress
issues, manages labels, opens PRs, and posts comments — plus live read-only
verification against a real repository.

## Problem
Phase 1 gave us a validated config. There is no GitHub access, so nothing
downstream (Phases 5–7) can be built or proven.

## Why this scope
`doc/implementation-plan.md` §Phases → Phase 2 defines 5 tasks and an exit
criterion: "Integration smoke test that lists issues on a test repo."

## Authorized scope

- `go.mod` / `go.sum`
- `internal/github/**`
- `cmd/lights-out/**` (only to add the smoke-test command)
- `config.yaml` (repo target — user changed it deliberately)
- `.env.example` (new)
- `.gitignore` (only if a new ignore rule is required)
- `doc/` (only to record the Phase 2 outcome)

NOT in scope: `internal/herdr`, `internal/pipeline`, `internal/daily`,
`.github/workflows`. No merge capability anywhere (FR5).

---

## Design decisions

| # | Decision | Resolution | Rationale |
|---|---|---|---|
| D1 | Which GitHub API for listing issues | **Search API** (`GET /search/issues`), not `GET /repos/{o}/{r}/issues` | Verified against GitHub REST docs. Solves three requirements in one round trip: `is:issue` excludes PRs server-side; `sort=created`+`order=asc` is the *only* way to get oldest-first (the list endpoint is newest-first with no order control, and PRD §4.4 requires the oldest issue); `-label:X` does the in-progress/blocked exclusion server-side. |
| D2 | `isPR()` filter on the interface | **Rejected.** `is:issue` in the query does it server-side. | A filter is implementation, not contract. Putting it on `GitHubClient` leaks HTTP response shape into Phase 5's contract and gives the pipeline a second thing to forget. |
| D3 | Token auth library | **No `golang.org/x/oauth2`.** Custom `http.RoundTripper` injecting `Authorization: Bearer <token>`. | oauth2 only earns its keep for the 3-legged browser flow, which a cron factory never runs. Dropping it removes the version ceiling (see D4) and keeps auth code dependency-free. |
| D4 | Dependency versions | `go-github/v69 v69.2.0`, `joho/godotenv v1.5.1` | Module pins `go 1.22` (Phase 1 decision: self-hosted runner compatibility). Go refuses deps with a higher `go` directive. v69.2.0 is the last go-github release at `go 1.22.0`; v70+ need `go 1.23.0`. godotenv declares `go 1.12`. |
| D5 | `.env` support | godotenv `v1.5.1` (user chose Option B) | `godotenv.Load()` never overwrites already-set variables, so real env beats `.env` automatically. Correct precedence by construction. |
| D6 | Token precedence | `LIGHTS_OUT_GITHUB_TOKEN` → `GITHUB_TOKEN` → `.env` file | Explicit override first. A stray `.env` on a self-hosted runner must never shadow the Actions-provided `GITHUB_TOKEN`. |
| D7 | `RemoveLabel` on a label the issue lacks | **Treat HTTP 404 as success** (idempotent) | PRD FR4 requires the lock be removed on *every* exit path including crash recovery. A non-idempotent removal turns a re-run into a permanent blocked issue. |
| D8 | Interface shape | Intention-revealing, not CRUD-generic (below) | The claim-candidate query is the one place three labels interact. Encoding that in the method name makes Phase 5 read correctly and prevents the pipeline from re-implementing the exclusion. |
| D9 | Reusable interface mock | **Deferred to Phase 5** | Phase 2 has no consumer of the interface, so a shared mock would be dead code that rots. Phase 2 proves mockability with a local test fake. |
| D10 | Live verification scope | **Read-only** (user authorized) | Exercises `ListClaimCandidates` + `CountInProgress` only. Proves token, query, and mapping without putting visible state on a public repo. |

---

## Contract

```go
// Issue is a domain issue. Pull requests are never surfaced as Issues.
type Issue struct {
    Number    int
    Title     string
    Body      string
    State     string
    Labels    []string
    URL       string
    CreatedAt time.Time
}

// PullRequestSpec is the request to open a pull request.
type PullRequestSpec struct {
    Head, Base, Title, Body string
}

// PullRequest is an opened pull request.
type PullRequest struct {
    Number      int
    URL         string
    Head, Base  string
    Title, Body string
    State       string
}

// Client is the GitHub surface the factory depends on.
type Client interface {
    // ListClaimCandidates returns open issues labeled labels.trigger that do
    // not carry labels.in_progress or labels.blocked, oldest first. Pull
    // requests are excluded server-side and must never appear here.
    ListClaimCandidates(ctx context.Context) ([]Issue, error)

    CountIssuesByLabel(ctx context.Context, label string) (int, error)
    AddLabel(ctx context.Context, issueNum int, label string) error
    RemoveLabel(ctx context.Context, issueNum int, label string) error
    CreatePR(ctx context.Context, spec PullRequestSpec) (*PullRequest, error)
    CommentOnIssue(ctx context.Context, issueNum int, body string) error
}
```

**There is deliberately no merge method.** FR5 ("no code path may merge a PR")
is then structurally impossible rather than merely forbidden.

### Search queries

```
ListClaimCandidates →
  repo:<owner>/<repo> is:issue is:open label:"<trigger>"
    -label:"<in_progress>" -label:"<blocked>"
  sort=created order=asc

CountIssuesByLabel →
  repo:<owner>/<repo> is:issue is:open label:"<label>"
```

Label values are quoted and `"` / `\` inside a label name are escaped.

### Token resolution

1. `LIGHTS_OUT_GITHUB_TOKEN`
2. `GITHUB_TOKEN`
3. `.env` file (path overridable via `LIGHTS_OUT_ENV_FILE`, default `.env`)

Missing token is a clear error naming all three sources. **The token value must
never appear in any log, error, or test failure output.**

---

## Tasks

- [x] T1 Add deps: `go-github/v69 v69.2.0`, `joho/godotenv v1.5.1`
- [x] T2 `types.go` — `Issue`, `PullRequestSpec`, `PullRequest`, `Client` interface, mappers from go-github structs
- [x] T3 `query.go` — pure search-query builders (no I/O)
- [x] T4 `tokens.go` — resolution order + Bearer `RoundTripper`
- [x] T5 `client.go` — the six methods; `RemoveLabel` 404-is-success (D7)
- [x] T6 tests — query builder tests + `httptest`-backed client tests (no network)
- [x] T7 Live read-only smoke test, build-tagged `live` so it never runs in CI
- [x] T8 `.env.example` + required token permissions documented

## Progress log

### T1–T8 complete (writer: `complete`, parent spot check: clean)

Verification evidence (parent re-ran, real output):

```
go build ./...                                    -> exit=0
go vet ./...                                      -> exit=0
go test -count=1 ./...                            -> 111 passed, 3 packages
go test -race ./...                               -> clean
go test -tags live -run TestLive ./internal/github/ -> 2 passed (read-only)
```

Live verification (read-only, authorized): **1 claim candidate** — issue `#2`
"Testing Lights out", labels `dark-factory`, created `2026-10-04T00:00:01Z`.
In-progress count: **1** (`concurrency_limit: 2`). Token resolved from the `.env`
file; the value was never printed. No mutating method was called.

Parent spot checks beyond the suite:

```
merge-capability audit   -> only the test asserting its absence; no Merge method
token-leak audit          -> only env var NAMES; no values, in code or fixtures
.env staged?              -> no
.env.example values       -> placeholders only, no token-shaped string
.env tracked by git?      -> no (matched by .gitignore:11)
```

### Real bug caught by TDD

The writer implemented `RemoveLabel` naively first and the D7 test failed at
runtime with a genuine GitHub 404 (`Label does not exist`). Fixed to treat 404 as
success. This is exactly the FR4 failure mode the design predicted.

### Deviations from this document

| # | Deviation | Reason |
|---|---|---|
| 1 | `go get` rewrote `go 1.22` → `go 1.22.0` | Canonicalization by Go, not a bump — go-github v69.2.0 declares `go 1.22.0`. The ≥1.23 ceiling (D4) was never crossed. |
| 2 | `go get` alone was insufficient | It writes only the direct module; `go-querystring` needs a separate `go mod tidy`. |
| 3 | `.gitignore` gained `!.env.example` | `.gitignore:12` `.env.*` silently hid `.env.example`, making T8 impossible. |
| 4 | `godotenv.Read` instead of `godotenv.Load` | `Read` returns a map rather than mutating the process env; otherwise resolving a token would pollute the whole test binary's environment. D5 precedence is unchanged. |
| 5 | Query tests live in `query_test.go`, not `client_test.go` | Co-location with `query.go`; T6's file split was a suggestion. |
| 6 | Live test asserts ordering, not candidate count | Count is external repo state — asserting ≥1 would fail whenever the queue empties and train readers to ignore failures. Criterion 7 is satisfied in fact (1 candidate observed), not by assertion. |
| 7 | `cmd/lights-out/main.go` untouched | No task required a smoke-test command; the `live` test already satisfies the exit criterion. |
| 8 | Exported `WithHTTPClient` added then removed | Untested and unused; D9's aversion to speculative surface applies. `WithBaseURL` stays (tests use it). |

### Carried into Phase 6/7 — action required

- **`.env` resolution is CWD-relative.** A scheduler or GitHub Action running
  from any directory other than the repo root will silently fail to find the
  token. Phase 6/7 must either run from the repo root or set an absolute
  `LIGHTS_OUT_ENV_FILE`. The live test failed once for exactly this reason.
- **`RemoveLabelForIssue` does not URL-escape the label** (go-github behavior).
  A configured label containing a space or slash would target the wrong URL on
  that one call. Shipped labels are safe; documented rather than papered over.
- **PR body / `Closes #<N>` is not Phase 2's job** (plan task 4 vs. the
  interface signature — see "Explicitly deferred"). Phase 5 composes it.

## Git state

```
main                = 9917ec5
feat/phase-2-github-client = babac01  (this work, unmerged)
```

Base ref (branch point): `9917ec5`. Authored lines: 2151 — over the ~400
planning heuristic, driven by 1290 lines of tests. Not cut, per the
no-shrinking policy. `review_due: true`, `review_due_reason: slice_budget_reached`,
risk `medium`.

> Correction to the Phase 1 doc: its review table lists base ref `9c77147`. That
> was correct when written, but `9917ec5` landed afterwards and is the real
> branch point. Assessing against `9c77147` wrongly pulled the Phase 1 doc
> commit into this candidate's scope.

## Review transaction — open, blocked on consent

| Field | Value |
|---|---|
| Lineage | `review-a19ad28b3f74a468` |
| Target | `sha256:70c1cb5aeaebf0652d34591341553f992bc28891b44e1aba26f24994bca6f5d4` |
| Base ref | `9917ec5` |
| Risk | medium, 14 files, 2151 lines |
| Outcome | **No review ran.** START returned `consent_required` (`blocking: true`). |

This runtime exposes no `question` tool, so the v3 consent envelope cannot be
presented natively, and the contract forbids substituting chat text as consent.
Authority is frozen and unburned; the transaction stays open.

## Next step

Human decision on consent, then Phase 3 (Daily Counter) — which is independent of
Phase 2 and can be started in either order.

## Explicitly deferred

- **`-linked:pr` exclusion.** Whether the factory should skip an issue that
  already has an open PR is a *business rule*, not a fetch concern — it is
  distinct from the PR-returns-from-issues-endpoint problem D1 solves. PRD §9
  keeps unstick handling manual in v1 and §11 lists auto-unstick as v2. Raising
  the `go` directive to ≥1.23 for a newer `go-github` is likewise a Phase 4/7
  decision, not Phase 2's.

## Acceptance criteria

1. `go build ./...` succeeds.
2. `go vet ./...` is clean.
3. `go test ./...` passes with no network access.
4. Query builders produce the exact strings in this document (unit-tested).
5. `RemoveLabel` succeeds when the label is already absent.
6. No merge capability exists anywhere in `internal/github`.
7. `go test -tags live ./internal/github/ -run TestLive` against the real repo
   returns ≥1 claim candidate and a plausible in-progress count.

## Verification commands

```
go build ./...
go vet ./...
go test -count=1 ./...
go test -tags live -run TestLive -v ./internal/github/
```

## Route
T2–T8 delegated to one writer (6+ non-trivial files → writer trigger fired).

## Git state at start

`main` = `9c77147`. Phase 1 shipped **unreviewed** (consent envelope could not
be presented in this runtime). Do not assume a passing receipt on this codebase.