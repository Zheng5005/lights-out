package pipeline_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Zheng5005/lights-out/internal/config"
	"github.com/Zheng5005/lights-out/internal/daily"
	"github.com/Zheng5005/lights-out/internal/github"
	"github.com/Zheng5005/lights-out/internal/herdr"
	"github.com/Zheng5005/lights-out/internal/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixedNow is the clock every counter in this file agrees on. A fixed instant
// keeps "today" deterministic: if the recording counter and the seeder
// disagreed on the UTC date, the counter would read as rolled over to 0.
var fixedNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

// callSeq records the interleaving of the pipeline's external calls so tests
// can assert order, not just presence. The fake GitHub client, the fake
// Herdr client, the injected exec runner, and the daily counter's injected
// clock all append to it.
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
	createPRErr   error
	commentErr    error

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
	return nil, g.createPRErr
}

func (g *fakeGitHub) CommentOnIssue(ctx context.Context, issueNum int, body string) error {
	g.seq.record("gh.CommentOnIssue")
	g.ctxs = append(g.ctxs, ctx)
	g.commentCalls = append(g.commentCalls, commentCall{issueNum: issueNum, body: body})
	return g.commentErr
}

// paneCall captures one SplitPane invocation and the pane id it returned.
type paneCall struct {
	paneID    string
	direction string
	cwd       string
	newPaneID string
}

// startCall captures one StartAgent invocation, including the extra args the
// pipeline forwarded after `--` (the {worktree}-expanded agent.args).
type startCall struct {
	name      string
	kind      string
	paneID    string
	extraArgs []string
}

// promptCall captures one PromptAgent invocation.
type promptCall struct {
	target  string
	text    string
	timeout time.Duration
}

// fakeHerdr is a hand-rolled recording fake for herdr.HerdrClient, built on
// the same conventions as fakeGitHub: every method records its name in seq,
// captures its arguments, and returns the canned result. panicOn lets a test
// inject a panic at a chosen step to prove the deferred cleanup recovers it.
type fakeHerdr struct {
	seq *callSeq

	// Canned results.
	probeErr     error
	info         herdr.WorktreeInfo
	createErr    error
	splitPaneID  string
	splitErr     error
	startErr     error
	promptStatus string
	promptErr    error
	readOutput   string
	readErr      error
	removeErr    error

	// panicOn names a method that panics when called instead of returning.
	panicOn string

	// Recorded observations, one field per HerdrClient method.
	probeCalls   int
	createCalls  int
	createCwd    string
	createBranch string
	createBase   string
	paneCalls    []paneCall
	startCalls   []startCall
	promptCalls  []promptCall
	readCalls    []string
	removeCalls  []string
	closeCalls   []string
	waitCalls    []string
	visibleCalls int
	sendKeys     int
	ctxs         []context.Context
}

var _ herdr.HerdrClient = (*fakeHerdr)(nil)

// newFakeHerdr returns a fake whose WorktreeInfo is complete — the shape the
// Phase 4 live smoke pinned — with a settling "done" agent, so each test
// overrides only what its scenario needs.
func newFakeHerdr(seq *callSeq) *fakeHerdr {
	return &fakeHerdr{
		seq: seq,
		info: herdr.WorktreeInfo{
			WorkspaceID: "ws-issue-7",
			TabID:       "tab-issue-7",
			RootPaneID:  "pane-root",
			Path:        "/repos/ship/worktrees/issue-7",
		},
		splitPaneID:  "pane-agent",
		promptStatus: "done",
		readOutput:   "agent transcript",
	}
}

func (h *fakeHerdr) Probe(ctx context.Context) error {
	h.seq.record("hd.Probe")
	h.ctxs = append(h.ctxs, ctx)
	h.probeCalls++
	return h.probeErr
}

func (h *fakeHerdr) CreateWorktree(ctx context.Context, cwd, branch, base string) (herdr.WorktreeInfo, error) {
	h.seq.record("hd.CreateWorktree")
	h.ctxs = append(h.ctxs, ctx)
	h.createCalls++
	h.createCwd, h.createBranch, h.createBase = cwd, branch, base
	return h.info, h.createErr
}

func (h *fakeHerdr) RemoveWorktree(ctx context.Context, workspaceID string) error {
	h.seq.record("hd.RemoveWorktree")
	h.ctxs = append(h.ctxs, ctx)
	h.removeCalls = append(h.removeCalls, workspaceID)
	return h.removeErr
}

func (h *fakeHerdr) CloseWorkspace(ctx context.Context, workspaceID string) error {
	h.seq.record("hd.CloseWorkspace")
	h.ctxs = append(h.ctxs, ctx)
	h.closeCalls = append(h.closeCalls, workspaceID)
	return nil
}

func (h *fakeHerdr) SplitPane(ctx context.Context, paneID, direction, cwd string) (string, error) {
	h.seq.record("hd.SplitPane")
	h.ctxs = append(h.ctxs, ctx)
	h.paneCalls = append(h.paneCalls, paneCall{
		paneID:    paneID,
		direction: direction,
		cwd:       cwd,
		newPaneID: h.splitPaneID,
	})
	return h.splitPaneID, h.splitErr
}

