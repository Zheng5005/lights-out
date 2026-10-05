// Package github is the factory's GitHub boundary.
//
// Everything the dark factory needs from GitHub is expressed as the Client
// interface, so later phases (pipeline, daily, Herdr) depend on an intention-
// revealing contract rather than on HTTP response shapes.
//
// Two deliberate absences:
//
//   - There is no merge method anywhere in this package, not even unexported.
//     PRD FR5 requires that no code path may merge a pull request, and a
//     missing capability is stronger than a forbidden one: a future caller
//     cannot reach a merge even by mistake.
//
//   - There is no way to list a pull request as if it were an issue. Pull
//     requests are excluded server-side by the is:issue qualifier rather than
//     filtered afterwards, so the pipeline never has to remember the rule.
//
// Authentication is a bearer token injected by a custom RoundTripper, resolved
// from the environment or a .env file. The token value never appears in an
// error, a log line, or a test failure: errors name the sources they tried.
package github

import (
	"context"
	"time"

	gh "github.com/google/go-github/v69/github"
)

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
	Head  string
	Base  string
	Title string
	Body  string
}

// PullRequest is an opened pull request.
type PullRequest struct {
	Number int
	URL    string
	Head   string
	Base   string
	Title  string
	Body   string
	State  string
}

// Client is the GitHub surface the factory depends on.
//
// There is intentionally no Merge method; see the package comment.
type Client interface {
	// ListClaimCandidates returns open issues labeled labels.trigger that do
	// not carry labels.in_progress or labels.blocked, oldest first. Pull
	// requests are excluded server-side and must never appear here.
	ListClaimCandidates(ctx context.Context) ([]Issue, error)

	// CountIssuesByLabel returns how many open issues currently carry label.
	CountIssuesByLabel(ctx context.Context, label string) (int, error)

	// AddLabel puts label on the issue.
	AddLabel(ctx context.Context, issueNum int, label string) error

	// RemoveLabel takes label off the issue. It is idempotent: a label the
	// issue does not carry is already the desired state and returns nil, so
	// a crash-recovery path can always release the lock. See RemoveLabel's
	// implementation for why that matters.
	RemoveLabel(ctx context.Context, issueNum int, label string) error

	// CreatePR opens a pull request from spec.
	CreatePR(ctx context.Context, spec PullRequestSpec) (*PullRequest, error)

	// CommentOnIssue posts body as a new comment on the issue.
	CommentOnIssue(ctx context.Context, issueNum int, body string) error
}

// issueFrom maps a GitHub API issue into the domain Issue.
//
// Every field goes through go-github's nil-safe accessors because GitHub omits
// and nulls fields freely: an issue opened from the web UI with no description
// carries "body": null, and a label may arrive without a name. Dereferencing
// those pointers directly would panic on ordinary, successful responses.
func issueFrom(src *gh.Issue) Issue {
	if src == nil {
		return Issue{}
	}

	issue := Issue{
		Number:    src.GetNumber(),
		Title:     src.GetTitle(),
		Body:      src.GetBody(),
		State:     src.GetState(),
		URL:       src.GetHTMLURL(),
		CreatedAt: src.GetCreatedAt().Time,
	}
	for _, label := range src.Labels {
		// A nameless label carries no information for the pipeline, and an
		// empty string in Labels would read as a real label name.
		if name := label.GetName(); name != "" {
			issue.Labels = append(issue.Labels, name)
		}
	}
	return issue
}

// issuesFrom maps a page of search results, preserving the order GitHub chose
// (oldest first for the claim-candidate query) and skipping nil entries.
func issuesFrom(src []*gh.Issue) []Issue {
	if len(src) == 0 {
		return nil
	}
	issues := make([]Issue, 0, len(src))
	for _, item := range src {
		if item != nil {
			issues = append(issues, issueFrom(item))
		}
	}
	return issues
}

// pullRequestFrom maps a GitHub API pull request into the domain PullRequest.
// The branch refs are flattened to strings because that is all the factory ever
// reports back to a human.
func pullRequestFrom(src *gh.PullRequest) *PullRequest {
	if src == nil {
		return nil
	}
	return &PullRequest{
		Number: src.GetNumber(),
		URL:    src.GetHTMLURL(),
		Head:   src.GetHead().GetRef(),
		Base:   src.GetBase().GetRef(),
		Title:  src.GetTitle(),
		Body:   src.GetBody(),
		State:  src.GetState(),
	}
}
