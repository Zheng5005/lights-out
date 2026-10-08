package pipeline_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Zheng5005/lights-out/internal/config"
	"github.com/Zheng5005/lights-out/internal/daily"
	"github.com/Zheng5005/lights-out/internal/github"
	"github.com/Zheng5005/lights-out/internal/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixedNow is the clock every counter in this file agrees on. A fixed instant
// keeps "today" deterministic: if the recording counter and the seeder
// disagreed on the UTC date, the counter would read as rolled over to 0.
var fixedNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

// callSeq records the interleaving of the pipeline's external calls so tests
// can assert order, not just presence. The fake GitHub client and the daily
// counter's injected clock both append to it.
type callSeq struct {
	events []string
}

func (s *callSeq) record(event string) { s.events = append(s.events, event) }

// labelCall captures one AddLabel/RemoveLabel invocation.
type labelCall struct {
	issueNum int
	label    string
}

// commentCall captures one CommentOnIssue invocation.
type commentCall struct {
	issueNum int
	body     string
}

// fakeGitHub is a hand-rolled recording fake for github.Client. Every method
// appends its name to seq, captures its arguments and context, and returns
// the canned result configured by the test.
type fakeGitHub struct {
	seq *callSeq

	// Canned results.
	countResult   int
	countErr      error
	candidates    []github.Issue
	candidatesErr error
	addLabelErr   error

	// Recorded observations, one field per Client method.
	countLabels   []string
	listCalls     int
	addLabelCalls []labelCall
	removeCalls   []labelCall
	createPRCalls []github.PullRequestSpec
	commentCalls  []commentCall
	ctxs          []context.Context
}

var _ github.Client = (*fakeGitHub)(nil)

func (g *fakeGitHub) ListClaimCandidates(ctx context.Context) ([]github.Issue, error) {
	g.seq.record("gh.ListClaimCandidates")
	g.ctxs = append(g.ctxs, ctx)
	g.listCalls++
	return g.candidates, g.candidatesErr
}

func (g *fakeGitHub) CountIssuesByLabel(ctx context.Context, label string) (int, error) {
	g.seq.record("gh.CountIssuesByLabel")
	g.ctxs = append(g.ctxs, ctx)
	g.countLabels = append(g.countLabels, label)
	return g.countResult, g.countErr
}

func (g *fakeGitHub) AddLabel(ctx context.Context, issueNum int, label string) error {
	g.seq.record("gh.AddLabel")
	g.ctxs = append(g.ctxs, ctx)
	g.addLabelCalls = append(g.addLabelCalls, labelCall{issueNum: issueNum, label: label})
	return g.addLabelErr
}

func (g *fakeGitHub) RemoveLabel(ctx context.Context, issueNum int, label string) error {
	g.seq.record("gh.RemoveLabel")
	g.ctxs = append(g.ctxs, ctx)
	g.removeCalls = append(g.removeCalls, labelCall{issueNum: issueNum, label: label})
	return nil
}

func (g *fakeGitHub) CreatePR(ctx context.Context, spec github.PullRequestSpec) (*github.PullRequest, error) {
	g.seq.record("gh.CreatePR")
	g.ctxs = append(g.ctxs, ctx)
	g.createPRCalls = append(g.createPRCalls, spec)
	return nil, nil
}

func (g *fakeGitHub) CommentOnIssue(ctx context.Context, issueNum int, body string) error {
	g.seq.record("gh.CommentOnIssue")
	g.ctxs = append(g.ctxs, ctx)
	g.commentCalls = append(g.commentCalls, commentCall{issueNum: issueNum, body: body})
	return nil
}

// testConfig returns a valid factory configuration with both limits at 2.
func testConfig() config.Config {
	return config.Config{
		Enabled:          true,
		Repo:             "example/ship",
		ConcurrencyLimit: 2,
		DailyLimit:       2,
		Labels: config.Labels{
			Trigger:    "dark-factory",
			InProgress: "factory-in-progress",
			Blocked:    "factory-blocked",
		},
	}
}

// newTestCounter returns the counter Run will use plus its backing file path.
// Its clock records "daily.clock" into seq before answering with fixedNow, so
// every Current and Increment shows up in the recorded sequence. The path lets
// tests seed and verify through a clock that does not record.
func newTestCounter(t *testing.T, seq *callSeq) (*daily.Counter, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "daily-claims.json")
	counter := daily.NewWithClock(path, func() time.Time {
		seq.record("daily.clock")
		return fixedNow
	})
	return counter, path
}

// seedDaily charges the counter n times through a non-recording clock, so
// seeding leaves no events in the sequence Run is asserted against.
func seedDaily(t *testing.T, path string, n int) {
	t.Helper()
	seeder := daily.NewWithClock(path, func() time.Time { return fixedNow })
	for i := 0; i < n; i++ {
		_, err := seeder.Increment()
		require.NoError(t, err)
	}
}

// currentDaily reads the counter through a non-recording clock: verification
// never adds events to the sequence either.
func currentDaily(t *testing.T, path string) int {
	t.Helper()
	reader := daily.NewWithClock(path, func() time.Time { return fixedNow })
	count, err := reader.Current()
	require.NoError(t, err)
	return count
}

