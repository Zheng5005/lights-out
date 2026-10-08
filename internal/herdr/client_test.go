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
		"agent list": {stdout: `{"result":{"agents":[]}}`},
		"pane split": {stdout: `{"result":{"pane_id":"pane:new"}}`},
		// The live 0.9.3 wait/prompt shape: the settled agent arrives as a
		// NESTED object and the status token lives at result.agent.agent_status.
		"agent prompt": {stdout: `{"result":{"agent":{"agent":"agy","agent_status":"done","interactive_ready":true,"type":"agent_info"}}}`},
		"agent wait":   {stdout: `{"result":{"agent":{"agent":"agy","agent_status":"done","interactive_ready":true,"type":"agent_info"}}}`},
		"agent read":   {stdout: `{"result":{"content":"output"}}`},
	})
	c := herdr.NewWithRunner(session, runner.run)
	ctx := context.Background()

	// Probe runs first: the factory fails fast on connectivity before any
	// work, exactly like the live loop.
	require.NoError(t, c.Probe(ctx))
	_, err := c.CreateWorktree(ctx, "/w/repo", "feat/x", "main")
	require.NoError(t, err)
	require.NoError(t, c.RemoveWorktree(ctx, "w1"))
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
	_, err = c.ReadAgentVisible(ctx, "agent:1")
	require.NoError(t, err)
	require.NoError(t, c.SendKeys(ctx, "w1:p1", "enter"))
	return runner
}