func (h *fakeHerdr) StartAgent(ctx context.Context, name, kind, paneID string, extraArgs ...string) error {
	h.seq.record("hd.StartAgent")
	h.ctxs = append(h.ctxs, ctx)
	h.startCalls = append(h.startCalls, startCall{name: name, kind: kind, paneID: paneID, extraArgs: extraArgs})
	if h.panicOn == "StartAgent" {
		panic("boom: StartAgent exploded")
	}
	return h.startErr
}

func (h *fakeHerdr) PromptAgent(ctx context.Context, target, text string, timeout time.Duration) (string, error) {
	h.seq.record("hd.PromptAgent")
	h.ctxs = append(h.ctxs, ctx)
	h.promptCalls = append(h.promptCalls, promptCall{target: target, text: text, timeout: timeout})
	return h.promptStatus, h.promptErr
}

func (h *fakeHerdr) WaitAgent(ctx context.Context, target string, until []string, timeout time.Duration) (string, error) {
	h.seq.record("hd.WaitAgent")
	h.ctxs = append(h.ctxs, ctx)
	h.waitCalls = append(h.waitCalls, target)
	return h.promptStatus, nil
}

func (h *fakeHerdr) ReadAgent(ctx context.Context, target string) (string, error) {
	h.seq.record("hd.ReadAgent")
	h.ctxs = append(h.ctxs, ctx)
	h.readCalls = append(h.readCalls, target)
	return h.readOutput, h.readErr
}

func (h *fakeHerdr) ReadAgentVisible(ctx context.Context, target string) (string, error) {
	h.seq.record("hd.ReadAgentVisible")
	h.ctxs = append(h.ctxs, ctx)
	h.visibleCalls++
	return h.readOutput, nil
}

func (h *fakeHerdr) SendKeys(ctx context.Context, target string, keys ...string) error {
	h.seq.record("hd.SendKeys")
	h.ctxs = append(h.ctxs, ctx)
	h.sendKeys++
	return nil
}

// execCall captures one injected exec invocation: the directory the command
// ran in, the binary, its arguments, and the context it received.
type execCall struct {
	dir  string
	name string
	args []string
	ctx  context.Context
}

// execResponse is the canned reply for one exec invocation.
type execResponse struct {
	output []byte
	err    error
}

// fakeExec is the injected execFunc for RunWithRunner: it records every git
// command the pipeline would have run and replies with the response scripted
// for that exact command line, so no test ever touches a real binary. A
// command with no scripted response fails loudly: an unscripted call means a
// code path changed without its test, never a silent success.
type fakeExec struct {
	seq       *callSeq
	responses map[string]execResponse
	calls     []execCall
}

// respond scripts the reply for one command line (e.g. "git status
// --porcelain") and returns the fake for chaining. output is returned as
// combined output on success; err, when non-nil, makes the command fail.
func (e *fakeExec) respond(command, output string, err error) *fakeExec {
	if e.responses == nil {
		e.responses = map[string]execResponse{}
	}
	e.responses[command] = execResponse{output: []byte(output), err: err}
	return e
}

func (e *fakeExec) run(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	event := name
	if len(args) > 0 {
		event += "." + args[0]
	}
	e.seq.record(event)
	e.calls = append(e.calls, execCall{dir: dir, name: name, args: args, ctx: ctx})
	command := name + " " + strings.Join(args, " ")
	resp, ok := e.responses[command]
	if !ok {
		return nil, fmt.Errorf("fakeExec: unexpected command %q", command)
	}
	return resp.output, resp.err
}

// newDirtyExec scripts the realistic live path for issueNum: the agent left
// the worktree dirty, so the pipeline stages, commits, and pushes.
func newDirtyExec(seq *callSeq, issueNum int) *fakeExec {
	return (&fakeExec{seq: seq}).
		respond("git status --porcelain", " M internal/foo.go\n", nil).
		respond("git add -A", "", nil).
		respond(fmt.Sprintf("git commit -m chore: apply changes for issue #%d", issueNum), "", nil).
		respond(fmt.Sprintf("git push origin factory/issue-%d", issueNum), "", nil)
}

