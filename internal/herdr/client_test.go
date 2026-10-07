package herdr_test

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Zheng5005/lights-out/internal/herdr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptedCall is the canned reply for one herdr subcommand.
type scriptedCall struct {
	stdout string
	stderr string
	err    error
}

// fakeRunner stands in for the herdr binary: it records every argv it
// receives and replies with the script keyed by the two-word subcommand
// (args[2] and args[3]). Subcommands without a script succeed with an empty
// result object.
type fakeRunner struct {
	calls  [][]string
	script map[string]scriptedCall
}

func newFakeRunner(script map[string]scriptedCall) *fakeRunner {
	return &fakeRunner{script: script}
}

func (f *fakeRunner) run(_ context.Context, args ...string) ([]byte, []byte, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	key := ""
	if len(args) >= 4 {
		key = args[2] + " " + args[3]
	}
	sc, ok := f.script[key]
	if !ok {
		return []byte(`{"id":"cli:test","result":{}}`), nil, nil
	}
	return []byte(sc.stdout), []byte(sc.stderr), sc.err
}

// requireArgvShape asserts the bare-herdr guard on every recorded call: at
// least three arguments, "--session" first, the session value second, and a
// real subcommand word third — never a bare `herdr`, a target, or a flag.
func requireArgvShape(t *testing.T, calls [][]string) {
	t.Helper()
	require.NotEmpty(t, calls)
	for i, args := range calls {
		require.GreaterOrEqualf(t, len(args), 3, "call %d has too few args: %v", i, args)
		assert.Equal(t, "--session", args[0], "call %d must start with --session: %v", i, args)
		switch args[2] {
		case "worktree", "workspace", "pane", "agent":
		default:
			t.Errorf("call %d: args[2] = %q is not a subcommand word: %v", i, args[2], args)
		}
	}
}

// mustExitError returns a real *exec.ExitError, obtained from `sh -c "exit 1"`
// so the fake can reproduce a herdr process failure without running herdr.
func mustExitError(t *testing.T) *exec.ExitError {
	t.Helper()
	err := exec.Command("sh", "-c", "exit 1").Run()
	require.Error(t, err)
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	return exitErr
}

// runAllMethods drives every HerdrClient method once against a fake and
// returns the fake with all recorded argv.
func runAllMethods(t *testing.T, session string) *fakeRunner {
	t.Helper()
	runner := newFakeRunner(map[string]scriptedCall{
		"pane split":   {stdout: `{"result":{"pane_id":"pane:new"}}`},
		"agent prompt": {stdout: `{"result":{"status":"done"}}`},
		"agent wait":   {stdout: `{"result":{"status":"done"}}`},
		"agent read":   {stdout: `{"result":{"content":"output"}}`},
	})
	c := herdr.NewWithRunner(session, runner.run)
	ctx := context.Background()

	_, err := c.CreateWorktree(ctx, "/w/repo", "feat/x", "main")
	require.NoError(t, err)
	require.NoError(t, c.RemoveWorktree(ctx, "wt:1"))
	require.NoError(t, c.CloseWorkspace(ctx, "ws:1"))
	_, err = c.SplitPane(ctx, "pane:root", "right", "/w/repo")
	require.NoError(t, err)
	require.NoError(t, c.StartAgent(ctx, "coder", "agy", "pane:2"))
	_, err = c.PromptAgent(ctx, "agent:1", "continue", 90*time.Second)
	require.NoError(t, err)
	_, err = c.WaitAgent(ctx, "agent:1", []string{"working", "done"}, 30*time.Second)
	require.NoError(t, err)
	_, err = c.ReadAgent(ctx, "agent:1")
	require.NoError(t, err)
	return runner
}