func TestEveryMethodSendsFullArgv(t *testing.T) {
	runner := runAllMethods(t, "testsession")

	want := [][]string{
		{"--session", "testsession", "agent", "list"},
		{"--session", "testsession", "worktree", "create", "--cwd", "/w/repo", "--branch", "feat/x", "--base", "main", "--trust-repository"},
		{"--session", "testsession", "worktree", "remove", "--workspace", "w1", "--force"},
		{"--session", "testsession", "workspace", "close", "ws:1"},
		{"--session", "testsession", "pane", "split", "pane:root", "--direction", "right", "--cwd", "/w/repo", "--no-focus"},
		{"--session", "testsession", "agent", "start", "coder", "--kind", "agy", "--pane", "pane:2"},
		{"--session", "testsession", "agent", "prompt", "agent:1", "continue", "--wait", "--timeout", "90000"},
		{"--session", "testsession", "agent", "wait", "agent:1", "--until", "working", "--until", "done", "--timeout", "30000"},
		{"--session", "testsession", "agent", "read", "agent:1", "--source", "recent-unwrapped", "--lines", "120"},
		{"--session", "testsession", "agent", "read", "agent:1", "--source", "visible"},
		{"--session", "testsession", "agent", "send-keys", "w1:p1", "enter"},
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

// TestRemoveWorktreeSendsWorkspaceFlag pins the herdr 0.9.3 removal
// contract: no positional argument — the LIVE workspace id goes in
// --workspace, because a worktree has no separate id in 0.9.3. A successful
// removal envelope returns nil; the same call also closes the workspace, so
// cleanup must not run a separate close afterwards (a closed workspace fails
// removal with workspace_not_found).
func TestRemoveWorktreeSendsWorkspaceFlag(t *testing.T) {
	runner := newFakeRunner(map[string]scriptedCall{
		"worktree remove": {stdout: `{"id":"cli:worktree:remove","result":{"forced":false,"path":"/w/branch","type":"worktree_removed","workspace_id":"w9"}}`},
	})
	c := herdr.NewWithRunner("factory", runner.run)

	err := c.RemoveWorktree(context.Background(), "w9")

	require.NoError(t, err)
	want := []string{"--session", "factory", "worktree", "remove", "--workspace", "w9", "--force"}
	assert.Equal(t, want, runner.calls[0])
	requireArgvShape(t, runner.calls)
}

// TestStartAgentForwardsExtraArgs pins the headless agent-start contract: when
// agent.args are configured, the extra arguments are appended after an
// explicit `--` separator so herdr forwards them to the agent binary. agy uses
// `--add-dir <abs worktree>` to suppress the project-trust dialog and
// `--dangerously-skip-permissions` to auto-approve tool permissions — both are
// required for an unmanned run.
func TestStartAgentForwardsExtraArgs(t *testing.T) {
	runner := newFakeRunner(map[string]scriptedCall{})
	c := herdr.NewWithRunner("factory", runner.run)

	err := c.StartAgent(context.Background(), "coder", "agy", "pane:2",
		"--add-dir", "/w/repo", "--dangerously-skip-permissions")

	require.NoError(t, err)
	want := []string{
		"--session", "factory",
		"agent", "start", "coder", "--kind", "agy", "--pane", "pane:2",
		"--", "--add-dir", "/w/repo", "--dangerously-skip-permissions",
	}
	assert.Equal(t, want, runner.calls[0])
	requireArgvShape(t, runner.calls)
}

// TestStartAgentWithoutExtraArgsOmitsSeparator guards the variadic boundary:
// with no extra args the argv must be exactly the bare start command — no
// trailing `--` separator is sent to herdr.
func TestStartAgentWithoutExtraArgsOmitsSeparator(t *testing.T) {
	runner := newFakeRunner(map[string]scriptedCall{})
	c := herdr.NewWithRunner("factory", runner.run)

	require.NoError(t, c.StartAgent(context.Background(), "coder", "agy", "pane:2"))

	want := []string{"--session", "factory", "agent", "start", "coder", "--kind", "agy", "--pane", "pane:2"}
	assert.Equal(t, want, runner.calls[0])
	requireArgvShape(t, runner.calls)
}

// TestCreateWorktreeTrustsRepository pins the headless contract: the last
// argument of `worktree create` is always --trust-repository. Without it a
// freshly created untrusted worktree makes the spawned agent block on a TUI
// trust prompt ("Do you trust the contents of this project?") before the
// first prompt can be answered, deadlocking the factory flow headless.
func TestCreateWorktreeTrustsRepository(t *testing.T) {
	runner := newFakeRunner(map[string]scriptedCall{
		"worktree create": {stdout: `{"result":{"workspace_id":"w1"}}`},
	})
	c := herdr.NewWithRunner("factory", runner.run)

	_, err := c.CreateWorktree(context.Background(), "/w/repo", "feat/x", "main")

	require.NoError(t, err)
	require.Len(t, runner.calls, 1)
	args := runner.calls[0]
	require.NotEmpty(t, args)
	assert.Equal(t, "--trust-repository", args[len(args)-1],
		"worktree create must end with --trust-repository so the spawned agent never blocks on the trust prompt: %v", args)
	requireArgvShape(t, runner.calls)
}

// TestPromptAgentStatusExtraction pins status extraction for BOTH shapes
// herdr answers with: the live 0.9.3 nested envelope
// (`result.agent.agent_status` — what `agent wait` and `agent prompt --wait`
// really return, including the dialog-state token "blocked") and the flat
// `status`/`result` keys kept for synthetic/older envelopes, plus the
// plain-string fallback.
func TestPromptAgentStatusExtraction(t *testing.T) {
	tests := []struct {
		name   string
		stdout string
		want   string
	}{
		// Live 0.9.3 nested shape: {"result":{"agent":{"agent_status":"done",...}}}.
		{name: "nested agent_status done", stdout: `{"id":"cli:agent:prompt","result":{"agent":{"agent":"agy","agent_status":"done","interactive_ready":true,"type":"agent_info"}}}`, want: "done"},
		{name: "nested agent_status blocked", stdout: `{"id":"cli:agent:prompt","result":{"agent":{"agent":"agy","agent_status":"blocked","interactive_ready":true,"type":"agent_info"}}}`, want: "blocked"},
		{name: "nested agent_status working", stdout: `{"result":{"agent":{"agent":"agy","agent_status":"working","type":"agent_info"}}}`, want: "working"},
		// Flat fallbacks kept for synthetic/older envelopes.
		{name: "status key", stdout: `{"id":"cli:agent:prompt","result":{"status":"done"}}`, want: "done"},
		{name: "result key", stdout: `{"result":{"result":"working"}}`, want: "working"},
		{name: "empty status falls through to result", stdout: `{"result":{"status":"","result":"idle"}}`, want: "idle"},
		{name: "plain string result", stdout: `{"result":"blocked"}`, want: "blocked"},
		// Unknown nested shape must fall through to the flat keys rather
		// than silently yielding no status.
		{name: "nested agent without agent_status falls through to flat", stdout: `{"result":{"agent":{"agent":"agy","type":"agent_info"},"status":"idle"}}`, want: "idle"},
		{name: "empty nested agent_status falls through to flat", stdout: `{"result":{"agent":{"agent_status":""},"result":"blocked"}}`, want: "blocked"},
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
		"agent prompt": {stdout: `{"result":{"agent":{"agent":"agy","agent_status":"done","type":"agent_info"}}}`},
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
				"agent wait": {stdout: `{"result":{"agent":{"agent":"agy","agent_status":"done","type":"agent_info"}}}`},
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

// TestSendKeysArgv pins the `agent send-keys` argv: the target follows the
// subcommand and each logical key is appended verbatim after it — one
// recovery key ("enter" accepting the trust prompt) and a multi-key combo.
func TestSendKeysArgv(t *testing.T) {
	tests := []struct {
		name   string
		target string
		keys   []string
		want   []string
	}{
		{
			name:   "single recovery key",
			target: "w1:p1",
			keys:   []string{"enter"},
			want:   []string{"--session", "factory", "agent", "send-keys", "w1:p1", "enter"},
		},
		{
			name:   "multiple keys in order",
			target: "t",
			keys:   []string{"enter", "up"},
			want:   []string{"--session", "factory", "agent", "send-keys", "t", "enter", "up"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := newFakeRunner(map[string]scriptedCall{
				"agent send-keys": {stdout: `{"id":"cli:agent:send-keys","result":{}}`},
			})
			c := herdr.NewWithRunner("factory", runner.run)

			err := c.SendKeys(context.Background(), tt.target, tt.keys...)

			require.NoError(t, err)
			assert.Equal(t, tt.want, runner.calls[0])
			requireArgvShape(t, runner.calls)
		})
	}
}

// TestSendKeysErrorPassthrough covers the recovery path's failure modes: a
// server error envelope becomes a CommandError carrying the code (and
// unwrapping to ErrServerUnavailable for server_not_running), while a bare
// ExitError with empty stderr keeps an empty code so it can never be
// mistaken for the server-not-running sentinel.
func TestSendKeysErrorPassthrough(t *testing.T) {
	exitErr := mustExitError(t)

	t.Run("server_not_running envelope becomes CommandError", func(t *testing.T) {
		runner := newFakeRunner(map[string]scriptedCall{
			"agent send-keys": {
				stderr: `{"id":"cli:agent:send-keys","error":{"code":"server_not_running","message":"no herdr server is running at /run/herdr.sock; run herdr up first"}}`,
				err:    exitErr,
			},
		})
		c := herdr.NewWithRunner("factory", runner.run)

		err := c.SendKeys(context.Background(), "w1:p1", "enter")

		require.Error(t, err)
		var cmdErr *herdr.CommandError
		require.ErrorAs(t, err, &cmdErr)
		assert.Equal(t, "agent send-keys", cmdErr.Subcommand)
		assert.Equal(t, "server_not_running", cmdErr.Code)
		assert.True(t, errors.Is(err, herdr.ErrServerUnavailable))
		requireArgvShape(t, runner.calls)
	})

	t.Run("empty stderr ExitError keeps an empty code", func(t *testing.T) {
		runner := newFakeRunner(map[string]scriptedCall{
			"agent send-keys": {err: mustExitError(t)},
		})
		c := herdr.NewWithRunner("factory", runner.run)

		err := c.SendKeys(context.Background(), "w1:p1", "enter")

		require.Error(t, err)
		var cmdErr *herdr.CommandError
		require.ErrorAs(t, err, &cmdErr)
		assert.Empty(t, cmdErr.Code)
		assert.False(t, errors.Is(err, herdr.ErrServerUnavailable))
		requireArgvShape(t, runner.calls)
	})
}

// TestReadAgentVisibleOutputExtraction covers the dialog-evidence read: the
// same content/text/output fallback chain as ReadAgent (shared
// extractReadOutput), compact-JSON degradation included, and every call must
// carry the exact `--source visible` argv that works while the agent is
// blocked.
func TestReadAgentVisibleOutputExtraction(t *testing.T) {
	tests := []struct {
		name   string
		stdout string
		want   string
	}{
		{name: "content key", stdout: `{"id":"cli:agent:read","result":{"content":"Do you trust the contents of this project?"}}`, want: "Do you trust the contents of this project?"},
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

			text, err := c.ReadAgentVisible(context.Background(), "agent:1")

			require.NoError(t, err)
			assert.Equal(t, tt.want, text)
			want := []string{"--session", "factory", "agent", "read", "agent:1", "--source", "visible"}
			assert.Equal(t, want, runner.calls[0])
			requireArgvShape(t, runner.calls)
		})
	}
}

// TestReadAgentVisibleErrorPassthrough pins the failure path of the
// dialog-evidence read: an error envelope (a blocked agent cannot be read
// from the wrong source, for instance) surfaces as the raw CommandError
// with its code intact — the caller decides, the client never substitutes a
// degraded read.
func TestReadAgentVisibleErrorPassthrough(t *testing.T) {
	runner := newFakeRunner(map[string]scriptedCall{
		"agent read": {
			stderr: `{"id":"cli:agent:read","error":{"code":"agent_not_idle","message":"agent agent:1 is blocked during startup and is not ready for prompts"}}`,
			err:    mustExitError(t),
		},
	})
	c := herdr.NewWithRunner("factory", runner.run)

	_, err := c.ReadAgentVisible(context.Background(), "agent:1")

	require.Error(t, err)
	var cmdErr *herdr.CommandError
	require.ErrorAs(t, err, &cmdErr)
	assert.Equal(t, "agent read", cmdErr.Subcommand)
	assert.Equal(t, "agent_not_idle", cmdErr.Code)
	assert.Equal(t, []string{"--session", "factory", "agent", "read", "agent:1", "--source", "visible"}, cmdErr.Args)
	requireArgvShape(t, runner.calls)
}

// TestReadAgentRawTextFallback pins the live herdr 0.9.3 success shape: agent
// read prints the terminal text as raw UTF-8 on stdout (NOT a JSON envelope),
// so ReadAgent must return the trimmed raw text instead of failing the
// envelope parse.
func TestReadAgentRawTextFallback(t *testing.T) {
	tests := []struct {
		name   string
		stdout string
		want   string
	}{
		{name: "single line with trailing newline", stdout: "zheng005@host:~/.herdr/worktrees/Lights-out/factory-x$ agy\n", want: "zheng005@host:~/.herdr/worktrees/Lights-out/factory-x$ agy"},
		{name: "multi line no trailing newline", stdout: "line one\npwd\n/home/zheng005/.herdr/worktrees/Lights-out/factory-x", want: "line one\npwd\n/home/zheng005/.herdr/worktrees/Lights-out/factory-x"},
		{name: "crlf trailing", stdout: "line\r\n\r\n", want: "line"},
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
			want := []string{"--session", "factory", "agent", "read", "agent:1", "--source", "recent-unwrapped", "--lines", "120"}
			assert.Equal(t, want, runner.calls[0])
			requireArgvShape(t, runner.calls)
		})
	}
}

