package github_test

import (
	"testing"

	"github.com/Zheng5005/lights-out/internal/config"
	"github.com/Zheng5005/lights-out/internal/github"
	"github.com/stretchr/testify/assert"
)

// defaultLabels mirrors the shipped config.yaml labels so the expected query
// strings below are the ones the real factory sends.
func defaultLabels() config.Labels {
	return config.Labels{
		Trigger:    "dark-factory",
		InProgress: "factory-in-progress",
		Blocked:    "factory-blocked",
	}
}

// TestClaimCandidatesQuery pins the exact search string from the Phase 2 design
// doc. The doc is the contract: the oldest-first ordering and the label
// exclusions only work if this string is byte-for-byte what GitHub receives.
func TestClaimCandidatesQuery(t *testing.T) {
	cases := []struct {
		name   string
		repo   string
		labels config.Labels
		want   string
	}{
		{
			name:   "shipped labels produce the documented query",
			repo:   "owner/repo",
			labels: defaultLabels(),
			want: `repo:owner/repo is:issue is:open label:"dark-factory" ` +
				`-label:"factory-in-progress" -label:"factory-blocked"`,
		},
		{
			name:   "a label with spaces stays inside the quotes",
			repo:   "owner/repo",
			labels: config.Labels{Trigger: "needs triage", InProgress: "in progress", Blocked: "blocked"},
			want: `repo:owner/repo is:issue is:open label:"needs triage" ` +
				`-label:"in progress" -label:"blocked"`,
		},
		{
			name:   "double quotes inside a label are escaped",
			repo:   "owner/repo",
			labels: config.Labels{Trigger: `say "hi"`, InProgress: "p", Blocked: "b"},
			want:   `repo:owner/repo is:issue is:open label:"say \"hi\"" -label:"p" -label:"b"`,
		},
		{
			name:   "backslashes inside a label are escaped",
			repo:   "owner/repo",
			labels: config.Labels{Trigger: `back\slash`, InProgress: "p", Blocked: "b"},
			want:   `repo:owner/repo is:issue is:open label:"back\\slash" -label:"p" -label:"b"`,
		},
		{
			name:   "a backslash before a quote is escaped independently",
			repo:   "owner/repo",
			labels: config.Labels{Trigger: `a\"b`, InProgress: "p", Blocked: "b"},
			want:   `repo:owner/repo is:issue is:open label:"a\\\"b" -label:"p" -label:"b"`,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, github.ClaimCandidatesQuery(tt.repo, tt.labels))
		})
	}
}

// TestCountIssuesByLabelQuery pins the count query from the design doc.
func TestCountIssuesByLabelQuery(t *testing.T) {
	cases := []struct {
		name  string
		repo  string
		label string
		want  string
	}{
		{
			name:  "shipped labels produce the documented query",
			repo:  "Zheng5005/Spotify-clone-frontend-focus-",
			label: "factory-in-progress",
			want:  `repo:Zheng5005/Spotify-clone-frontend-focus- is:issue is:open label:"factory-in-progress"`,
		},
		{
			name:  "double quotes inside a label are escaped",
			repo:  "owner/repo",
			label: `weird"label`,
			want:  `repo:owner/repo is:issue is:open label:"weird\"label"`,
		},
		{
			name:  "an empty label still yields a well formed query",
			repo:  "owner/repo",
			label: "",
			want:  `repo:owner/repo is:issue is:open label:""`,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, github.CountIssuesByLabelQuery(tt.repo, tt.label))
		})
	}
}