func TestEveryMethodSendsFullArgv(t *testing.T) {
	runner := runAllMethods(t, "testsession")

	want := [][]string{
		{"--session", "testsession", "worktree", "create", "--cwd", "/w/repo", "--branch", "feat/x", "--base", "main"},
		{"--session", "testsession", "worktree", "remove", "wt:1"},
		{"--session", "testsession", "workspace", "close", "ws:1"},
		{"--session", "testsession", "pane", "split", "pane:root", "--direction", "right", "--cwd", "/w/repo", "--no-focus"},
		{"--session", "testsession", "agent", "start", "coder", "--kind", "agy", "--pane", "pane:2"},
		{"--session", "testsession", "agent", "prompt", "agent:1", "continue", "--wait", "--timeout", "90000"},
		{"--session", "testsession", "agent", "wait", "agent:1", "--until", "working", "--until", "done", "--timeout", "30000"},
		{"--session", "testsession", "agent", "read", "agent:1", "--source", "recent-unwrapped", "--lines", "120"},
	}

	assert.Equal(t, want, runner.calls)
	requireArgvShape(t, runner.calls)
}

func TestCustomSessionFlowsIntoEveryCall(t *testing.T) {
	runner := runAllMethods(t, "staging-7")

	for i, args := range runner.calls {
		require.GreaterOrEqualf(t, len(args), 3, "call %d", i)
		assert.Equal(t, "staging-7", args[1], "call %d must carry the session at position 1: %v", i, args)
	}
	requireArgvShape(t, runner.calls)
}

func TestCreateWorktreeExtraction(t *testing.T) {
	tests := []struct {
		name   string
		stdout string
		want   herdr.WorktreeInfo
	}{
		{
			name:   "flat snake_case shape",
			stdout: `{"id":"cli:worktree:create","result":{"worktree_id":"wt:1","workspace_id":"ws:1","tab_id":"tab:1","pane_id":"pane:1","path":"/w/branch"}}`,
			want: herdr.WorktreeInfo{
				ID:          "wt:1",
				WorkspaceID: "ws:1",
				TabID:       "tab:1",
				RootPaneID:  "pane:1",
				Path:        "/w/branch",
			},
		},
		{
			name:   "nested object shape",
			stdout: `{"id":"cli:worktree:create","result":{"workspace":{"workspace_id":"ws:9"},"tab":{"tab_id":"tab:9"},"root_pane":{"pane_id":"pane:9"},"worktree":{"worktree_id":"wt:9","path":"/w/9"}}}`,
			want: herdr.WorktreeInfo{
				ID:          "wt:9",
				WorkspaceID: "ws:9",
				TabID:       "tab:9",
				RootPaneID:  "pane:9",
				Path:        "/w/9",
			},
		},
		{
			name:   "flat root_pane_id alias",
			stdout: `{"result":{"workspace_id":"ws:2","root_pane_id":"pane:2"}}`,
			want: herdr.WorktreeInfo{
				WorkspaceID: "ws:2",
				RootPaneID:  "pane:2",
			},
		},
		{
			name:   "mixed flat and nested with missing ids",
			stdout: `{"result":{"workspace_id":"ws:3","tab":{"tab_id":"tab:3"},"path":""}}`,
			want: herdr.WorktreeInfo{
				WorkspaceID: "ws:3",
				TabID:       "tab:3",
			},
		},
		{
			name:   "empty flat value falls through to nested",
			stdout: `{"result":{"pane_id":"","root_pane":{"pane_id":"pane:7"},"workspace":{"workspace_id":"ws:7"}}}`,
			want: herdr.WorktreeInfo{
				WorkspaceID: "ws:7",
				RootPaneID:  "pane:7",
			},
		},
		{
			name:   "empty result yields zero value",
			stdout: `{"result":{}}`,
			want:   herdr.WorktreeInfo{},
		},
		{
			name:   "non-object result yields zero value",
			stdout: `{"result":"weird"}`,
			want:   herdr.WorktreeInfo{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := newFakeRunner(map[string]scriptedCall{
				"worktree create": {stdout: tt.stdout},
			})
			c := herdr.NewWithRunner("factory", runner.run)

			info, err := c.CreateWorktree(context.Background(), "/w/repo", "feat/x", "main")

			require.NoError(t, err)
			assert.Equal(t, tt.want, info)
			requireArgvShape(t, runner.calls)
		})
	}
}