// TestRunDisabledGateHasZeroSideEffects is the FR1 proof: with enabled false,
// Run returns nil without touching GitHub, the daily counter, or the herdr
// dispatcher. herdr is passed as nil — any dispatch call would panic the test
// — and each github.Client method is asserted individually so a regression
// names the offending call.
func TestRunDisabledGateHasZeroSideEffects(t *testing.T) {
	seq := &callSeq{}
	gh := &fakeGitHub{seq: seq, countResult: 1} // even spare capacity must not matter
	counter, path := newTestCounter(t, seq)
	cfg := testConfig()
	cfg.Enabled = false

	err := pipeline.Run(context.Background(), cfg, gh, nil, counter)
	require.NoError(t, err, "a disabled gate exits silently")

	assert.Empty(t, gh.countLabels, "CountIssuesByLabel must not run when disabled")
	assert.Zero(t, gh.listCalls, "ListClaimCandidates must not run when disabled")
	assert.Empty(t, gh.addLabelCalls, "AddLabel must not run when disabled")
	assert.Empty(t, gh.removeCalls, "RemoveLabel must not run when disabled")
	assert.Empty(t, gh.createPRCalls, "CreatePR must not run when disabled")
	assert.Empty(t, gh.commentCalls, "CommentOnIssue must not run when disabled")
	assert.Empty(t, seq.events, "no external call of any kind may happen when disabled")
	assert.Zero(t, currentDaily(t, path), "the daily counter must not be charged when disabled")
}

// TestRunSilentExits covers the remaining specified nil exits of §4 steps 2–4:
// both capacity limits and an empty candidate list return nil with no lock.
// The exact event sequence doubles as the proof of what was NOT called.
func TestRunSilentExits(t *testing.T) {
	tests := []struct {
		name string
		// mutate seeds the preconditions of the case after the fixture is
		// built but before Run is called.
		mutate       func(t *testing.T, cfg *config.Config, gh *fakeGitHub, path string)
		wantListCall int
		wantEvents   []string
	}{
		{
			name: "concurrency at limit stops before claim",
			mutate: func(t *testing.T, cfg *config.Config, gh *fakeGitHub, path string) {
				gh.countResult = cfg.ConcurrencyLimit
			},
			wantListCall: 0,
			wantEvents:   []string{"gh.CountIssuesByLabel"},
		},
		{
			name: "daily at limit stops despite concurrency headroom",
			mutate: func(t *testing.T, cfg *config.Config, gh *fakeGitHub, path string) {
				gh.countResult = 0 // well under cfg.ConcurrencyLimit
				seedDaily(t, path, cfg.DailyLimit)
			},
			wantListCall: 0,
			wantEvents:   []string{"gh.CountIssuesByLabel", "daily.clock"},
		},
		{
			name: "no claim candidates when the list is nil",
			mutate: func(t *testing.T, cfg *config.Config, gh *fakeGitHub, path string) {
				gh.candidates = nil
			},
			wantListCall: 1,
			wantEvents: []string{
				"gh.CountIssuesByLabel",
				"daily.clock",
				"gh.ListClaimCandidates",
			},
		},
		{
			name: "no claim candidates when the list is empty",
			mutate: func(t *testing.T, cfg *config.Config, gh *fakeGitHub, path string) {
				gh.candidates = []github.Issue{}
			},
			wantListCall: 1,
			wantEvents: []string{
				"gh.CountIssuesByLabel",
				"daily.clock",
				"gh.ListClaimCandidates",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seq := &callSeq{}
			gh := &fakeGitHub{seq: seq}
			counter, path := newTestCounter(t, seq)
			cfg := testConfig()
			tt.mutate(t, &cfg, gh, path)

			err := pipeline.Run(context.Background(), cfg, gh, nil, counter)
			require.NoError(t, err, "a capacity or candidate miss exits silently")

			assert.Equal(t, tt.wantEvents, seq.events)
			assert.Equal(t, tt.wantListCall, gh.listCalls, "unexpected ListClaimCandidates call count")
			assert.Empty(t, gh.addLabelCalls, "no issue may be locked on a silent exit")
		})
	}
}

