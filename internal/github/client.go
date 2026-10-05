package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/Zheng5005/lights-out/internal/config"
	gh "github.com/google/go-github/v69/github"
)

const (
	// maxPerPage is the largest page the search API accepts; taking the maximum
	// keeps the number of round trips a claim run makes small.
	maxPerPage = 100

	// searchSortCreated with searchOrderAsc is the only way to list issues
	// oldest first: the list-issues endpoint is newest first and offers no
	// order control at all (design decision D1).
	searchSortCreated = "created"
	searchOrderAsc    = "asc"
)

// options collects construction-time settings. It is unexported so the Option
// surface cannot grow fields callers are expected to understand.
type options struct {
	baseURL string
}

// Option customises a Client at construction time.
type Option func(*options)

// WithBaseURL points the Client at a different GitHub API root. It exists so
// tests can serve the real wire format from an httptest server; production
// callers leave it unset and get https://api.github.com/.
//
// It is deliberately the only Option. An HTTP-client override was considered and
// dropped: nothing in Phase 2 needs it, and an unused constructor hook is a
// surface with no test and no user.
func WithBaseURL(rawURL string) Option {
	return func(o *options) { o.baseURL = rawURL }
}

// apiClient is the only implementation of Client. It is unexported so the
// contract cannot be widened by accident: adding a method to the concrete type
// is invisible to consumers, while adding one to Client breaks them loudly.
type apiClient struct {
	owner  string
	repo   string
	labels config.Labels
	api    *gh.Client
}

// Compile-time proof that the implementation covers the whole contract, and
// that the contract has no merge method to satisfy.
var _ Client = (*apiClient)(nil)

// NewClient builds a Client for cfg.
//
// Authentication is resolved eagerly: a missing or malformed token fails here
// rather than on the first HTTP call, which in a cron factory would surface
// minutes later with no context. The repo is validated for the same reason.
//
// Token and base-URL resolution both belong here so no other phase has to
// remember them, and so a test can point the same Client at a local server.
func NewClient(cfg config.Config, opts ...Option) (Client, error) {
	owner, name, err := splitRepo(cfg.Repo)
	if err != nil {
		return nil, err
	}

	var settings options
	for _, opt := range opts {
		opt(&settings)
	}

	token, _, err := ResolveToken()
	if err != nil {
		return nil, err
	}

	api := gh.NewClient(NewTokenHTTPClient(token))
	if settings.baseURL != "" {
		// go-github resolves every request against BaseURL and requires a
		// trailing slash; without it the last path segment is dropped.
		base, err := url.Parse(strings.TrimSuffix(settings.baseURL, "/") + "/")
		if err != nil {
			return nil, fmt.Errorf("parse base url %q: %w", settings.baseURL, err)
		}
		api.BaseURL = base
	}

	return &apiClient{owner: owner, repo: name, labels: cfg.Labels, api: api}, nil
}

// splitRepo splits the validated "owner/repo" form. config.Validate already
// enforces the shape, and this repeats the check so a Client built from a
// hand-written Config struct fails at construction instead of issuing requests
// against a nonsensical path.
func splitRepo(repo string) (owner, name string, err error) {
	owner, name, found := strings.Cut(repo, "/")
	if !found || strings.Contains(name, "/") ||
		strings.TrimSpace(owner) == "" || strings.TrimSpace(name) == "" {
		return "", "", fmt.Errorf("repo must be in %q form, got %q", "owner/repo", repo)
	}
	return owner, name, nil
}

// slug is the "owner/repo" form the search query needs.
func (c *apiClient) slug() string {
	return c.owner + "/" + c.repo
}

// ListClaimCandidates returns the open issues the factory may claim, oldest
// first.
//
// The selection is expressed entirely in the query: the trigger label, the
// in-progress and blocked exclusions, and the is:issue qualifier that keeps pull
// requests out. Nothing is filtered afterwards, so a bug here cannot accidentally
// hand the pipeline an issue it is forbidden to touch.
//
// Every page is followed. Returning only the first page would quietly cap the
// factory at one candidate no matter how high the concurrency limit is.
func (c *apiClient) ListClaimCandidates(ctx context.Context) ([]Issue, error) {
	query := ClaimCandidatesQuery(c.slug(), c.labels)
	search := &gh.SearchOptions{
		Sort:  searchSortCreated,
		Order: searchOrderAsc,
		ListOptions: gh.ListOptions{
			PerPage: maxPerPage,
		},
	}

	var issues []Issue
	for {
		result, resp, err := c.api.Search.Issues(ctx, query, search)
		if err != nil {
			return nil, fmt.Errorf("list claim candidates: %w", err)
		}
		issues = append(issues, issuesFrom(result.Issues)...)

		if resp == nil || resp.NextPage == 0 {
			return issues, nil
		}
		search.Page = resp.NextPage
	}
}