func TestPromptAgentStatusExtraction(t *testing.T) {
	tests := []struct {
		name   string
		stdout string
		want   string
	}{
		{name: "status key", stdout: `{"id":"cli:agent:prompt","result":{"status":"done"}}`, want: "done"},
		{name: "result key", stdout: `{"result":{"result":"working"}}`, want: "working"},
		{name: "empty status falls through to result", stdout: `{"result":{"status":"","result":"idle"}}`, want: "idle"},
		{name: "plain string result", stdout: `{"result":"blocked"}`, want: "blocked"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := newFakeRunner(map[string]scriptedCall{
				"agent prompt": {stdout: tt.stdout},
			})
			c := herdr.NewWithRunner("factory", runner.run)

			status, err := c.PromptAgent(context.Background(), "agent:1", "go", time.Second)

			require.NoError(t, err)
			assert.Equal(t, tt.want, status)
			requireArgvShape(t, runner.calls)
		})
	}
}

func TestPromptAgentTimeoutIsMilliseconds(t *testing.T) {
	runner := newFakeRunner(map[string]scriptedCall{
		"agent prompt": {stdout: `{"result":{"status":"done"}}`},
	})
	c := herdr.NewWithRunner("factory", runner.run)

	_, err := c.PromptAgent(context.Background(), "agent:1", "go now", 1500*time.Millisecond)

	require.NoError(t, err)
	want := []string{"--session", "factory", "agent", "prompt", "agent:1", "go now", "--wait", "--timeout", "1500"}
	assert.Equal(t, want, runner.calls[0])
	requireArgvShape(t, runner.calls)
}

func TestWaitAgentRepeatsUntilInOrder(t *testing.T) {
	tests := []struct {
		name     string
		until    []string
		wait     time.Duration
		wantTail []string
	}{
		{
			name:     "repeats until once per state in order",
			until:    []string{"working", "done", "blocked"},
			wait:     60 * time.Second,
			wantTail: []string{"--until", "working", "--until", "done", "--until", "blocked", "--timeout", "60000"},
		},
		{
			name:     "single state",
			until:    []string{"done"},
			wait:     5 * time.Second,
			wantTail: []string{"--until", "done", "--timeout", "5000"},
		},
		{
			name:     "no states waits for any settle",
			until:    nil,
			wait:     6 * time.Second,
			wantTail: []string{"--timeout", "6000"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := newFakeRunner(map[string]scriptedCall{
				"agent wait": {stdout: `{"result":{"status":"done"}}`},
			})
			c := herdr.NewWithRunner("factory", runner.run)

			status, err := c.WaitAgent(context.Background(), "agent:1", tt.until, tt.wait)

			require.NoError(t, err)
			assert.Equal(t, "done", status)
			want := append([]string{"--session", "factory", "agent", "wait", "agent:1"}, tt.wantTail...)
			assert.Equal(t, want, runner.calls[0])
			requireArgvShape(t, runner.calls)
		})
	}
}