// testConfig returns a valid factory configuration with both limits at 2 and
// the worktree/agent blocks a run to completion needs.
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
		Agent: config.Agent{
			Dispatcher: "herdr",
			Mode:       "headless",
			Kind:       "agy",
			Timeout:    10 * time.Minute,
			Session:    "default",
		},
		Worktree: config.Worktree{
			Base: "main",
			Path: "/repos/ship",
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

// TestRunHappyPathRunsFullCycleLockingBeforeCharging is the PRD §4 success
// path end to end: capacity checks, the oldest candidate claimed, the lock
// applied BEFORE the daily charge, then probe → worktree → agent → prompt →
// push → PR → cleanup. The event list is compared in full, so any reordering
// — a push before the agent settles, a PR before the push, the charge before
// the lock — fails the test.
func TestRunHappyPathRunsFullCycleLockingBeforeCharging(t *testing.T) {
	seq := &callSeq{}
	oldest := github.Issue{Number: 7, Title: "Fix the thing", Body: "It is broken."}
	newer := github.Issue{Number: 42, Title: "newer"}
	gh := &fakeGitHub{
		seq: seq,
		// Candidates arrive oldest first per the github.Client contract.
		candidates: []github.Issue{oldest, newer},
	}
	counter, path := newTestCounter(t, seq)
	cfg := testConfig()
	hd := newFakeHerdr(seq)
	exec := newDirtyExec(seq, 7)
	ctx := context.Background()

	err := pipeline.RunWithRunner(ctx, cfg, gh, hd, counter, exec.run)
	require.NoError(t, err)

	want := []string{
		"gh.CountIssuesByLabel",
		"daily.clock", // daily.Current
		"gh.ListClaimCandidates",
		"gh.AddLabel", // FR4 — the lock...
		"daily.clock", // ...precedes daily.Increment (the charge)
		"hd.Probe",    // step 5 — fail fast first
		"hd.CreateWorktree",
		"hd.SplitPane",
		"hd.StartAgent",
		"hd.PromptAgent", // alone: it already waits (--wait)
		"git.status",     // stage and commit the agent's work...
		"git.add",
		"git.commit",
		"git.push",          // ...then step 5b — in the worktree, before any PR
		"gh.CreatePR",       // step 6
		"gh.RemoveLabel",    // step 6 — release the lock after the PR opened
		"hd.RemoveWorktree", // FR4 defer — every exit path
		"gh.RemoveLabel",    // FR4 defer — idempotent second release
	}
	require.Equal(t, want, seq.events, "the full §4 cycle must run in exactly this order")

	// The claim: the OLDEST candidate, locked with the in-progress label.
	require.Len(t, gh.addLabelCalls, 1)
	assert.Equal(t,
		labelCall{issueNum: oldest.Number, label: cfg.Labels.InProgress},
		gh.addLabelCalls[0],
		"the oldest candidate must be locked with the in-progress label")
	assert.Equal(t, []string{cfg.Labels.InProgress}, gh.countLabels,
		"capacity must be counted against the in-progress label")
	assert.Equal(t, 1, gh.listCalls)

	// Step 5 — worktree and agent wiring, exactly as the decisions require.
	assert.Equal(t, "/repos/ship", hd.createCwd, "worktree must be created from the configured clone")
	assert.Equal(t, "factory/issue-7", hd.createBranch, "branch name is factory/issue-<N>")
	assert.Equal(t, "main", hd.createBase, "base comes from cfg.Worktree.Base")
	require.Len(t, hd.paneCalls, 1)
	assert.Equal(t,
		paneCall{paneID: "pane-root", direction: "down", cwd: "/repos/ship/worktrees/issue-7", newPaneID: "pane-agent"},
		hd.paneCalls[0],
		"the agent pane must be split down from the root pane with the worktree as cwd")
	require.Len(t, hd.startCalls, 1)
	assert.Equal(t,
		startCall{name: "factory-issue-7", kind: "agy", paneID: "pane-agent"},
		hd.startCalls[0],
		"agent name must be unique per run and derived from the issue number")
	require.Len(t, hd.promptCalls, 1)
	assert.Equal(t,
		promptCall{target: "factory-issue-7", text: "Fix the thing\n\nIt is broken.", timeout: cfg.Agent.Timeout},
		hd.promptCalls[0],
		"the prompt must be the issue title and body, sent with the configured timeout")
	assert.Empty(t, hd.waitCalls, "PromptAgent already waits; WaitAgent would be redundant")
	assert.Empty(t, hd.readCalls, "a settled-done run needs no diagnostic read")

	// Step 5b — the commit of the agent's dirty work, then the push: real git
	// semantics, in the worktree, one branch.
	require.Len(t, exec.calls, 4)
	assert.Equal(t,
		execCall{ctx: ctx, dir: "/repos/ship/worktrees/issue-7", name: "git", args: []string{"status", "--porcelain"}},
		exec.calls[0],
		"the agent's worktree must be inspected for changes first")
	assert.Equal(t,
		execCall{ctx: ctx, dir: "/repos/ship/worktrees/issue-7", name: "git", args: []string{"add", "-A"}},
		exec.calls[1],
		"a dirty worktree must be staged before committing")
	assert.Equal(t,
		execCall{ctx: ctx, dir: "/repos/ship/worktrees/issue-7", name: "git", args: []string{"commit", "-m", "chore: apply changes for issue #7"}},
		exec.calls[2],
		"the agent's changes must be committed with a conventional, attribution-free message")
	assert.Equal(t,
		execCall{ctx: ctx, dir: "/repos/ship/worktrees/issue-7", name: "git", args: []string{"push", "origin", "factory/issue-7"}},
		exec.calls[3],
		"git push must run in the worktree after the commit and push the issue branch")

	// Step 6 — the PR carries the closing keyword that closes the issue on
	// merge (FR5: the factory itself never merges).
	require.Len(t, gh.createPRCalls, 1)
	assert.Equal(t,
		github.PullRequestSpec{
			Head:  "factory/issue-7",
			Base:  "main",
			Title: "Fix the thing",
			Body:  "It is broken.\n\nCloses #7",
		},
		gh.createPRCalls[0],
		"the PR must open from the issue branch onto the base with Closes #N in the body")

	// Terminal state: lock released, NOT blocked, NO comment (FR6 comments
	// only failures), worktree removed.
	assert.Contains(t, gh.removeCalls, labelCall{issueNum: 7, label: cfg.Labels.InProgress},
		"in_progress must be removed on the success path")
	assert.Equal(t,
		[]labelCall{{issueNum: 7, label: cfg.Labels.InProgress}},
		gh.addLabelCalls,
		"the success path must never apply the blocked label")
	assert.Empty(t, gh.commentCalls, "a successful run posts no comment (FR6)")
	assert.Equal(t, []string{"ws-issue-7"}, hd.removeCalls,
		"the worktree must be removed on the success path")
	assert.Empty(t, hd.closeCalls, "cleanup uses RemoveWorktree, which closes the workspace; CloseWorkspace must not be called")

	// Every external call receives the caller's context.
	for _, got := range gh.ctxs {
		assert.Equal(t, ctx, got, "every GitHub call must receive the caller's context")
	}
	for _, got := range hd.ctxs {
		assert.Equal(t, ctx, got, "every Herdr call must receive the caller's context")
	}

	// Read through a non-recording clock, after the sequence assertion:
	// exactly one claim was charged, i.e. Increment ran exactly once.
	assert.Equal(t, 1, currentDaily(t, path))
}

// TestRunPassesAgentArgsWithWorktreeExpansion proves the trust-dialog fix: the
// configured agent.args are forwarded to StartAgent with the {worktree} token
// replaced by the run's absolute worktree path, so agy's --add-dir
// pre-authorizes the fresh worktree and --dangerously-skip-permissions
// auto-approves tool permissions for an unmanned run.
func TestRunPassesAgentArgsWithWorktreeExpansion(t *testing.T) {
	seq := &callSeq{}
	gh := &fakeGitHub{seq: seq, candidates: []github.Issue{{Number: 7, Title: "Fix the thing"}}}
	counter, _ := newTestCounter(t, seq)
	cfg := testConfig()
	cfg.Agent.Args = []string{"--add-dir", "{worktree}", "--dangerously-skip-permissions"}
	hd := newFakeHerdr(seq)
	exec := newDirtyExec(seq, 7)

	err := pipeline.RunWithRunner(context.Background(), cfg, gh, hd, counter, exec.run)
	require.NoError(t, err)

	require.Len(t, hd.startCalls, 1)
	assert.Equal(t,
		[]string{"--add-dir", "/repos/ship/worktrees/issue-7", "--dangerously-skip-permissions"},
		hd.startCalls[0].extraArgs,
		"StartAgent must receive the configured args with {worktree} expanded to the run's absolute worktree path")
}

// TestRunCleanWorktreeWithAgentCommitPushes covers the other commit branch: the
// agent already committed its own work, so the tree is clean and HEAD differs
// from the base ref. The pipeline must skip add/commit and push directly.
func TestRunCleanWorktreeWithAgentCommitPushes(t *testing.T) {
	seq := &callSeq{}
	gh := &fakeGitHub{seq: seq, candidates: []github.Issue{{Number: 7, Title: "Fix the thing"}}}
	counter, _ := newTestCounter(t, seq)
	cfg := testConfig()
	hd := newFakeHerdr(seq)
	exec := (&fakeExec{seq: seq}).
		respond("git status --porcelain", "", nil).
		respond("git rev-parse HEAD", "abc123\n", nil).
		respond("git rev-parse main", "def456\n", nil).
		respond("git push origin factory/issue-7", "", nil)

	err := pipeline.RunWithRunner(context.Background(), cfg, gh, hd, counter, exec.run)
	require.NoError(t, err)

	// status, rev-parse HEAD, rev-parse base, push — with no add/commit.
	require.Len(t, exec.calls, 4)
	for _, call := range exec.calls {
		assert.NotEqual(t, "add", call.args[0], "a clean tree must not be staged")
		assert.NotEqual(t, "commit", call.args[0], "a clean tree must not be committed again")
	}
	last := exec.calls[len(exec.calls)-1]
	assert.Equal(t, "factory/issue-7", last.args[len(last.args)-1], "the agent's own commit must still be pushed")
	require.Len(t, gh.createPRCalls, 1, "a committed agent run opens the PR")
	require.Empty(t, gh.commentCalls, "a successful run posts no comment (FR6)")
}

// TestRunCleanWorktreeAtBaseFailsWithoutPush covers the empty-run guard: the
// agent settled `done`, but the tree is clean and HEAD equals the base ref, so
// there is nothing to push. The run must fail loudly, never push an empty
// branch and never open a PR.
func TestRunCleanWorktreeAtBaseFailsWithoutPush(t *testing.T) {
	seq := &callSeq{}
	gh := &fakeGitHub{seq: seq, candidates: []github.Issue{{Number: 7, Title: "Fix the thing"}}}
	counter, _ := newTestCounter(t, seq)
	cfg := testConfig()
	hd := newFakeHerdr(seq)
	exec := (&fakeExec{seq: seq}).
		respond("git status --porcelain", "", nil).
		respond("git rev-parse HEAD", "abc123\n", nil).
		respond("git rev-parse main", "abc123\n", nil)

	err := pipeline.RunWithRunner(context.Background(), cfg, gh, hd, counter, exec.run)
	require.ErrorContains(t, err, "commit agent work")
	require.ErrorContains(t, err, "no changes", "the error must explain the empty worktree")

	require.Len(t, exec.calls, 3, "status and both rev-parse calls only")
	for _, call := range exec.calls {
		assert.NotEqual(t, "push", call.args[0], "an empty branch must never be pushed")
	}
	assert.Empty(t, gh.createPRCalls, "an empty run must never open a PR")

	require.Len(t, gh.commentCalls, 1, "FR6: exactly one comment on the failure path")
	assert.Contains(t, gh.commentCalls[0].body, "no changes")
	require.Len(t, gh.addLabelCalls, 2, "lock then blocked")
	assert.Equal(t, labelCall{issueNum: 7, label: cfg.Labels.Blocked}, gh.addLabelCalls[1])
	assert.Contains(t, gh.removeCalls, labelCall{issueNum: 7, label: cfg.Labels.InProgress},
		"FR4: the lock must be released")
	assert.Equal(t, []string{"ws-issue-7"}, hd.removeCalls, "the worktree must be removed on the failure path")
}

// TestRunProbeDownRoutesToErrorPathWithoutWorktree is the plan Phase 4
// fail-fast: a down Herdr server must reach the error path as an error, not a
// panic, and nothing downstream may run — no worktree, no push, no PR. The
// deferred cleanup still posts exactly one comment, replaces the lock with
// blocked, and returns the probe error.
func TestRunProbeDownRoutesToErrorPathWithoutWorktree(t *testing.T) {
	seq := &callSeq{}
	gh := &fakeGitHub{seq: seq, candidates: []github.Issue{{Number: 7, Title: "Fix the thing"}}}
	counter, _ := newTestCounter(t, seq)
	cfg := testConfig()
	hd := newFakeHerdr(seq)
	probeErr := fmt.Errorf("herdr session %q is not running: %w", "default", herdr.ErrServerUnavailable)
	hd.probeErr = probeErr
	exec := &fakeExec{seq: seq}

	err := pipeline.RunWithRunner(context.Background(), cfg, gh, hd, counter, exec.run)
	require.ErrorIs(t, err, probeErr, "the probe error must be returned")

	want := []string{
		"gh.CountIssuesByLabel",
		"daily.clock",
		"gh.ListClaimCandidates",
		"gh.AddLabel",
		"daily.clock",
		"hd.Probe",
		"gh.CommentOnIssue", // exactly one failure comment (FR6)
		"gh.RemoveLabel",    // release the lock
		"gh.AddLabel",       // replace it with blocked (step 7)
	}
	require.Equal(t, want, seq.events)

	assert.Zero(t, hd.createCalls, "a down server must fail before any worktree exists")
	assert.Empty(t, hd.removeCalls, "nothing was created, so nothing may be removed")
	assert.Empty(t, exec.calls, "no push may happen when the server is down")
	assert.Empty(t, gh.createPRCalls, "the error path must never open a PR")

	require.Len(t, gh.commentCalls, 1, "FR6: exactly one comment")
	assert.Equal(t, 7, gh.commentCalls[0].issueNum)
	assert.Contains(t, gh.commentCalls[0].body, "herdr probe", "the comment must explain what happened")

	require.Len(t, gh.addLabelCalls, 2, "lock then blocked")
	assert.Equal(t, labelCall{issueNum: 7, label: cfg.Labels.InProgress}, gh.addLabelCalls[0])
	assert.Equal(t, labelCall{issueNum: 7, label: cfg.Labels.Blocked}, gh.addLabelCalls[1],
		"the error path must swap in_progress for blocked")
	assert.Contains(t, gh.removeCalls, labelCall{issueNum: 7, label: cfg.Labels.InProgress},
		"FR4: the lock must be released on the error path")
}

// TestRunIncompleteWorktreeInfoRoutesToErrorPath covers decision 3: the
// Phase 4 live smoke pins Path, RootPaneID, and WorkspaceID as present, so an
// empty field means extraction failed and the run must stop BEFORE any agent
// work — while a worktree that does have a workspace id is still torn down.
func TestRunIncompleteWorktreeInfoRoutesToErrorPath(t *testing.T) {
	tests := []struct {
		name string
		info herdr.WorktreeInfo
		// missing names the field the error must call out.
		missing string
		// wantRemove is false only when WorkspaceID itself is the missing
		// field: there is then no handle to remove the worktree with.
		wantRemove bool
	}{
		{
			name:       "empty Path",
			info:       herdr.WorktreeInfo{WorkspaceID: "ws-issue-7", RootPaneID: "pane-root"},
			missing:    "Path",
			wantRemove: true,
		},
		{
			name:       "empty RootPaneID",
			info:       herdr.WorktreeInfo{WorkspaceID: "ws-issue-7", Path: "/repos/ship/worktrees/issue-7"},
			missing:    "RootPaneID",
			wantRemove: true,
		},
		{
			name:       "empty WorkspaceID",
			info:       herdr.WorktreeInfo{Path: "/repos/ship/worktrees/issue-7", RootPaneID: "pane-root"},
			missing:    "WorkspaceID",
			wantRemove: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seq := &callSeq{}
			gh := &fakeGitHub{seq: seq, candidates: []github.Issue{{Number: 7, Title: "Fix the thing"}}}
			counter, _ := newTestCounter(t, seq)
			cfg := testConfig()
			hd := newFakeHerdr(seq)
			hd.info = tt.info
			exec := &fakeExec{seq: seq}

			err := pipeline.RunWithRunner(context.Background(), cfg, gh, hd, counter, exec.run)
			require.ErrorContains(t, err, "create worktree")
			require.ErrorContains(t, err, tt.missing, "the error must name the missing field")

			assert.Equal(t, 1, hd.probeCalls, "the probe runs exactly once")
			assert.Empty(t, hd.paneCalls, "no pane may be split from an invalid worktree")
			assert.Empty(t, hd.startCalls, "no agent may start without a valid worktree")
			assert.Empty(t, hd.promptCalls, "no prompt may be sent without an agent")
			assert.Empty(t, exec.calls, "no push may happen")
			assert.Empty(t, gh.createPRCalls, "no PR may be opened")

			if tt.wantRemove {
				assert.Equal(t, []string{"ws-issue-7"}, hd.removeCalls,
					"a partially extracted worktree must still be removed")
			} else {
				assert.Empty(t, hd.removeCalls, "no workspace id means no removal handle")
			}

			require.Len(t, gh.commentCalls, 1, "FR6: exactly one comment")
			assert.Contains(t, gh.commentCalls[0].body, tt.missing)
			require.Len(t, gh.addLabelCalls, 2, "lock then blocked")
			assert.Equal(t, labelCall{issueNum: 7, label: cfg.Labels.Blocked}, gh.addLabelCalls[1])
			assert.Contains(t, gh.removeCalls, labelCall{issueNum: 7, label: cfg.Labels.InProgress},
				"FR4: the lock must be released")
		})
	}
}