// CountIssuesByLabel counts open issues carrying label.
//
// It shares the query shape with ListClaimCandidates so the count and the
// candidate list can never disagree about what an issue is. Only total_count is
// needed, so a single item is requested.
func (c *apiClient) CountIssuesByLabel(ctx context.Context, label string) (int, error) {
	result, _, err := c.api.Search.Issues(
		ctx,
		CountIssuesByLabelQuery(c.slug(), label),
		&gh.SearchOptions{ListOptions: gh.ListOptions{PerPage: 1}},
	)
	if err != nil {
		return 0, fmt.Errorf("count issues labeled %q: %w", label, err)
	}
	if result.Total == nil {
		// Reporting 0 here would unlock the concurrency gate on a malformed
		// response, so an absent total is an error rather than a count.
		return 0, fmt.Errorf("count issues labeled %q: response carried no total_count", label)
	}
	return *result.Total, nil
}

// AddLabel puts label on the issue.
func (c *apiClient) AddLabel(ctx context.Context, issueNum int, label string) error {
	if _, _, err := c.api.Issues.AddLabelsToIssue(
		ctx, c.owner, c.repo, issueNum, []string{label},
	); err != nil {
		return fmt.Errorf("add label %q to issue %d: %w", label, issueNum, err)
	}
	return nil
}

// RemoveLabel takes label off the issue.
//
// HTTP 404 is treated as success, not as a failure (design decision D7). PRD FR4
// requires the in-progress lock to come off the issue on every exit path,
// crash recovery included, and crash recovery re-runs cleanup that has usually
// already succeeded once. If "the label was already gone" were an error, every
// re-run after a crash would report failure and leave the issue locked forever
// with nothing to tell an operator why.
//
// Only 404 is swallowed. A revoked token (401), missing permission (403) or a
// GitHub outage (5xx) still surfaces: those leave the lock on the issue, and
// hiding them would strand it silently. A 422 for a label name that does not
// exist in the repository at all also surfaces, because that is a configuration
// bug rather than an already-satisfied lock.
//
// The label is interpolated into the request path by the GitHub client without
// URL escaping, so a configured label must stay URL-safe (no spaces or slashes).
// The shipped labels satisfy that; a label with a space would target the wrong
// URL on this call and only this call.
func (c *apiClient) RemoveLabel(ctx context.Context, issueNum int, label string) error {
	_, err := c.api.Issues.RemoveLabelForIssue(ctx, c.owner, c.repo, issueNum, label)
	if err == nil {
		return nil
	}
	if isNotFound(err) {
		return nil
	}
	return fmt.Errorf("remove label %q from issue %d: %w", label, issueNum, err)
}

// isNotFound reports whether err is the GitHub client's own HTTP 404.
//
// The status code is the only thing inspected. Matching on the message text
// instead would be brittle: GitHub phrases a missing label, a missing issue and
// a missing repository almost identically, and those messages are not an API
// contract. The GitHub client wraps every non-2xx response in an ErrorResponse
// carrying the original response, including a 404 with an empty or non-JSON
// body, so no other error shape needs handling here.
func isNotFound(err error) bool {
	var apiErr *gh.ErrorResponse
	return errors.As(err, &apiErr) &&
		apiErr.Response != nil &&
		apiErr.Response.StatusCode == http.StatusNotFound
}

// CreatePR opens a pull request.
//
// There is no merge counterpart anywhere in this package: opening a pull
// request is the end of the factory's involvement, and a human decides what
// happens next (PRD FR5).
func (c *apiClient) CreatePR(ctx context.Context, spec PullRequestSpec) (*PullRequest, error) {
	created, _, err := c.api.PullRequests.Create(ctx, c.owner, c.repo, &gh.NewPullRequest{
		Head:  gh.String(spec.Head),
		Base:  gh.String(spec.Base),
		Title: gh.String(spec.Title),
		Body:  gh.String(spec.Body),
	})
	if err != nil {
		return nil, fmt.Errorf("create pull request from %q into %q: %w", spec.Head, spec.Base, err)
	}
	return pullRequestFrom(created), nil
}

// CommentOnIssue posts body as a new comment on the issue.
func (c *apiClient) CommentOnIssue(ctx context.Context, issueNum int, body string) error {
	if _, _, err := c.api.Issues.CreateComment(
		ctx, c.owner, c.repo, issueNum, &gh.IssueComment{Body: gh.String(body)},
	); err != nil {
		return fmt.Errorf("comment on issue %d: %w", issueNum, err)
	}
	return nil
}