// TestReadAgentVisibleRawTextFallback pins the same live raw-text success
// shape for the visible read: a blocked agent's rendered dialog screen arrives
// as raw UTF-8 text, which is the evidence the evidence gate consumes.
func TestReadAgentVisibleRawTextFallback(t *testing.T) {
	tests := []struct {
		name   string
		stdout string
		want   string
	}{
		{name: "dialog text with trailing newline", stdout: "> Yes, I trust this folder\n", want: "> Yes, I trust this folder"},
		{name: "dialog text no trailing newline", stdout: "Do you trust the contents of this project?", want: "Do you trust the contents of this project?"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := newFakeRunner(map[string]scriptedCall{
				"agent read": {stdout: tt.stdout},
			})
			c := herdr.NewWithRunner("factory", runner.run)

			text, err := c.ReadAgentVisible(context.Background(), "agent:1")

			require.NoError(t, err)
			assert.Equal(t, tt.want, text)
			want := []string{"--session", "factory", "agent", "read", "agent:1", "--source", "visible"}
			assert.Equal(t, want, runner.calls[0])
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
		"--trust-repository",
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

		err := c.RemoveWorktree(context.Background(), "w1")

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

// TestProbeReturnsNilWhenSessionAnswers covers the happy path: one cheap
// read-only `agent list` through the normal envelope protocol, nil when the
// pinned session answers.
func TestProbeReturnsNilWhenSessionAnswers(t *testing.T) {
	runner := newFakeRunner(map[string]scriptedCall{
		"agent list": {stdout: `{"id":"cli:agent:list","result":{"agents":[]}}`},
	})
	c := herdr.NewWithRunner("factory", runner.run)

	err := c.Probe(context.Background())

	require.NoError(t, err)
	want := []string{"--session", "factory", "agent", "list"}
	assert.Equal(t, want, runner.calls[0])
	requireArgvShape(t, runner.calls)
}

// TestProbeWrapsServerNotRunningWithSessionName pins the fail-fast contract:
// a missing session surfaces as ErrServerUnavailable with a message that
// names the pinned session and says it is not running, so the operator knows
// exactly which session to start or fix.
func TestProbeWrapsServerNotRunningWithSessionName(t *testing.T) {
	runner := newFakeRunner(map[string]scriptedCall{
		"agent list": {
			stderr: `{"id":"cli:agent:list","error":{"code":"server_not_running","message":"no herdr server is running at /run/herdr.sock; run herdr up first"}}`,
			err:    mustExitError(t),
		},
	})
	c := herdr.NewWithRunner("factory-live", runner.run)

	err := c.Probe(context.Background())

	require.Error(t, err)
	assert.ErrorIs(t, err, herdr.ErrServerUnavailable)
	assert.ErrorContains(t, err, `herdr session "factory-live" is not running (start it or fix agent.session)`)
	requireArgvShape(t, runner.calls)
}

// TestProbeReturnsOtherEnvelopeErrorUnchanged keeps Probe honest: only
// server_not_running becomes the fail-fast connectivity error; any other
// envelope error passes through as the raw *CommandError.
func TestProbeReturnsOtherEnvelopeErrorUnchanged(t *testing.T) {
	runner := newFakeRunner(map[string]scriptedCall{
		"agent list": {
			stderr: `{"id":"cli:agent:list","error":{"code":"invalid_args","message":"unknown agent filter"}}`,
			err:    mustExitError(t),
		},
	})
	c := herdr.NewWithRunner("factory", runner.run)

	err := c.Probe(context.Background())

	require.Error(t, err)
	assert.False(t, errors.Is(err, herdr.ErrServerUnavailable))
	var cmdErr *herdr.CommandError
	require.ErrorAs(t, err, &cmdErr)
	assert.Equal(t, "agent list", cmdErr.Subcommand)
	assert.Equal(t, "invalid_args", cmdErr.Code)
	assert.Equal(t, "unknown agent filter", cmdErr.Message)
	requireArgvShape(t, runner.calls)
}

// TestProbeEmptyStderrExitErrorHasNoCode covers a process failure with no
// envelope: the CommandError carries the process message and an empty code,
// which must NOT be mistaken for the server-not-running sentinel.
func TestProbeEmptyStderrExitErrorHasNoCode(t *testing.T) {
	runner := newFakeRunner(map[string]scriptedCall{
		"agent list": {err: mustExitError(t)},
	})
	c := herdr.NewWithRunner("factory", runner.run)

	err := c.Probe(context.Background())

	require.Error(t, err)
	assert.False(t, errors.Is(err, herdr.ErrServerUnavailable))
	var cmdErr *herdr.CommandError
	require.ErrorAs(t, err, &cmdErr)
	assert.Equal(t, "agent list", cmdErr.Subcommand)
	assert.Empty(t, cmdErr.Code)
	requireArgvShape(t, runner.calls)
}

// TestProbeStartupFailurePassthrough covers the binary never starting
// (exec.ErrNotFound): Probe passes it through untouched so the caller can
// errors.Is() on it — no envelope, no CommandError, no sentinel.
func TestProbeStartupFailurePassthrough(t *testing.T) {
	runner := newFakeRunner(map[string]scriptedCall{
		"agent list": {err: exec.ErrNotFound},
	})
	c := herdr.NewWithRunner("factory", runner.run)

	err := c.Probe(context.Background())

	require.Error(t, err)
	assert.True(t, errors.Is(err, exec.ErrNotFound))
	var cmdErr *herdr.CommandError
	assert.False(t, errors.As(err, &cmdErr))
	requireArgvShape(t, runner.calls)
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