// TestRunAgentSettledBlockedPostsAgentOutputInComment is the non-done settle
// of decision 1: headless reports "done", never "idle", and anything else is
// a failure whose comment carries the ReadAgent diagnostic body (§4 step 5).
func TestRunAgentSettledBlockedPostsAgentOutputInComment(t *testing.T) {
	seq := &callSeq{}
	gh := &fakeGitHub{seq: seq, candidates: []github.Issue{{Number: 7, Title: "Fix the thing"}}}
	counter, _ := newTestCounter(t, seq)
	cfg := testConfig()
	hd := newFakeHerdr(seq)
	hd.promptStatus = "blocked"
	hd.readOutput = "permission dialog waiting for approval"
	exec := &fakeExec{seq: seq}

	err := pipeline.RunWithRunner(context.Background(), cfg, gh, hd, counter, exec.run)
	require.ErrorContains(t, err, `agent factory-issue-7 settled "blocked"`)
	require.ErrorContains(t, err, `"done"`)

	require.Len(t, gh.commentCalls, 1, "FR6: exactly one comment")
	body := gh.commentCalls[0].body
	assert.Contains(t, body, "permission dialog waiting for approval",
		"the comment must carry the ReadAgent diagnostic output")
	assert.Contains(t, body, `settled "blocked"`, "the comment must explain why the run stopped")
	require.Len(t, hd.readCalls, 1)
	assert.Equal(t, "factory-issue-7", hd.readCalls[0])

	assert.Empty(t, exec.calls, "a blocked agent must never reach the push")
	assert.Empty(t, gh.createPRCalls, "the error path must never open a PR")
	assert.Empty(t, hd.waitCalls, "PromptAgent already waits; WaitAgent would be redundant")

	assert.Equal(t, []string{"ws-issue-7"}, hd.removeCalls,
		"the worktree must be removed on the failure path")
	require.Len(t, gh.addLabelCalls, 2, "lock then blocked")
	assert.Equal(t, labelCall{issueNum: 7, label: cfg.Labels.Blocked}, gh.addLabelCalls[1])
	assert.Contains(t, gh.removeCalls, labelCall{issueNum: 7, label: cfg.Labels.InProgress},
		"FR4: the lock must be released")
}

