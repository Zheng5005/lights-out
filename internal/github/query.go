package github

import (
	"fmt"
	"strings"

	"github.com/Zheng5005/lights-out/internal/config"
)

// labelEscaper escapes the two characters that would otherwise terminate or
// escape a quoted label inside a GitHub search query. strings.NewReplacer
// replaces in a single pass, so an escaped backslash is never re-escaped.
var labelEscaper = strings.NewReplacer(
	`\`, `\\`,
	`"`, `\"`,
)

// ClaimCandidatesQuery builds the search query selecting open issues that carry
// the trigger label and neither the in-progress nor the blocked label.
//
// Three requirements collapse into this one string, which is why it is built
// here and nowhere else:
//
//   - is:issue excludes pull requests server-side, so no client-side filter is
//     needed (design decision D2).
//   - the caller must sort oldest-first, and the list-issues endpoint is
//     newest-first with no order control; SearchOptions carries sort/order
//     alongside this query (design decision D1).
//   - -label: performs the in-progress and blocked exclusion in the same round
//     trip, so the pipeline cannot fetch an issue it is forbidden to claim.
//
// The result is a pure function of its arguments: no I/O, no clock, no config
// lookup, which is what makes the exact string unit-testable.
func ClaimCandidatesQuery(repo string, labels config.Labels) string {
	return fmt.Sprintf(
		"repo:%s is:issue is:open label:%s -label:%s -label:%s",
		repo,
		quoteLabel(labels.Trigger),
		quoteLabel(labels.InProgress),
		quoteLabel(labels.Blocked),
	)
}

// CountIssuesByLabelQuery builds the search query counting open issues carrying
// label. It shares the repo and is:issue qualifiers with ClaimCandidatesQuery so
// a count and the candidate list can never disagree about what an "issue" is.
func CountIssuesByLabelQuery(repo string, label string) string {
	return fmt.Sprintf("repo:%s is:issue is:open label:%s", repo, quoteLabel(label))
}

// quoteLabel wraps a label name in double quotes, escaping any quote or
// backslash it already contains. Quoting is unconditional so a label with a
// space stays a single term instead of splitting into two.
func quoteLabel(label string) string {
	return `"` + labelEscaper.Replace(label) + `"`
}
