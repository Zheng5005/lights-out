// Package herdr implements the Herdr CLI client the lights-out dark factory
// uses to drive a Herdr session programmatically.
//
// Every call targets `herdr --session <cfg> <cmd> ...`: the global --session
// flag comes first, before the subcommand, so a Client is pinned to one
// session for its lifetime. Herdr answers with JSON envelopes: the result
// arrives on stdout, a server failure arrives as an error envelope on stderr
// with exit status 1, and a CLI syntax error is plain stderr with exit
// status 2.
//
// In headless operation an agent reports "done" rather than "idle": the
// "idle" status requires the agent's tab to be seen by the focused UI, which
// a headless factory session never provides.
package herdr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// maxMessageRunes bounds every CommandError message: a hostile or verbose
// pane can put arbitrary text on stderr, and the factory must never carry an
// unbounded string into its logs or error chain.
const maxMessageRunes = 500

// Fixed flags ReadAgent always sends: the agent's recent pane output with
// escape sequences unwrapped, capped at 120 lines.
const (
	readSource = "recent-unwrapped"
	readLines  = "120"
)

// ErrServerUnavailable is wrapped by CommandError when the Herdr server for
// the configured session is not running (JSON error code "server_not_running"):
// the factory's fail-fast connectivity probe errors.Is() on it.
var ErrServerUnavailable = errors.New("herdr: session unavailable")

// CommandError is any herdr CLI failure: a JSON error envelope (from the
// server, exit status 1) or a non-JSON failure (CLI syntax error, exit status
// 2, or unparseable stderr). Code is the JSON "error.code" when an envelope
// was parsed, "" otherwise.
type CommandError struct {
	Subcommand string // e.g. "worktree create"; never empty
	Code       string
	Message    string   // server message or stderr snippet
	Args       []string // the full argv after "herdr"
}

