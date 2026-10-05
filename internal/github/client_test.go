package github_test

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Zheng5005/lights-out/internal/config"
	"github.com/Zheng5005/lights-out/internal/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testOwner = "Zheng5005"
	testRepo  = "Spotify-clone-frontend-focus-"
)

// searchHandler answers a search request and records the query it received.
type recordedSearch struct {
	query string
	sort  string
	order string
	page  string
}

// newSearchServer serves handler and returns the server plus the recorded
// search parameters. Requests are recorded under a mutex because go-github
// pages concurrently only in principle, but the race detector should not have to
// be trusted to stay quiet.
func newSearchServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, func() []recordedSearch) {
	t.Helper()

	var (
		mu      sync.Mutex
		records []recordedSearch
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		mu.Lock()
		records = append(records, recordedSearch{
			query: query.Get("q"),
			sort:  query.Get("sort"),
			order: query.Get("order"),
			page:  query.Get("page"),
		})
		mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(server.Close)

	return server, func() []recordedSearch {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedSearch(nil), records...)
	}
}

// newTestClient builds a Client pointed at server with the shipped labels.
func newTestClient(t *testing.T, server *httptest.Server, labels config.Labels) github.Client {
	t.Helper()

	t.Setenv(github.TokenEnvVar, secretEnv)

	client, err := github.NewClient(
		config.Config{Repo: testOwner + "/" + testRepo, Labels: labels},
		github.WithBaseURL(server.URL),
	)
	require.NoError(t, err)
	return client
}

// writeJSON is the standard success responder.
func writeJSON(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	_, err := w.Write([]byte(body))
	require.NoError(t, err)
}

// TestListClaimCandidatesSendsTheDocumentedQuery pins the wire contract: the
// is:issue qualifier, the label, both exclusions, and oldest-first ordering.
// Each part is load-bearing, so the whole query is asserted as one string.
func TestListClaimCandidatesSendsTheDocumentedQuery(t *testing.T) {
	server, records := newSearchServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, `{"total_count":0,"incomplete_results":false,"items":[]}`)
	})

	client := newTestClient(t, server, defaultLabels())

	_, err := client.ListClaimCandidates(context.Background())

	require.NoError(t, err)
	require.Len(t, records(), 1)

	got := records()[0]
	assert.Equal(t,
		`repo:Zheng5005/Spotify-clone-frontend-focus- is:issue is:open `+
			`label:"dark-factory" -label:"factory-in-progress" -label:"factory-blocked"`,
		got.query)
	assert.Equal(t, "created", got.sort, "oldest-first requires sort=created")
	assert.Equal(t, "asc", got.order, "oldest-first requires order=asc")
}

// TestListClaimCandidatesMapsTheResponse covers the domain mapping, including
// the nulls GitHub really sends: an issue created from a blank body has
// "body": null, and a label may come back without a name.
func TestListClaimCandidatesMapsTheResponse(t *testing.T) {
	server, _ := newSearchServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, `{
			"total_count": 2,
			"items": [
				{
					"number": 7,
					"title": "Fix the flaky login test",
					"body": "It fails on CI about one run in five.",
					"state": "open",
					"labels": [{"name": "dark-factory"}, {"name": "good first issue"}],
					"html_url": "https://github.com/o/r/issues/7",
					"created_at": "2024-03-01T10:00:00Z"
				},
				{
					"number": 8,
					"title": "Document the release process",
					"body": null,
					"state": "open",
					"labels": null,
					"html_url": "https://github.com/o/r/issues/8",
					"created_at": null
				}
			]
		}`)
	})

	client := newTestClient(t, server, defaultLabels())

	issues, err := client.ListClaimCandidates(context.Background())

	require.NoError(t, err)
	require.Len(t, issues, 2)

	assert.Equal(t, github.Issue{
		Number:    7,
		Title:     "Fix the flaky login test",
		Body:      "It fails on CI about one run in five.",
		State:     "open",
		Labels:    []string{"dark-factory", "good first issue"},
		URL:       "https://github.com/o/r/issues/7",
		CreatedAt: time.Date(2024, 3, 1, 10, 0, 0, 0, time.UTC),
	}, issues[0])

	assert.Equal(t, 8, issues[1].Number)
	assert.Empty(t, issues[1].Body, "a null body must not panic")
	assert.Empty(t, issues[1].CreatedAt, "a null created_at must not panic")
	assert.Empty(t, issues[1].Labels, "null labels must not panic")
}