// TestRunGitPushFailureRoutesToErrorPath proves step 5b: a failed push is a
// run failure — the error path comments and blocks, and step 6 never opens a
// PR from a branch that is not on the remote.
func TestRunGitPushFailureRoutesToErrorPath(t *testing.T) {
	seq := &callSeq{}
	gh := &fakeGitHub{seq: seq, candidates: []github.Issue{{Number: 7, Title: "Fix the thing"}}}
	counter, _ := newTestCounter(t, seq)
	cfg := testConfig()
	hd := newFakeHerdr(seq)
	exec := newDirtyExec(seq, 7).
		respond("git push origin factory/issue-7", "remote: rejected", errors.New("exit status 1"))

	err := pipeline.RunWithRunner(context.Background(), cfg, gh, hd, counter, exec.run)
	require.ErrorContains(t, err, "git push factory/issue-7")
	require.ErrorContains(t, err, "remote: rejected", "git's output must reach the returned error")

	require.Len(t, exec.calls, 4, "status, add, commit, then the single push")
	last := exec.calls[len(exec.calls)-1]
	assert.Equal(t, "factory/issue-7", last.args[len(last.args)-1])
	assert.Empty(t, gh.createPRCalls, "a failed push must never reach CreatePR")

	require.Len(t, gh.commentCalls, 1, "FR6: exactly one comment")
	assert.Contains(t, gh.commentCalls[0].body, "git push")
	require.Len(t, gh.addLabelCalls, 2, "lock then blocked")
	assert.Equal(t, labelCall{issueNum: 7, label: cfg.Labels.Blocked}, gh.addLabelCalls[1])
	assert.Contains(t, gh.removeCalls, labelCall{issueNum: 7, label: cfg.Labels.InProgress})
	assert.Equal(t, []string{"ws-issue-7"}, hd.removeCalls,
		"the worktree must be removed on the failure path")
}

