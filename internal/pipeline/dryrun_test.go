package pipeline_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/Zheng5005/lights-out/internal/config"
	"github.com/Zheng5005/lights-out/internal/github"
	"github.com/Zheng5005/lights-out/internal/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDryRunDisabledGateIsReadOnly is the FR1 dry-run case: a disabled gate
// stops at the gate with the would-exit message and makes NO external call of
// any kind — no capacity count, no daily read, no claim query. It reuses the
// same fakes and call recorder as the Run suite.
func TestDryRunDisabledGateIsReadOnly(t *testing.T) {
	var buf bytes.Buffer
	seq := &callSeq{}
	gh := &fakeGitHub{seq: seq, countResult: 1} // spare capacity must not matter
	counter, path := newTestCounter(t, seq)
	cfg := testConfig()
	cfg.Enabled = false

	var err error
	captureLog(t, &buf, func() {
		err = pipeline.DryRun(context.Background(), cfg, gh, counter)
	})
	require.NoError(t, err, "a disabled gate is a silent no-op in dry-run too")

	out := buf.String()
	assert.Contains(t, out, "mode=dry-run")
	assert.Contains(t, out, "gate=disabled")
	assert.Contains(t, out, "would exit 0, no candidate")
	assert.Empty(t, seq.events, "a disabled gate must make no external call")
	assert.Equal(t, 0, currentDaily(t, path), "dry-run must never charge the counter")
}

// TestDryRunStopsReadOnlyForSilentExits covers the remaining read-only
// decision misses: concurrency full, daily reached, and an empty candidate
// list. Each announces the reason and the would-exit message, and each stops
// before any lock or counter mutation.
func TestDryRunStopsReadOnlyForSilentExits(t *testing.T) {
	tests := []struct {
		name string
		// mutate seeds the preconditions after the fixture is built.
		mutate     func(t *testing.T, cfg *config.Config, gh *fakeGitHub, path string)
		wantLog    []string
		wantEvents []string
		// wantDaily is the counter reading after dry-run: the seeded value
		// for the daily-at-limit case, 0 everywhere else. It proves dry-run
		// never charged the counter beyond what the fixture seeded.
		wantDaily int
	}{
		{
			name: "concurrency at limit",
			mutate: func(t *testing.T, cfg *config.Config, gh *fakeGitHub, path string) {
				gh.countResult = cfg.ConcurrencyLimit
			},
			wantLog:    []string{"gate=passed", "capacity=full (2/2)", "would exit 0, no candidate"},
			wantEvents: []string{"gh.CountIssuesByLabel"},
			wantDaily:  0,
		},
		{
			name: "daily at limit",
			mutate: func(t *testing.T, cfg *config.Config, gh *fakeGitHub, path string) {
				gh.countResult = 0 // well under the concurrency limit
				seedDaily(t, path, cfg.DailyLimit)
			},
			wantLog:    []string{"gate=passed", "capacity=ok (0/2)", "daily=reached (2/2)", "would exit 0, no candidate"},
			wantEvents: []string{"gh.CountIssuesByLabel", "daily.clock"},
			wantDaily:  2, // the seeded value: dry-run adds nothing
		},
		{
			name: "empty candidates",
			mutate: func(t *testing.T, cfg *config.Config, gh *fakeGitHub, path string) {
				gh.candidates = []github.Issue{}
			},
			wantLog:    []string{"candidate=none", "would exit 0, no candidate"},
			wantEvents: []string{"gh.CountIssuesByLabel", "daily.clock", "gh.ListClaimCandidates"},
			wantDaily:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			seq := &callSeq{}
			gh := &fakeGitHub{seq: seq}
			counter, path := newTestCounter(t, seq)
			cfg := testConfig()
			tt.mutate(t, &cfg, gh, path)

			var err error
			captureLog(t, &buf, func() {
				err = pipeline.DryRun(context.Background(), cfg, gh, counter)
			})
			require.NoError(t, err, "a decision miss is a silent no-op in dry-run")

			out := buf.String()
			for _, want := range tt.wantLog {
				assert.Contains(t, out, want)
			}
			assert.Equal(t, tt.wantEvents, seq.events, "dry-run must stop at the recorded decision point")
			assert.Empty(t, gh.addLabelCalls, "dry-run must never lock an issue")
			assert.Equal(t, tt.wantDaily, currentDaily(t, path), "dry-run must never charge the counter")
		})
	}
}

// TestDryRunCandidateReportsWouldClaimWithoutMutating is the core proof that
// dry-run is read-only: a real candidate is reported as `would claim #N`, but
// AddLabel and Increment never run — the recorded event list ends at the claim
// query, and the counter file is untouched.
func TestDryRunCandidateReportsWouldClaimWithoutMutating(t *testing.T) {
	var buf bytes.Buffer
	seq := &callSeq{}
	gh := &fakeGitHub{seq: seq, candidates: []github.Issue{{Number: 7, Title: "Fix the thing"}}}
	counter, path := newTestCounter(t, seq)
	cfg := testConfig()

	var err error
	captureLog(t, &buf, func() {
		err = pipeline.DryRun(context.Background(), cfg, gh, counter)
	})
	require.NoError(t, err)

	out := buf.String()
	assert.Contains(t, out, "candidate=#7")
	assert.Contains(t, out, "would claim #7 (AddLabel, Increment, agent, push, PR skipped)")

	assert.Equal(t,
		[]string{"gh.CountIssuesByLabel", "daily.clock", "gh.ListClaimCandidates"},
		seq.events,
		"dry-run must stop after the claim query — AddLabel and Increment must never run")
	assert.Empty(t, gh.addLabelCalls, "dry-run must never lock an issue")
	assert.Equal(t, 0, currentDaily(t, path), "the daily counter must not be charged in dry-run")
}

// TestDryRunPropagatesCapacityError proves a decision error is returned
// unchanged and never panics, so a broken GitHub client surfaces in dry-run
// exactly as it does in a real cycle.
func TestDryRunPropagatesCapacityError(t *testing.T) {
	var buf bytes.Buffer
	seq := &callSeq{}
	boom := errors.New("count issues labeled \"factory-in-progress\": 502")
	gh := &fakeGitHub{seq: seq, countErr: boom}
	counter, _ := newTestCounter(t, seq)

	var err error
	captureLog(t, &buf, func() {
		err = pipeline.DryRun(context.Background(), testConfig(), gh, counter)
	})

	require.ErrorIs(t, err, boom, "a decision error must be returned unchanged")
	assert.Empty(t, gh.addLabelCalls, "a failed decision must never lock an issue")
}

// TestDryRunPropagatesClaimError triangulates the error path with the last
// read-only decision: a failed claim query returns its error and still never
// reaches the lock.
func TestDryRunPropagatesClaimError(t *testing.T) {
	var buf bytes.Buffer
	seq := &callSeq{}
	boom := errors.New("list claim candidates: 503")
	gh := &fakeGitHub{seq: seq, candidatesErr: boom}
	counter, _ := newTestCounter(t, seq)

	var err error
	captureLog(t, &buf, func() {
		err = pipeline.DryRun(context.Background(), testConfig(), gh, counter)
	})

	require.ErrorIs(t, err, boom, "a claim-query error must be returned unchanged")
	assert.Empty(t, gh.addLabelCalls, "a failed claim query must never lock an issue")
}