// TestListClaimCandidatesFollowsPagination proves the factory sees every
// candidate, not just the first page: silently claiming one issue at a time
// would starve the concurrency limit.
func TestListClaimCandidatesFollowsPagination(t *testing.T) {
	var (
		pages   int
		server  *httptest.Server
		records func() []recordedSearch
	)
	server, records = newSearchServer(t, func(w http.ResponseWriter, r *http.Request) {
		pages++
		if r.URL.Query().Get("page") == "" {
			w.Header().Set("Link", fmt.Sprintf(
				`<%s/search/issues?q=x&page=2>; rel="next"`, server.URL))
			writeJSON(t, w, `{"total_count":2,"items":[{"number":1,"title":"first"}]}`)
			return
		}
		writeJSON(t, w, `{"total_count":2,"items":[{"number":2,"title":"second"}]}`)
	})

	client := newTestClient(t, server, defaultLabels())

	issues, err := client.ListClaimCandidates(context.Background())

	require.NoError(t, err)
	require.Len(t, issues, 2, "both pages must be returned")
	assert.Equal(t, 1, issues[0].Number)
	assert.Equal(t, 2, issues[1].Number)
	assert.Equal(t, 2, pages)
	assert.Len(t, records(), 2)
}

// TestListClaimCandidatesPropagatesAnAPIError proves a failure is never reported
// as an empty candidate list, which the pipeline would read as "nothing to do".
func TestListClaimCandidatesPropagatesAnAPIError(t *testing.T) {
	server, _ := newSearchServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		writeJSON(t, w, `{"message":"API rate limit exceeded"}`)
	})

	client := newTestClient(t, server, defaultLabels())

	issues, err := client.ListClaimCandidates(context.Background())

	require.Error(t, err)
	assert.Nil(t, issues)
	assert.ErrorContains(t, err, "rate limit")
}

// TestCountIssuesByLabelSendsTheDocumentedQueryAndReturnsTotal pins the count
// contract and the query it shares with the candidate list.
func TestCountIssuesByLabelSendsTheDocumentedQueryAndReturnsTotal(t *testing.T) {
	server, records := newSearchServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, `{"total_count":2,"items":[{"number":1},{"number":2}]}`)
	})

	client := newTestClient(t, server, defaultLabels())

	count, err := client.CountIssuesByLabel(context.Background(), "factory-in-progress")

	require.NoError(t, err)
	assert.Equal(t, 2, count)
	require.Len(t, records(), 1)
	assert.Equal(t,
		`repo:Zheng5005/Spotify-clone-frontend-focus- is:issue is:open label:"factory-in-progress"`,
		records()[0].query)
}

// TestCountIssuesByLabelHandlesZero proves a quiet factory reports 0 rather
// than an error, so the concurrency gate can compare it directly.
func TestCountIssuesByLabelHandlesZero(t *testing.T) {
	server, _ := newSearchServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, `{"total_count":0,"items":[]}`)
	})

	client := newTestClient(t, server, defaultLabels())

	count, err := client.CountIssuesByLabel(context.Background(), "factory-in-progress")

	require.NoError(t, err)
	assert.Zero(t, count)
}

// TestCountIssuesByLabelPropagatesAnAPIError keeps a count failure from
// looking like a zero count and unlocking the concurrency gate.
func TestCountIssuesByLabelPropagatesAnAPIError(t *testing.T) {
	server, _ := newSearchServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(t, w, `{"message":"Bad credentials"}`)
	})

	client := newTestClient(t, server, defaultLabels())

	count, err := client.CountIssuesByLabel(context.Background(), "factory-in-progress")

	require.Error(t, err)
	assert.Zero(t, count)
	assert.ErrorContains(t, err, "Bad credentials")
}

// TestAddLabelPostsTheLabel proves the lock is applied to the right issue. The
// add-labels endpoint takes a bare JSON array, not an object, so the body shape
// is asserted exactly.
func TestAddLabelPostsTheLabel(t *testing.T) {
	type recorded struct {
		method string
		path   string
		labels []string
	}
	got := make(chan recorded, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var labels []string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&labels))
		got <- recorded{method: r.Method, path: r.URL.Path, labels: labels}
		writeJSON(t, w, `[{"name":"factory-in-progress"}]`)
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, server, defaultLabels())

	require.NoError(t, client.AddLabel(context.Background(), 42, "factory-in-progress"))

	r := <-got
	assert.Equal(t, http.MethodPost, r.method)
	assert.Equal(t, "/repos/Zheng5005/Spotify-clone-frontend-focus-/issues/42/labels", r.path)
	assert.Equal(t, []string{"factory-in-progress"}, r.labels)
}

