package pipeline_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/Zheng5005/lights-out/internal/github"
	"github.com/Zheng5005/lights-out/internal/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMain silences the default logger for the whole package: the pipeline
// logs step transitions through slog.Default(), and the suite must neither
// pollute stdout nor assert on it. Tests that care about the log override the
// default themselves and restore it (see TestRunLogsStepTransitions).
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

// captureLog installs a default logger writing into buf for the duration of
// fn and restores the previous default afterwards.
func captureLog(t *testing.T, buf *bytes.Buffer, fn func()) {
	t.Helper()
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	defer slog.SetDefault(old)
	fn()
}

// TestRunLogsStepTransitions proves the step-transition logging of Phase 6 T1:
// every decision point in run/execute emits exactly one Info line on the
// default logger, with no signature or sequencing change. The silent exits
// keep returning nil — they just announce themselves now.
func TestRunLogsStepTransitions(t *testing.T) {
	t.Run("capacity miss logs gate, capacity, daily and candidate", func(t *testing.T) {
		var buf bytes.Buffer
		captureLog(t, &buf, func() {
			seq := &callSeq{}
			gh := &fakeGitHub{seq: seq, countResult: 1} // 1/2 — under the limit
			counter, _ := newTestCounter(t, seq)

			err := pipeline.Run(context.Background(), testConfig(), gh, nil, counter)
			require.NoError(t, err, "a silent exit still returns nil")
		})

		out := buf.String()
		assert.Contains(t, out, "gate=passed")
		assert.Contains(t, out, "capacity=ok (1/2)")
		assert.Contains(t, out, "daily=ok (0/2)")
		assert.Contains(t, out, "candidate=none")
	})

	t.Run("disabled gate logs only gate=disabled", func(t *testing.T) {
		var buf bytes.Buffer
		cfg := testConfig()
		cfg.Enabled = false
		captureLog(t, &buf, func() {
			seq := &callSeq{}
			gh := &fakeGitHub{seq: seq}
			counter, _ := newTestCounter(t, seq)

			err := pipeline.Run(context.Background(), cfg, gh, nil, counter)
			require.NoError(t, err)
		})

		out := buf.String()
		assert.Contains(t, out, "gate=disabled")
		assert.NotContains(t, out, "capacity=", "the disabled gate must stop before capacity")
	})

	t.Run("full cycle logs every transition through cleanup", func(t *testing.T) {
		var buf bytes.Buffer
		captureLog(t, &buf, func() {
			seq := &callSeq{}
			gh := &fakeGitHub{seq: seq, candidates: []github.Issue{{Number: 7, Title: "Fix the thing"}}}
			counter, _ := newTestCounter(t, seq)
			hd := newFakeHerdr(seq)
			exec := newDirtyExec(seq, 7)

			err := pipeline.RunWithRunner(context.Background(), testConfig(), gh, hd, counter, exec.run)
			require.NoError(t, err)
		})

		out := buf.String()
		assert.Contains(t, out, "gate=passed")
		assert.Contains(t, out, "capacity=ok (0/2)")
		assert.Contains(t, out, "daily=ok (0/2)")
		assert.Contains(t, out, "candidate=#7")
		assert.Contains(t, out, "lock=applied (#7)")
		assert.Contains(t, out, "worktree=ready (workspace ws-issue-7)")
		assert.Contains(t, out, "agent=started")
		assert.Contains(t, out, "agent=settled done")
		assert.Contains(t, out, "push=ok (branch factory/issue-7)")
		assert.Contains(t, out, "pr=opened (#7)")
		assert.Contains(t, out, "cleanup=ok (worktree removed, lock released)")
	})

	t.Run("failure path never logs cleanup=ok", func(t *testing.T) {
		var buf bytes.Buffer
		captureLog(t, &buf, func() {
			seq := &callSeq{}
			gh := &fakeGitHub{seq: seq, candidates: []github.Issue{{Number: 7, Title: "Fix the thing"}}}
			counter, _ := newTestCounter(t, seq)
			hd := newFakeHerdr(seq)
			hd.promptStatus = "blocked"
			hd.readOutput = "permission dialog waiting for approval"

			err := pipeline.RunWithRunner(context.Background(), testConfig(), gh, hd, counter, (&fakeExec{seq: seq}).run)
			require.ErrorContains(t, err, `settled "blocked"`)
		})

		out := buf.String()
		assert.Contains(t, out, "agent=settled blocked", "a settled agent reports its status token")
		assert.NotContains(t, out, "cleanup=ok", "the failure path reports via the error, not cleanup=ok")
	})
}