// TestRunCreatePRFailureRoutesToErrorPath proves step 6's failure mode: the
// PR error reaches the returned error and the error path, with the push
// already done — comment once, block, release, remove the worktree.
func TestRunCreatePRFailureRoutesToErrorPath(t *testing.T) {
	seq := &callSeq{}
	boom := errors.New("create pull request: 502")
	gh := &fakeGitHub{
		seq:         seq,
		candidates:  []github.Issue{{Number: 7, Title: "Fix the thing"}},
		createPRErr: boom,
	}
	counter, _ := newTestCounter(t, seq)
	cfg := testConfig()
	hd := newFakeHerdr(seq)
	exec := newDirtyExec(seq, 7)

	err := pipeline.RunWithRunner(context.Background(), cfg, gh, hd, counter, exec.run)
	require.ErrorIs(t, err, boom, "the CreatePR error must be returned unchanged")

	require.Len(t, exec.calls, 4, "status, add, commit, and the push happen before the PR attempt")
	require.Len(t, gh.createPRCalls, 1)
	require.Len(t, gh.commentCalls, 1, "FR6: exactly one comment")
	assert.Contains(t, gh.commentCalls[0].body, "create pull request")
	require.Len(t, gh.addLabelCalls, 2, "lock then blocked")
	assert.Equal(t, labelCall{issueNum: 7, label: cfg.Labels.Blocked}, gh.addLabelCalls[1])
	assert.Contains(t, gh.removeCalls, labelCall{issueNum: 7, label: cfg.Labels.InProgress})
	assert.Equal(t, []string{"ws-issue-7"}, hd.removeCalls,
		"the worktree must be removed on the failure path")
}