// TestRemoveLabelDeletesTheLabel is the happy path.
func TestRemoveLabelDeletesTheLabel(t *testing.T) {
	type recorded struct {
		method string
		path   string
	}
	got := make(chan recorded, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- recorded{method: r.Method, path: r.URL.Path}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, server, defaultLabels())

	require.NoError(t, client.RemoveLabel(context.Background(), 42, "factory-in-progress"))

	r := <-got
	assert.Equal(t, http.MethodDelete, r.method)
	assert.Equal(t,
		"/repos/Zheng5005/Spotify-clone-frontend-focus-/issues/42/labels/factory-in-progress",
		r.path)
}

// TestRemoveLabelTreatsNotFoundAsSuccess is design decision D7 and the most
// important behaviour in this package.
//
// PRD FR4 requires the in-progress lock to come off the issue on every exit
// path, crash recovery included. Crash recovery re-runs cleanup that has
// already succeeded once, so removal has to be idempotent: if a missing label
// were an error, every re-run after a crash would leave the issue locked
// forever and no human would know why.
func TestRemoveLabelTreatsNotFoundAsSuccess(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{
			name:   "github 404 for an absent label",
			status: http.StatusNotFound,
			body:   `{"message":"Label does not exist"}`,
		},
		{
			name:   "404 with an empty body",
			status: http.StatusNotFound,
			body:   "",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				if tt.body != "" {
					writeJSON(t, w, tt.body)
				}
			}))
			t.Cleanup(server.Close)

			client := newTestClient(t, server, defaultLabels())

			require.NoError(t, client.RemoveLabel(context.Background(), 42, "factory-in-progress"),
				"a label the issue does not carry is already the desired state")
		})
	}
}

// TestRemoveLabelStillReportsRealFailures is the other half of D7: treating 404
// as success must not turn every other failure into a silent success, or a
// revoked token would leave issues locked with no diagnostic anywhere.
func TestRemoveLabelStillReportsRealFailures(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{
			name:    "unauthorized",
			status:  http.StatusUnauthorized,
			body:    `{"message":"Bad credentials"}`,
			wantErr: "Bad credentials",
		},
		{
			name:    "forbidden",
			status:  http.StatusForbidden,
			body:    `{"message":"Resource not accessible by integration"}`,
			wantErr: "not accessible",
		},
		{
			name:    "server error",
			status:  http.StatusInternalServerError,
			body:    `{"message":"Server Error"}`,
			wantErr: "Server Error",
		},
		{
			// GitHub answers 422 when the label name does not exist at all in
			// the repository. That is an authoring bug, not an already-solved
			// lock, so it must stay visible.
			name:    "unprocessable label",
			status:  http.StatusUnprocessableEntity,
			body:    `{"message":"Validation Failed"}`,
			wantErr: "Validation Failed",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				writeJSON(t, w, tt.body)
			}))
			t.Cleanup(server.Close)

			client := newTestClient(t, server, defaultLabels())

			err := client.RemoveLabel(context.Background(), 42, "factory-in-progress")

			require.Error(t, err, "only 404 may be swallowed")
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}

