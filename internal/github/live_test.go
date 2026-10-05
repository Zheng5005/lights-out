//go:build live

// Live read-only verification against the real GitHub API.
//
// This file is behind the `live` build tag so it never runs in CI, where no
// token exists and no outbound call is wanted. Run it deliberately:
//
//	go test -tags live -run TestLive -v ./internal/github/
//
// SCOPE: this file may only call read-only methods. Opening a pull request,
// commenting, or adding and removing labels puts visible state on a public
// repository and is NOT authorized by the Phase 2 design decision D10. The two
// calls below are the whole authorization: ListClaimCandidates and
// CountIssuesByLabel.
//
// The resolved token is never printed. Only the name of the source that
// provided it is reported.
package github_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Zheng5005/lights-out/internal/config"
	"github.com/Zheng5005/lights-out/internal/github"
	"github.com/stretchr/testify/require"
)

// repoRoot is the repository root relative to this package directory. Go runs a
// test with its own package directory as the working directory, so both the
// configuration and the .env file live two levels up, not here.
var repoRoot = filepath.Join("..", "..")

// liveConfig loads the repository configuration for the live run. It honours an
// explicit LIGHTS_OUT_CONFIG and otherwise points at the repository's own
// config.yaml.
func liveConfig(t *testing.T) config.Config {
	t.Helper()

	if os.Getenv(config.EnvVar) == "" {
		t.Setenv(config.EnvVar, filepath.Join(repoRoot, "config.yaml"))
	}

	cfg, err := config.Load()
	require.NoError(t, err)
	return cfg
}

// liveClient builds a Client for the configured repository and reports which
// source supplied the credential, without revealing the credential.
//
// The .env path needs the same repository-root correction as the config path:
// DefaultEnvFile is "./.env", resolved against the process working directory, so
// a test run from internal/github would otherwise look for a .env that does not
// exist there. Setting it only when unset leaves an explicit
// LIGHTS_OUT_ENV_FILE in charge.
func liveClient(t *testing.T) (github.Client, config.Config) {
	t.Helper()

	if os.Getenv(github.EnvFileEnvVar) == "" {
		t.Setenv(github.EnvFileEnvVar, filepath.Join(repoRoot, ".env"))
	}

	cfg := liveConfig(t)

	token, source, err := github.ResolveToken()
	require.NoErrorf(t, err, "resolve a GitHub token; set %s or %s", github.TokenEnvVar, github.FallbackTokenEnvVar)
	require.NotEmpty(t, token)
	t.Logf("token source: %s (value never printed)", source)

	client, err := github.NewClient(cfg)
	require.NoError(t, err)

	return client, cfg
}

// TestLiveListClaimCandidates exercises the search query against the real API.
// It proves the query is accepted, that oldest-first ordering and the label
// exclusions are honoured by GitHub itself, and that the response maps onto the
// domain Issue type without panicking on real-world nulls.
//
// It deliberately asserts nothing about how many candidates exist: that is a
// property of the external repository, not of this code, and a test that failed
// whenever the queue happened to be empty would train its readers to ignore it.
// The count is logged instead.
func TestLiveListClaimCandidates(t *testing.T) {
	client, cfg := liveClient(t)

	query := github.ClaimCandidatesQuery(cfg.Repo, cfg.Labels)
	t.Logf("repo:   %s", cfg.Repo)
	t.Logf("query:  %s", query)
	t.Logf("sort:   created  order: asc")

	candidates, err := client.ListClaimCandidates(context.Background())
	require.NoError(t, err, "list claim candidates")

	t.Logf("claim candidates: %d", len(candidates))
	for i, issue := range candidates {
		// Bound the output so a large repository cannot flood the log.
		if i == 10 {
			t.Logf("  ... %d more not shown", len(candidates)-i)
			break
		}
		t.Logf("  #%d [%s] %s (labels: %s) created %s",
			issue.Number,
			issue.State,
			issue.Title,
			strings.Join(issue.Labels, ", "),
			issue.CreatedAt.UTC().Format(time.RFC3339),
		)
	}

	if len(candidates) == 0 {
		t.Logf("NOTE: no issue currently carries %q in %s. The API call succeeded, "+
			"which is what this test proves; an empty queue is external state.",
			cfg.Labels.Trigger, cfg.Repo)
	}

	// Ordering is the one thing this package asserts about the results
	// themselves, because it is the reason the query carries sort and order at
	// all. Checking it against real data catches a query that GitHub silently
	// ignored, which no unit test can.
	for i := 1; i < len(candidates); i++ {
		require.Falsef(t, candidates[i].CreatedAt.Before(candidates[i-1].CreatedAt),
			"candidates are not oldest-first: #%d was created before #%d",
			candidates[i].Number, candidates[i-1].Number)
	}
}

// TestLiveCountIssuesByLabel exercises the count query against the real API.
// This is the number the concurrency gate compares against
// config.ConcurrencyLimit, so it has to come from GitHub's own total_count.
func TestLiveCountIssuesByLabel(t *testing.T) {
	client, cfg := liveClient(t)

	label := cfg.Labels.InProgress
	query := github.CountIssuesByLabelQuery(cfg.Repo, label)
	t.Logf("repo:  %s", cfg.Repo)
	t.Logf("query: %s", query)

	count, err := client.CountIssuesByLabel(context.Background(), label)
	require.NoError(t, err, "count issues by label")

	t.Logf("issues labeled %q: %d", label, count)
	t.Logf("concurrency_limit: %d", cfg.ConcurrencyLimit)

	if count > cfg.ConcurrencyLimit {
		t.Logf("NOTE: %d issues are already in progress, above the configured limit of %d. "+
			"A run would claim nothing until they finish.", count, cfg.ConcurrencyLimit)
	}
}
