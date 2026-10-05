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

- [ ] T1 Add deps: `go-github/v69 v69.2.0`, `joho/godotenv v1.5.1`
- [ ] T2 `types.go` — `Issue`, `PullRequestSpec`, `PullRequest`, `Client` interface, mappers from go-github structs
- [ ] T3 `query.go` — pure search-query builders (no I/O)
- [ ] T4 `tokens.go` — resolution order + Bearer `RoundTripper`
- [ ] T5 `client.go` — the six methods; `RemoveLabel` 404-is-success (D7)
- [ ] T6 `client_test.go` — query builder tests + `httptest`-backed client tests (no network)
- [ ] T7 Live read-only smoke test, build-tagged `live` so it never runs in CI
- [ ] T8 `.env.example` + README note on required token permissions

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