// TestCreatePRPostsTheSpec proves head, base, title and body all reach GitHub.
func TestCreatePRPostsTheSpec(t *testing.T) {
	type posted struct {
		head  string
		base  string
		title string
		body  string
	}
	got := make(chan posted, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Head  string `json:"head"`
			Base  string `json:"base"`
			Title string `json:"title"`
			Body  string `json:"body"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		got <- posted{payload.Head, payload.Base, payload.Title, payload.Body}
		writeJSON(t, w, `{
			"number": 99,
			"html_url": "https://github.com/o/r/pull/99",
			"title": "Fix the flaky login test",
			"body": "Closes #7",
			"state": "open",
			"head": {"ref": "factory/issue-7"},
			"base": {"ref": "main"}
		}`)
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, server, defaultLabels())

	pr, err := client.CreatePR(context.Background(), github.PullRequestSpec{
		Head:  "factory/issue-7",
		Base:  "main",
		Title: "Fix the flaky login test",
		Body:  "Closes #7",
	})

	require.NoError(t, err)

	sent := <-got
	assert.Equal(t, "factory/issue-7", sent.head)
	assert.Equal(t, "main", sent.base)
	assert.Equal(t, "Fix the flaky login test", sent.title)
	assert.Equal(t, "Closes #7", sent.body)

	assert.Equal(t, &github.PullRequest{
		Number: 99,
		URL:    "https://github.com/o/r/pull/99",
		Head:   "factory/issue-7",
		Base:   "main",
		Title:  "Fix the flaky login test",
		Body:   "Closes #7",
		State:  "open",
	}, pr)
}

// TestCreatePRHandlesNullFields covers a minimal GitHub payload, where body and
// state may be absent.
func TestCreatePRHandlesNullFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, `{"number":1,"body":null,"title":null,"state":null,"head":null,"base":null}`)
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, server, defaultLabels())

	pr, err := client.CreatePR(context.Background(), github.PullRequestSpec{Head: "a", Base: "main"})

	require.NoError(t, err)
	require.NotNil(t, pr)
	assert.Equal(t, 1, pr.Number)
	assert.Empty(t, pr.Body)
	assert.Empty(t, pr.Head)
	assert.Empty(t, pr.Base)
}

// TestCreatePRPropagatesAnAPIError keeps a failed PR from looking like an
// opened one.
func TestCreatePRPropagatesAnAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		writeJSON(t, w, `{"message":"No commits between main and factory/issue-7"}`)
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, server, defaultLabels())

	pr, err := client.CreatePR(context.Background(), github.PullRequestSpec{Head: "a", Base: "main"})

	require.Error(t, err)
	assert.Nil(t, pr)
	assert.ErrorContains(t, err, "No commits between")
}

// TestCommentOnIssuePostsTheBody proves the report-back path reaches the issue.
func TestCommentOnIssuePostsTheBody(t *testing.T) {
	type recorded struct {
		method string
		path   string
		body   string
	}
	got := make(chan recorded, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Body string `json:"body"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		got <- recorded{r.Method, r.URL.Path, payload.Body}
		writeJSON(t, w, `{"id":1,"body":"done"}`)
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, server, defaultLabels())

	require.NoError(t, client.CommentOnIssue(context.Background(), 42, "PR opened: #99"))

	r := <-got
	assert.Equal(t, http.MethodPost, r.method)
	assert.Equal(t, "/repos/Zheng5005/Spotify-clone-frontend-focus-/issues/42/comments", r.path)
	assert.Equal(t, "PR opened: #99", r.body)
}

// TestCommentOnIssuePropagatesAnAPIError proves a failed comment is reported.
func TestCommentOnIssuePropagatesAnAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		writeJSON(t, w, `{"message":"Issue Not Found"}`)
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, server, defaultLabels())

	err := client.CommentOnIssue(context.Background(), 42, "done")

	require.Error(t, err)
	assert.ErrorContains(t, err, "Issue Not Found")
}

// TestClientAuthenticatesEveryRequest proves the resolved token reaches GitHub
// on the real client, not only through NewTokenHTTPClient in isolation.
func TestClientAuthenticatesEveryRequest(t *testing.T) {
	authorizations := make(chan string, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizations <- r.Header.Get("Authorization")
		writeJSON(t, w, `{"total_count":0,"items":[]}`)
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, server, defaultLabels())

	_, err := client.CountIssuesByLabel(context.Background(), "factory-in-progress")

	require.NoError(t, err)
	assert.Equal(t, "Bearer "+secretEnv, <-authorizations)
}

// TestNewClientFailsWithoutAToken keeps a missing credential a construction
// failure rather than a runtime surprise on the first HTTP call.
func TestNewClientFailsWithoutAToken(t *testing.T) {
	t.Setenv(github.TokenEnvVar, "")
	t.Setenv(github.FallbackTokenEnvVar, "")
	t.Setenv(github.EnvFileEnvVar, filepath.Join(t.TempDir(), "absent.env"))

	client, err := github.NewClient(config.Config{Repo: "owner/repo"})

	require.Error(t, err)
	assert.Nil(t, client)
	assert.NotContains(t, err.Error(), secretEnv, "the error must never echo a token")
}

// TestNewClientRejectsAMalformedRepo keeps the owner/repo split honest before
// it is concatenated into a URL path.
func TestNewClientRejectsAMalformedRepo(t *testing.T) {
	t.Setenv(github.TokenEnvVar, secretEnv)

	cases := []struct {
		name string
		repo string
	}{
		{name: "empty", repo: ""},
		{name: "no slash", repo: "owner"},
		{name: "two slashes", repo: "owner/repo/extra"},
		{name: "empty owner", repo: "/repo"},
		{name: "empty name", repo: "owner/"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			client, err := github.NewClient(config.Config{Repo: tt.repo})

			require.Error(t, err)
			assert.Nil(t, client)
			assert.ErrorContains(t, err, "owner/repo")
		})
	}
}