// TestRunPanicDuringExecuteIsRecoveredWithFullCleanup is the FR4 crash proof:
// a panic anywhere inside execute must not leak a worktree or strand the
// lock. The deferred function recovers it, runs the full cleanup, and the run
// surfaces the panic as an ordinary error.
func TestRunPanicDuringExecuteIsRecoveredWithFullCleanup(t *testing.T) {
	seq := &callSeq{}
	gh := &fakeGitHub{seq: seq, candidates: []github.Issue{{Number: 7, Title: "Fix the thing"}}}
	counter, _ := newTestCounter(t, seq)
	cfg := testConfig()
	hd := newFakeHerdr(seq)
	hd.panicOn = "StartAgent"
	exec := &fakeExec{seq: seq}

	err := pipeline.RunWithRunner(context.Background(), cfg, gh, hd, counter, exec.run)
	require.ErrorContains(t, err, "panic", "a recovered panic must surface as an error")
	require.ErrorContains(t, err, "boom: StartAgent exploded")

	assert.Equal(t, []string{"ws-issue-7"}, hd.removeCalls,
		"a panic must not leak the worktree")
	assert.Empty(t, hd.closeCalls, "cleanup uses RemoveWorktree, never CloseWorkspace")

	require.Len(t, gh.commentCalls, 1, "FR6: a panic is a failure outcome, commented exactly once")
	require.Len(t, gh.addLabelCalls, 2, "lock then blocked")
	assert.Equal(t, labelCall{issueNum: 7, label: cfg.Labels.Blocked}, gh.addLabelCalls[1])
	assert.Contains(t, gh.removeCalls, labelCall{issueNum: 7, label: cfg.Labels.InProgress},
		"FR4: a panic must not strand the lock")
	assert.Empty(t, gh.createPRCalls, "a panic must never open a PR")
	assert.Empty(t, exec.calls, "the panic happens before the push")
}