// TestRunHappyPathClaimsOldestAndLocksBeforeCharging proves the whole §4
// claim sequence: capacity checks first, the oldest candidate claimed, the
// in-progress label applied as the lock, and only then the daily counter
// charged. The event list is compared in full, so any reordering fails.
func TestRunHappyPathClaimsOldestAndLocksBeforeCharging(t *testing.T) {
	seq := &callSeq{}
	oldest := github.Issue{Number: 7, Title: "oldest"}
	newer := github.Issue{Number: 42, Title: "newer"}
	gh := &fakeGitHub{
		seq: seq,
		// Candidates arrive oldest first per the github.Client contract.
		candidates: []github.Issue{oldest, newer},
	}
	counter, path := newTestCounter(t, seq)
	cfg := testConfig()
	ctx := context.Background()

	err := pipeline.Run(ctx, cfg, gh, nil, counter)
	require.NoError(t, err)

	// The counter's clock fires exactly once in Current (the capacity
	// check) and once in Increment (the charge), so the second
	// "daily.clock" after "gh.AddLabel" proves the lock preceded the
	// charge — the FR4 ordering.
	want := []string{
		"gh.CountIssuesByLabel",
		"daily.clock", // daily.Current
		"gh.ListClaimCandidates",
		"gh.AddLabel",
		"daily.clock", // daily.Increment
	}
	require.Equal(t, want, seq.events, "lock must be applied before the daily counter is charged")

	require.Len(t, gh.addLabelCalls, 1)
	assert.Equal(t,
		labelCall{issueNum: oldest.Number, label: cfg.Labels.InProgress},
		gh.addLabelCalls[0],
		"the oldest candidate must be locked with the in-progress label")
	assert.Equal(t, []string{cfg.Labels.InProgress}, gh.countLabels,
		"capacity must be counted against the in-progress label")
	assert.Equal(t, 1, gh.listCalls)

	for _, got := range gh.ctxs {
		assert.Equal(t, ctx, got, "every GitHub call must receive the caller's context")
	}

	// Read through a non-recording clock, after the sequence assertion:
	// exactly one claim was charged, i.e. Increment ran exactly once.
	assert.Equal(t, 1, currentDaily(t, path))
}

// TestRunAddLabelErrorAbortsWithoutChargingDaily is the FR4 failure side: a
// failed lock returns its error before daily.Increment, so a claim that never
// took is never charged and no further step runs.
func TestRunAddLabelErrorAbortsWithoutChargingDaily(t *testing.T) {
	seq := &callSeq{}
	boom := errors.New("add label issue 7: 422")
	gh := &fakeGitHub{
		seq:         seq,
		candidates:  []github.Issue{{Number: 7}},
		addLabelErr: boom,
	}
	counter, path := newTestCounter(t, seq)

	err := pipeline.Run(context.Background(), testConfig(), gh, nil, counter)
	require.ErrorIs(t, err, boom, "the lock error must be returned unchanged")

	want := []string{
		"gh.CountIssuesByLabel",
		"daily.clock",
		"gh.ListClaimCandidates",
		"gh.AddLabel",
	}
	require.Equal(t, want, seq.events, "no Increment may follow a failed lock")
	require.Len(t, gh.addLabelCalls, 1)
	assert.Equal(t, 0, currentDaily(t, path), "the daily counter must not be charged")
}

// TestRunCountErrorReturnsBeforeDailyCheck verifies the first capacity step
// surfaces its error before the counter is even read.
func TestRunCountErrorReturnsBeforeDailyCheck(t *testing.T) {
	seq := &callSeq{}
	boom := errors.New("count issues labeled \"factory-in-progress\": 502")
	gh := &fakeGitHub{seq: seq, countErr: boom}
	counter, _ := newTestCounter(t, seq)

	err := pipeline.Run(context.Background(), testConfig(), gh, nil, counter)
	require.ErrorIs(t, err, boom)

	assert.Equal(t, []string{"gh.CountIssuesByLabel"}, seq.events)
	assert.Zero(t, gh.listCalls, "no claim attempt after a failed capacity check")
	assert.Empty(t, gh.addLabelCalls)
}

// TestRunDailyCurrentErrorReturnsBeforeClaim makes the daily capacity step
// fail on a corrupt counter file and proves Run surfaces it before any claim.
// Current fails parsing before it consults the clock, hence no clock event.
func TestRunDailyCurrentErrorReturnsBeforeClaim(t *testing.T) {
	seq := &callSeq{}
	gh := &fakeGitHub{seq: seq}
	counter, path := newTestCounter(t, seq)
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))

	err := pipeline.Run(context.Background(), testConfig(), gh, nil, counter)
	require.ErrorContains(t, err, "parse daily counter")

	assert.Equal(t, []string{"gh.CountIssuesByLabel"}, seq.events)
	assert.Zero(t, gh.listCalls, "no claim attempt after a failed daily check")
	assert.Empty(t, gh.addLabelCalls)
}

// TestRunListCandidatesErrorReturnsBeforeLock verifies a failed claim query
// never reaches the lock or the charge.
func TestRunListCandidatesErrorReturnsBeforeLock(t *testing.T) {
	seq := &callSeq{}
	boom := errors.New("list claim candidates: 503")
	gh := &fakeGitHub{seq: seq, candidatesErr: boom}
	counter, path := newTestCounter(t, seq)

	err := pipeline.Run(context.Background(), testConfig(), gh, nil, counter)
	require.ErrorIs(t, err, boom)

	want := []string{
		"gh.CountIssuesByLabel",
		"daily.clock",
		"gh.ListClaimCandidates",
	}
	require.Equal(t, want, seq.events)
	assert.Empty(t, gh.addLabelCalls, "a failed claim query must not lock anything")
	assert.Equal(t, 0, currentDaily(t, path))
}