// fakeClient is a test-local fake, the mockability proof design decision D9
// asks for. It implements every Client method and nothing else, so it only
// compiles while the interface stays complete and merge-free.
type fakeClient struct {
	candidates    []github.Issue
	candidatesErr error

	removedLabels []string
	addedLabels   []string
	comments      []string
	createdPRs    []github.PullRequestSpec
	counts        map[string]int
}

func (f *fakeClient) ListClaimCandidates(context.Context) ([]github.Issue, error) {
	return f.candidates, f.candidatesErr
}

func (f *fakeClient) CountIssuesByLabel(_ context.Context, label string) (int, error) {
	return f.counts[label], nil
}

func (f *fakeClient) AddLabel(_ context.Context, _ int, label string) error {
	f.addedLabels = append(f.addedLabels, label)
	return nil
}

func (f *fakeClient) RemoveLabel(_ context.Context, _ int, label string) error {
	f.removedLabels = append(f.removedLabels, label)
	return nil
}

func (f *fakeClient) CreatePR(_ context.Context, spec github.PullRequestSpec) (*github.PullRequest, error) {
	f.createdPRs = append(f.createdPRs, spec)
	return &github.PullRequest{Number: 1, Title: spec.Title}, nil
}

func (f *fakeClient) CommentOnIssue(_ context.Context, _ int, body string) error {
	f.comments = append(f.comments, body)
	return nil
}

// TestFakeClientSatisfiesTheInterface proves the boundary is mockable without a
// shared mock package: Phase 5 can substitute a double as easily as this does.
func TestFakeClientSatisfiesTheInterface(t *testing.T) {
	var client github.Client = &fakeClient{
		candidates: []github.Issue{{Number: 7, Title: "Fix the flaky login test"}},
		counts:     map[string]int{"factory-in-progress": 1},
	}

	candidates, err := client.ListClaimCandidates(context.Background())
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	assert.Equal(t, 7, candidates[0].Number)

	count, err := client.CountIssuesByLabel(context.Background(), "factory-in-progress")
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	require.NoError(t, client.AddLabel(context.Background(), 7, "factory-in-progress"))
	require.NoError(t, client.RemoveLabel(context.Background(), 7, "factory-in-progress"))
	require.NoError(t, client.CommentOnIssue(context.Background(), 7, "done"))

	pr, err := client.CreatePR(context.Background(), github.PullRequestSpec{Title: "t"})
	require.NoError(t, err)
	assert.Equal(t, 1, pr.Number)

	fake := client.(*fakeClient)
	assert.Equal(t, []string{"factory-in-progress"}, fake.addedLabels)
	assert.Equal(t, []string{"factory-in-progress"}, fake.removedLabels)
	assert.Equal(t, []string{"done"}, fake.comments)
	assert.Len(t, fake.createdPRs, 1)
}

// TestPackageHasNoMergeCapability enforces PRD FR5 structurally. A missing
// method is stronger than a forbidden one, but "do not add merge" decays the
// first time someone adds a helper, so the absence is asserted rather than
// assumed. Test files are excluded: only shipped package code is inspected.
func TestPackageHasNoMergeCapability(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	fset := token.NewFileSet()
	inspected := 0

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		inspected++

		file, err := parser.ParseFile(fset, name, nil, 0)
		require.NoErrorf(t, err, "parse %s", name)

		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				assert.NotContainsf(t, strings.ToLower(d.Name.Name), "merge",
					"%s declares %s: PRD FR5 forbids any merge capability in internal/github",
					name, d.Name.Name)
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						assert.NotContainsf(t, strings.ToLower(s.Name.Name), "merge",
							"%s declares type %s: PRD FR5 forbids any merge capability",
							name, s.Name.Name)
					case *ast.ValueSpec:
						for _, ident := range s.Names {
							assert.NotContainsf(t, strings.ToLower(ident.Name), "merge",
								"%s declares %s: PRD FR5 forbids any merge capability",
								name, ident.Name)
						}
					}
				}
			}
		}
	}

	assert.Positive(t, inspected, "no package source was inspected, so the guard proves nothing")
}