// TestRunCleanupFailureAfterFailurePostsExactlyOneComment is the FR6 hard
// case: the agent fails, THEN worktree removal fails during cleanup. The
// original error must survive unmasksed, and the run must still produce
// exactly one comment — never one per failure.
func TestRunCleanupFailureAfterFailurePostsExactlyOneComment(t *testing.T) {
	seq := &callSeq{}
	gh := &fakeGitHub{seq: seq, candidates: []github.Issue{{Number: 7, Title: "Fix the thing"}}}
	counter, _ := newTestCounter(t, seq)
	cfg := testConfig()
	hd := newFakeHerdr(seq)
	hd.promptStatus = "blocked"
	hd.readOutput = "agent stopped on a dialog"
	hd.removeErr = errors.New("workspace_not_found")
	exec := &fakeExec{seq: seq}

	err := pipeline.RunWithRunner(context.Background(), cfg, gh, hd, counter, exec.run)
	require.ErrorContains(t, err, `settled "blocked"`, "the original failure must be returned")
	require.NotErrorIs(t, err, hd.removeErr, "a cleanup error must never mask the original error")

	require.Len(t, gh.commentCalls, 1, "FR6: exactly one comment despite two failures")
	assert.Contains(t, gh.commentCalls[0].body, "agent stopped on a dialog",
		"the comment must describe the original failure, not the cleanup")
	assert.Equal(t, []string{"ws-issue-7"}, hd.removeCalls, "the removal is attempted once")
	require.Len(t, gh.addLabelCalls, 2, "lock then blocked")
	assert.Equal(t, labelCall{issueNum: 7, label: cfg.Labels.Blocked}, gh.addLabelCalls[1])
	assert.Contains(t, gh.removeCalls, labelCall{issueNum: 7, label: cfg.Labels.InProgress})
}

// TestRunWorktreeRemovalFailureAfterSuccessReturnsError pins the other half
// of the cleanup-error rule: with no earlier error, the cleanup failure IS
// the returned error. The run then counts as failed, so FR6 applies — one
// comment explaining the removal failure, and the issue is blocked rather
// than left claimable with a stray worktree behind it.
func TestRunWorktreeRemovalFailureAfterSuccessReturnsError(t *testing.T) {
	seq := &callSeq{}
	gh := &fakeGitHub{seq: seq, candidates: []github.Issue{{Number: 7, Title: "Fix the thing"}}}
	counter, _ := newTestCounter(t, seq)
	cfg := testConfig()
	hd := newFakeHerdr(seq)
	hd.removeErr = errors.New("worktree remove failed: exit status 1")
	exec := newDirtyExec(seq, 7)

	err := pipeline.RunWithRunner(context.Background(), cfg, gh, hd, counter, exec.run)
	require.ErrorContains(t, err, "cleanup: remove worktree",
		"with no original error the cleanup error becomes the returned error")

	require.Len(t, gh.createPRCalls, 1, "the PR was opened before the cleanup ran")
	require.Len(t, gh.commentCalls, 1, "FR6: the failed run is commented exactly once")
	assert.Contains(t, gh.commentCalls[0].body, "cleanup: remove worktree")
	require.Len(t, gh.addLabelCalls, 2, "lock then blocked")
	assert.Equal(t, labelCall{issueNum: 7, label: cfg.Labels.Blocked}, gh.addLabelCalls[1])
}

// TestRunCommentFailureStillReturnsOriginalError closes the FR6 sequence:
// when the comment POST itself fails, the attempt is never retried (the
// posted flag is set before the attempt) and the comment error never masks
// the failure that triggered it.
func TestRunCommentFailureStillReturnsOriginalError(t *testing.T) {
	seq := &callSeq{}
	gh := &fakeGitHub{
		seq:        seq,
		candidates: []github.Issue{{Number: 7, Title: "Fix the thing"}},
		commentErr: errors.New("403 forbidden"),
	}
	counter, _ := newTestCounter(t, seq)
	cfg := testConfig()
	hd := newFakeHerdr(seq)
	hd.promptStatus = "blocked"
	exec := &fakeExec{seq: seq}

	err := pipeline.RunWithRunner(context.Background(), cfg, gh, hd, counter, exec.run)
	require.ErrorContains(t, err, `settled "blocked"`, "the comment error must not replace the original error")

	require.Len(t, gh.commentCalls, 1, "FR6: a failed comment attempt is never retried")
	require.Len(t, gh.addLabelCalls, 2, "the block swap still runs after a failed comment")
	assert.Equal(t, labelCall{issueNum: 7, label: cfg.Labels.Blocked}, gh.addLabelCalls[1])
	assert.Contains(t, gh.removeCalls, labelCall{issueNum: 7, label: cfg.Labels.InProgress})
	assert.Equal(t, []string{"ws-issue-7"}, hd.removeCalls)
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