// Error implements error as "herdr <Subcommand>: <Code>: <Message>", with
// empty parts omitted.
func (e *CommandError) Error() string {
	var b strings.Builder
	b.WriteString("herdr")
	if e.Subcommand != "" {
		b.WriteString(" ")
		b.WriteString(e.Subcommand)
	}
	if e.Code != "" {
		b.WriteString(": ")
		b.WriteString(e.Code)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	return b.String()
}

// Unwrap reports ErrServerUnavailable only for the "server_not_running" code,
// so the factory's connectivity probe can errors.Is() against it without
// matching every other server failure.
func (e *CommandError) Unwrap() error {
	if e.Code == "server_not_running" {
		return ErrServerUnavailable
	}
	return nil
}

// execFunc runs one herdr command and returns its stdout, stderr, and a
// non-nil error when the process could not start or exited non-zero.
type execFunc func(ctx context.Context, args ...string) (stdout, stderr []byte, err error)

// HerdrClient is the factory-facing contract, one method per herdr command.
type HerdrClient interface {
	// CreateWorktree creates a worktree for branch off base with cwd as its
	// working directory and returns the ids needed to drive the new session.
	CreateWorktree(ctx context.Context, cwd, branch, base string) (WorktreeInfo, error)
	// RemoveWorktree removes the worktree identified by worktreeID.
	RemoveWorktree(ctx context.Context, worktreeID string) error
	// CloseWorkspace closes the workspace identified by workspaceID.
	CloseWorkspace(ctx context.Context, workspaceID string) error
	// SplitPane splits paneID in direction with cwd as the new pane's working
	// directory and returns the new pane's id.
	SplitPane(ctx context.Context, paneID, direction, cwd string) (string, error)
	// StartAgent starts an agent of kind inside paneID.
	StartAgent(ctx context.Context, name, kind, paneID string) error
	// PromptAgent sends text to the target agent and returns its settled
	// status token.
	PromptAgent(ctx context.Context, target, text string, timeout time.Duration) (string, error)
	// WaitAgent blocks until the target agent reaches one of the until states
	// and returns the observed status token.
	WaitAgent(ctx context.Context, target string, until []string, timeout time.Duration) (string, error)
	// ReadAgent returns the agent's recent output text.
	ReadAgent(ctx context.Context, target string) (string, error)
}

// WorktreeInfo carries the ids of a freshly created worktree: the workspace,
// tab, and root pane the factory drives, plus the worktree id and checkout
// path when herdr reports them.
type WorktreeInfo struct {
	ID          string // worktree id, when reported
	WorkspaceID string
	TabID       string
	RootPaneID  string // the pane to split for the agent
	Path        string // checkout path, when reported
}

// Client is a Herdr CLI client pinned to one session. Construct it with New
// or NewWithRunner.
type Client struct {
	session string
	run     execFunc
}

// *Client must satisfy the factory-facing contract.
var _ HerdrClient = (*Client)(nil)

// New returns a client targeting the given Herdr session using the real
// herdr binary from PATH (exec.CommandContext; the binary name is "herdr").
// The factory passes config.Agent.Session here.
func New(session string) *Client {
	return &Client{session: session, run: runHerdr}
}

// NewWithRunner is the test seam; it replaces the executor. Do NOT call the
// real binary from tests.
func NewWithRunner(session string, run execFunc) *Client {
	return &Client{session: session, run: run}
}

// runHerdr runs the herdr binary from PATH synchronously, keeping stdout and
// stderr in SEPARATE buffers (never CombinedOutput): the result envelope must
// be parsed from stdout and the error envelope from stderr independently. An
// *exec.ExitError is returned as-is so the client can handle it; other
// errors, such as exec.ErrNotFound, pass through unchanged.
func runHerdr(ctx context.Context, args ...string) (stdout, stderr []byte, err error) {
	cmd := exec.CommandContext(ctx, "herdr", args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return outBuf.Bytes(), errBuf.Bytes(), err
}

// envelope is the top-level shape of herdr's JSON responses.
type envelope struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// argv builds the full command line after "herdr": the global --session flag
// first, then the subcommand and its arguments.
func (c *Client) argv(rest []string) []string {
	args := make([]string, 0, len(rest)+2)
	args = append(args, "--session", c.session)
	args = append(args, rest...)
	return args
}

// execute runs one herdr command and applies the shared envelope protocol:
//
//  1. a non-nil error whose stderr parses as a JSON error envelope becomes a
//     *CommandError carrying the server code and message (Unwrap reports
//     ErrServerUnavailable for "server_not_running");
//  2. a non-nil error with no usable envelope: an *exec.ExitError becomes a
//     *CommandError whose Message is the trimmed stderr snippet (the process
//     error when stderr is empty), truncated to maxMessageRunes; any other
//     error (a startup failure such as exec.ErrNotFound, or a canceled
//     context) is returned wrapped so errors.Is() keeps working;
//  3. a nil error parses the stdout envelope: an envelope error becomes a
//     *CommandError, unparseable stdout becomes a *CommandError with an
//     "<invalid JSON>" message, and the raw "result" value is returned for
//     the caller to extract.
func (c *Client) execute(ctx context.Context, subcommand string, rest ...string) (json.RawMessage, error) {
	args := c.argv(rest)

	stdout, stderr, err := c.run(ctx, args...)

	if err != nil {
		var env envelope
		if json.Unmarshal(stderr, &env) == nil && env.Error != nil {
			return nil, newCommandError(subcommand, env.Error.Code, env.Error.Message, args)
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			message := strings.TrimSpace(string(stderr))
			if message == "" {
				message = err.Error()
			}
			return nil, newCommandError(subcommand, "", message, args)
		}
		return nil, fmt.Errorf("herdr %s: %w", subcommand, err)
	}

	var env envelope
	if err := json.Unmarshal(stdout, &env); err != nil {
		return nil, newCommandError(subcommand, "", "<invalid JSON> "+string(stdout), args)
	}
	if env.Error != nil {
		return nil, newCommandError(subcommand, env.Error.Code, env.Error.Message, args)
	}
	return env.Result, nil
}

// newCommandError builds a *CommandError with the message truncated to
// maxMessageRunes so a hostile or verbose pane can never balloon an error
// string the factory logs or stores.
func newCommandError(subcommand, code, message string, args []string) *CommandError {
	return &CommandError{
		Subcommand: subcommand,
		Code:       code,
		Message:    truncateRunes(message, maxMessageRunes),
		Args:       args,
	}
}

// missingValueError reports a success envelope that did not carry the value
// the method must return; the compact result is attached so the live smoke
// test can see what herdr actually answered.
func (c *Client) missingValueError(subcommand, what string, result json.RawMessage, rest []string) *CommandError {
	return newCommandError(subcommand, "", "<missing "+what+"> "+compactResult(result), c.argv(rest))
}

func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

// CreateWorktree creates a worktree for branch off base with cwd as its
// working directory and returns the workspace, tab, and root pane the factory
// drives (RootPaneID is the pane to split for the agent), plus the worktree
// id and checkout path when herdr reports them.
//
// Extraction accepts BOTH the flattened snake_case keys observed on herdr's
// live list endpoints and the nested workspace/tab/root_pane objects the
// skill documents for creation responses. Missing ids are NOT an error at
// parse time: the WorktreeInfo carries whatever fields parsed and the caller
// validates what it needs, because the exact live shape of `worktree create`
// is not yet verified — Unit C's live smoke test pins it.
func (c *Client) CreateWorktree(ctx context.Context, cwd, branch, base string) (WorktreeInfo, error) {
	rest := []string{
		"worktree", "create",
		"--cwd", cwd,
		"--branch", branch,
		"--base", base,
	}
	result, err := c.execute(ctx, "worktree create", rest...)
	if err != nil {
		return WorktreeInfo{}, err
	}
	return extractWorktreeInfo(result), nil
}

// RemoveWorktree removes the worktree identified by worktreeID.
func (c *Client) RemoveWorktree(ctx context.Context, worktreeID string) error {
	rest := []string{"worktree", "remove", worktreeID}
	_, err := c.execute(ctx, "worktree remove", rest...)
	return err
}

// CloseWorkspace closes the workspace identified by workspaceID.
func (c *Client) CloseWorkspace(ctx context.Context, workspaceID string) error {
	rest := []string{"workspace", "close", workspaceID}
	_, err := c.execute(ctx, "workspace close", rest...)
	return err
}

// SplitPane splits paneID in direction ("right" or "down") with cwd as the
// new pane's working directory and returns the new pane's id. The --no-focus
// flag keeps the user's focus where it is: driving the session must never
// steal focus from a human working in the same Herdr session.
func (c *Client) SplitPane(ctx context.Context, paneID, direction, cwd string) (string, error) {
	rest := []string{
		"pane", "split", paneID,
		"--direction", direction,
		"--cwd", cwd,
		"--no-focus",
	}
	result, err := c.execute(ctx, "pane split", rest...)
	if err != nil {
		return "", err
	}
	id, ok := extractPaneID(result)
	if !ok {
		return "", c.missingValueError("pane split", "pane id", result, rest)
	}
	return id, nil
}

// StartAgent starts an agent named name of kind kind inside paneID. The
// --pane flag is required: `agent start` never creates or splits panes
// itself, so the pane must already exist (created by SplitPane). No timeout
// flag is sent; the CLI's default 30s startup timeout applies.
func (c *Client) StartAgent(ctx context.Context, name, kind, paneID string) error {
	rest := []string{"agent", "start", name, "--kind", kind, "--pane", paneID}
	_, err := c.execute(ctx, "agent start", rest...)
	return err
}

// PromptAgent sends text to the target agent and returns its settled status
// token ("done", "blocked", "idle", ...). The --wait flag makes the CLI block
// until the agent settles (idle, done, or blocked), bounded by timeout
// converted to milliseconds for --timeout.
func (c *Client) PromptAgent(ctx context.Context, target, text string, timeout time.Duration) (string, error) {
	rest := []string{
		"agent", "prompt", target, text,
		"--wait",
		"--timeout", strconv.FormatInt(timeout.Milliseconds(), 10),
	}
	result, err := c.execute(ctx, "agent prompt", rest...)
	if err != nil {
		return "", err
	}
	status, ok := extractStatus(result)
	if !ok {
		return "", c.missingValueError("agent prompt", "status", result, rest)
	}
	return status, nil
}

// WaitAgent blocks until the target agent's status matches one of the until
// states, or until timeout (converted to milliseconds for --timeout) elapses,
// and returns the observed status token. The --until flag is repeated once
// per state, in the order given; an empty until list waits for any settled
// state.
func (c *Client) WaitAgent(ctx context.Context, target string, until []string, timeout time.Duration) (string, error) {
	rest := []string{"agent", "wait", target}
	for _, state := range until {
		rest = append(rest, "--until", state)
	}
	rest = append(rest, "--timeout", strconv.FormatInt(timeout.Milliseconds(), 10))

	result, err := c.execute(ctx, "agent wait", rest...)
	if err != nil {
		return "", err
	}
	status, ok := extractStatus(result)
	if !ok {
		return "", c.missingValueError("agent wait", "status", result, rest)
	}
	return status, nil
}

// ReadAgent returns the agent's recent output text, read with the fixed
// package flags: readLines unwrapped lines of the pane's recent output.
func (c *Client) ReadAgent(ctx context.Context, target string) (string, error) {
	rest := []string{"agent", "read", target, "--source", readSource, "--lines", readLines}
	result, err := c.execute(ctx, "agent read", rest...)
	if err != nil {
		return "", err
	}
	return extractReadOutput(result), nil
}

// fieldString returns the first key among keys whose value is a non-empty
// JSON string; keys that are present but empty are skipped.
func fieldString(m map[string]json.RawMessage, keys ...string) (string, bool) {
	for _, key := range keys {
		raw, ok := m[key]
		if !ok {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) != nil || s == "" {
			continue
		}
		return s, true
	}
	return "", false
}

// nestedObject unmarshals the object stored under key, when that is what the
// value is.
func nestedObject(m map[string]json.RawMessage, key string) (map[string]json.RawMessage, bool) {
	raw, ok := m[key]
	if !ok {
		return nil, false
	}
	var nested map[string]json.RawMessage
	if json.Unmarshal(raw, &nested) != nil {
		return nil, false
	}
	return nested, true
}

// resultObject parses a raw result value as a JSON object.
func resultObject(result json.RawMessage) (map[string]json.RawMessage, bool) {
	var m map[string]json.RawMessage
	if len(result) == 0 || json.Unmarshal(result, &m) != nil || m == nil {
		return nil, false
	}
	return m, true
}

// firstString tries the flat keys in m first, then the same keys inside the
// nested object stored under nestedKey: herdr's live list endpoints return
// flat snake_case ids while the documented creation responses return nested
// workspace/tab/root_pane objects.
func firstString(m map[string]json.RawMessage, nestedKey string, keys ...string) string {
	if s, ok := fieldString(m, keys...); ok {
		return s
	}
	if nested, ok := nestedObject(m, nestedKey); ok {
		if s, ok := fieldString(nested, keys...); ok {
			return s
		}
	}
	return ""
}

// extractWorktreeInfo pulls the WorktreeInfo fields out of a worktree create
// result, accepting both the flattened keys and the nested objects. Missing
// ids yield zero values rather than an error — see CreateWorktree.
func extractWorktreeInfo(result json.RawMessage) WorktreeInfo {
	var info WorktreeInfo
	m, ok := resultObject(result)
	if !ok {
		return info
	}
	info.WorkspaceID = firstString(m, "workspace", "workspace_id")
	info.TabID = firstString(m, "tab", "tab_id")
	info.RootPaneID = firstString(m, "root_pane", "pane_id", "root_pane_id")
	info.ID = firstString(m, "worktree", "worktree_id")
	info.Path = firstString(m, "worktree", "path", "worktree_path")
	return info
}

// extractPaneID returns the new pane id from a pane split result: the flat
// "pane_id" (or "id"), else the same keys inside a nested "pane" object. ok
// is false when the result carries no id — SplitPane exists only to return
// it, so the caller reports an error rather than an empty id.
func extractPaneID(result json.RawMessage) (string, bool) {
	if m, ok := resultObject(result); ok {
		if id, ok := fieldString(m, "pane_id", "id"); ok {
			return id, true
		}
		if pane, ok := nestedObject(m, "pane"); ok {
			if id, ok := fieldString(pane, "pane_id", "id"); ok {
				return id, true
			}
		}
	}
	return "", false
}

// extractStatus returns the settled status token from a prompt/wait result:
// the "status" key, else the "result" key, else — when the result is a plain
// JSON string — the string itself. ok is false when no non-empty token
// exists; an empty token would silently break the factory's state matching,
// so the caller turns that into an error.
func extractStatus(result json.RawMessage) (string, bool) {
	if m, ok := resultObject(result); ok {
		return fieldString(m, "status", "result")
	}
	var s string
	if json.Unmarshal(result, &s) == nil && s != "" {
		return s, true
	}
	return "", false
}

// extractReadOutput returns the agent text from a read result: "content",
// then "text", then "output". When none of those keys holds a non-empty
// string, the raw result is returned as compact JSON — degraded, but still
// diagnostic.
func extractReadOutput(result json.RawMessage) string {
	if m, ok := resultObject(result); ok {
		if text, ok := fieldString(m, "content", "text", "output"); ok {
			return text
		}
	}
	return compactResult(result)
}

// compactResult re-marshals a raw result value as compact JSON so it can be
// quoted inside an error message or returned as degraded read output.
func compactResult(result json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, result); err != nil {
		return strings.TrimSpace(string(result))
	}
	return buf.String()
}