func TestReadAgentOutputExtraction(t *testing.T) {
	tests := []struct {
		name   string
		stdout string
		want   string
	}{
		{name: "content key", stdout: `{"id":"cli:agent:read","result":{"content":"alpha"}}`, want: "alpha"},
		{name: "text fallback", stdout: `{"result":{"text":"beta"}}`, want: "beta"},
		{name: "output fallback", stdout: `{"result":{"output":"gamma"}}`, want: "gamma"},
		{name: "content wins over text", stdout: `{"result":{"content":"first","text":"second"}}`, want: "first"},
		{name: "compact result json when no text key", stdout: `{"id":"cli:agent:read","result": {"lines" : 3, "open" : true}}`, want: `{"lines":3,"open":true}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := newFakeRunner(map[string]scriptedCall{
				"agent read": {stdout: tt.stdout},
			})
			c := herdr.NewWithRunner("factory", runner.run)

			text, err := c.ReadAgent(context.Background(), "agent:1")

			require.NoError(t, err)
			assert.Equal(t, tt.want, text)
			requireArgvShape(t, runner.calls)
		})
	}
}

func TestErrorEnvelopeInStderrBecomesCommandError(t *testing.T) {
	assert.EqualError(t, herdr.ErrServerUnavailable, "herdr: session unavailable")

	exitErr := mustExitError(t)
	runner := newFakeRunner(map[string]scriptedCall{
		"worktree create": {
			stderr: `{"id":"cli:worktree:create","error":{"code":"server_not_running","message":"no herdr server is running at /run/herdr.sock; run herdr up first"}}`,
			err:    exitErr,
		},
	})
	c := herdr.NewWithRunner("factory", runner.run)

	_, err := c.CreateWorktree(context.Background(), "/w/repo", "feat/x", "main")

	require.Error(t, err)
	var cmdErr *herdr.CommandError
	require.ErrorAs(t, err, &cmdErr)
	assert.Equal(t, "worktree create", cmdErr.Subcommand)
	assert.Equal(t, "server_not_running", cmdErr.Code)
	assert.Equal(t, "no herdr server is running at /run/herdr.sock; run herdr up first", cmdErr.Message)
	assert.Equal(t, []string{
		"--session", "factory",
		"worktree", "create",
		"--cwd", "/w/repo",
		"--branch", "feat/x",
		"--base", "main",
	}, cmdErr.Args)
	assert.True(t, errors.Is(err, herdr.ErrServerUnavailable))
	requireArgvShape(t, runner.calls)

	t.Run("other error codes do not unwrap to ErrServerUnavailable", func(t *testing.T) {
		runner := newFakeRunner(map[string]scriptedCall{
			"worktree remove": {
				stderr: `{"id":"cli:worktree:remove","error":{"code":"invalid_args","message":"no such worktree"}}`,
				err:    exitErr,
			},
		})
		c := herdr.NewWithRunner("factory", runner.run)

		err := c.RemoveWorktree(context.Background(), "wt:1")

		require.Error(t, err)
		var cmdErr *herdr.CommandError
		require.ErrorAs(t, err, &cmdErr)
		assert.Equal(t, "invalid_args", cmdErr.Code)
		assert.Equal(t, "no such worktree", cmdErr.Message)
		assert.False(t, errors.Is(err, herdr.ErrServerUnavailable))
	})
}

func TestErrorEnvelopeOnStdoutWithExitZeroBecomesCommandError(t *testing.T) {
	runner := newFakeRunner(map[string]scriptedCall{
		"agent wait": {
			stdout: `{"id":"cli:agent:wait","error":{"code":"server_not_running","message":"no herdr server is running at /run/herdr.sock"}}`,
		},
	})
	c := herdr.NewWithRunner("factory", runner.run)

	_, err := c.WaitAgent(context.Background(), "agent:1", nil, time.Second)

	require.Error(t, err)
	var cmdErr *herdr.CommandError
	require.ErrorAs(t, err, &cmdErr)
	assert.Equal(t, "agent wait", cmdErr.Subcommand)
	assert.Equal(t, "server_not_running", cmdErr.Code)
	assert.Equal(t, "no herdr server is running at /run/herdr.sock", cmdErr.Message)
	assert.True(t, errors.Is(err, herdr.ErrServerUnavailable))
	requireArgvShape(t, runner.calls)
}

func TestNonJSONStderrBecomesCommandError(t *testing.T) {
	exitErr := mustExitError(t)

	t.Run("syntax failure carries the stderr text", func(t *testing.T) {
		runner := newFakeRunner(map[string]scriptedCall{
			"pane split": {stderr: "Error: unknown flag: --bogus\n", err: exitErr},
		})
		c := herdr.NewWithRunner("factory", runner.run)

		_, err := c.SplitPane(context.Background(), "pane:root", "right", "/w/repo")

		require.Error(t, err)
		var cmdErr *herdr.CommandError
		require.ErrorAs(t, err, &cmdErr)
		assert.Equal(t, "pane split", cmdErr.Subcommand)
		assert.Empty(t, cmdErr.Code)
		assert.Equal(t, "Error: unknown flag: --bogus", cmdErr.Message)
		assert.False(t, errors.Is(err, herdr.ErrServerUnavailable))
		requireArgvShape(t, runner.calls)
	})

	t.Run("empty stderr falls back to the process error", func(t *testing.T) {
		runner := newFakeRunner(map[string]scriptedCall{
			"workspace close": {err: exitErr},
		})
		c := herdr.NewWithRunner("factory", runner.run)

		err := c.CloseWorkspace(context.Background(), "ws:1")

		require.Error(t, err)
		var cmdErr *herdr.CommandError
		require.ErrorAs(t, err, &cmdErr)
		assert.Empty(t, cmdErr.Code)
		assert.Equal(t, exitErr.Error(), cmdErr.Message)
	})
}

func TestStartupFailureWrapsErrNotFound(t *testing.T) {
	runner := newFakeRunner(map[string]scriptedCall{
		"agent read": {err: exec.ErrNotFound},
	})
	c := herdr.NewWithRunner("factory", runner.run)

	_, err := c.ReadAgent(context.Background(), "agent:1")

	require.Error(t, err)
	assert.True(t, errors.Is(err, exec.ErrNotFound))
	assert.ErrorContains(t, err, "herdr agent read")
	var cmdErr *herdr.CommandError
	assert.False(t, errors.As(err, &cmdErr))
}

func TestContextCancellationPropagates(t *testing.T) {
	runner := newFakeRunner(map[string]scriptedCall{
		"pane split": {err: context.Canceled},
	})
	c := herdr.NewWithRunner("factory", runner.run)

	_, err := c.SplitPane(context.Background(), "pane:root", "right", "/w/repo")

	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
	var cmdErr *herdr.CommandError
	assert.False(t, errors.As(err, &cmdErr))
}

func TestStderrMessageIsTruncated(t *testing.T) {
	runner := newFakeRunner(map[string]scriptedCall{
		"agent read": {stderr: strings.Repeat("é", 900), err: mustExitError(t)},
	})
	c := herdr.NewWithRunner("factory", runner.run)

	_, err := c.ReadAgent(context.Background(), "agent:1")

	require.Error(t, err)
	var cmdErr *herdr.CommandError
	require.ErrorAs(t, err, &cmdErr)
	runes := []rune(cmdErr.Message)
	assert.LessOrEqual(t, len(runes), 520, "message must stay bounded")
	assert.GreaterOrEqual(t, len(runes), 400, "message must keep ~500 runes of context")
	assert.True(t, utf8.ValidString(cmdErr.Message), "truncation must not split a rune")
}

func TestCommandErrorFormat(t *testing.T) {
	tests := []struct {
		name string
		err  *herdr.CommandError
		want string
	}{
		{
			name: "code and message",
			err: &herdr.CommandError{
				Subcommand: "worktree create",
				Code:       "server_not_running",
				Message:    "no herdr server is running",
			},
			want: "herdr worktree create: server_not_running: no herdr server is running",
		},
		{
			name: "message without code",
			err:  &herdr.CommandError{Subcommand: "pane split", Message: "unknown flag"},
			want: "herdr pane split: unknown flag",
		},
		{
			name: "code without message",
			err:  &herdr.CommandError{Subcommand: "agent start", Code: "timeout"},
			want: "herdr agent start: timeout",
		},
		{
			name: "subcommand alone",
			err:  &herdr.CommandError{Subcommand: "agent read"},
			want: "herdr agent read",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.err.Error())
		})
	}
}
