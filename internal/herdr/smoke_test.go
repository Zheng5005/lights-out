// Live smoke test for the herdr client: drives the REAL herdr binary
// against a live Herdr session through the full factory loop — probe,
// worktree create, pane split, agent start (with evidence-gated first-boot
// trust-dialog recovery), prompt (with evidence-gated first-command
// permission-dialog recovery), read. Together the two dialogs prove the
// factory's real first-run path: every blocked settle is answered ONLY
// after its pinned visible-screen evidence, and an unpinned dialog is a
// loud failure, never an auto-accept.
//
// It is gated behind LIGHTS_OUT_HERDR_SMOKE=1 because every step MUTATES a
// live session (creates a worktree, a workspace, panes, and an agent), so it
// must be impossible to run by accident. Without the env var the only thing
// the test does is skip — that gate is its deterministic check in unit-land;
// the live run itself is parent-owned and happens outside verification.

package herdr_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Zheng5005/lights-out/internal/herdr"
	"github.com/stretchr/testify/require"
)

// envOr returns the environment value for key when it is set and non-empty,
// else fallback: the smoke test's override points (session, agent kind).
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// findModuleRoot walks up from start until it finds a directory containing
// go.mod and returns that directory. It errors loudly rather than guessing:
// a smoke run against the wrong checkout would create a worktree of the
// wrong repository.
func findModuleRoot(start string) (string, error) {
	for dir := start; ; {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod found walking up from %s", start)
		}
		dir = parent
	}
}

// TestLiveSmoke runs the full factory loop against a real Herdr session and
// pins the live answer shapes the client tolerantly extracts. Each step's
// failure is a real finding: nothing is swallowed, and the success log
// records the pinned WorktreeInfo fields, pane id, status, and read output
// as the live signature.
//
// Gated: set LIGHTS_OUT_HERDR_SMOKE=1 to run it. Never runs by accident —
// it mutates a live session.
func TestLiveSmoke(t *testing.T) {
	if os.Getenv("LIGHTS_OUT_HERDR_SMOKE") != "1" {
		t.Skip("set LIGHTS_OUT_HERDR_SMOKE=1 to run the live Herdr smoke test")
	}

	// One context for every exec, so a hung herdr process dies when the test
	// does instead of outliving it. 8 minutes bounds the whole loop: it
	// includes a possibly cold agent boot. Registered FIRST via t.Cleanup so
	// it runs LAST (LIFO): the cleanup steps below still get a live ctx.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	t.Cleanup(cancel)

	// "default" matches the config default and the live session name.
	session := envOr("LIGHTS_OUT_HERDR_SMOKE_SESSION", "default")

	repo := os.Getenv("LIGHTS_OUT_HERDR_SMOKE_REPO")
	if repo == "" {
		wd, err := os.Getwd()
		require.NoError(t, err, "getwd must work")
		repo, err = findModuleRoot(wd)
		require.NoErrorf(t, err, "LIGHTS_OUT_HERDR_SMOKE_REPO unset and no go.mod above %s", wd)
	}

	// The REAL binary: New (not NewWithRunner) shells out to herdr from PATH.
	client := herdr.New(session)

	// Step 4 — probe first: proves the factory's fail-fast connectivity check
	// (plan Open Decision #3: assume running, fail fast otherwise) works
	// live, before anything mutates the session.
	if err := client.Probe(ctx); err != nil {
		t.Fatalf("probe: %v", err)
	}

	// One timestamp for both branch and agent name: one identifiable
	// throwaway pair the operator can recognise in the session afterwards.
	stamp := strconv.FormatInt(time.Now().Unix(), 10)

	// Step 5 — throwaway worktree of THIS repository, based on main.
	info, err := client.CreateWorktree(ctx, repo, "factory-smoke-"+stamp, "main")

	// Step 6 — cleanup registered EARLY — immediately after the call, before any
	// validation — so even a partially extracted WorktreeInfo is torn down
	// on every failure path. t.Cleanup runs after t.Fatal too (a defer would
	// not: Fatal unwinds via runtime.Goexit). Verified herdr 0.9.3 contract:
	// ONE `worktree remove --workspace <id>` call removes the checkout AND
	// closes its workspace. Calling remove after the workspace is closed fails
	// with workspace_not_found, so there is no separate close step. The
	// workspace id is attempted only when present, and any failure is reported
	// via t.Errorf so a cleanup error is visible without masking the primary
	// finding that failed the test.
	t.Cleanup(func() {
		if info.WorkspaceID != "" {
			if err := client.RemoveWorktree(ctx, info.WorkspaceID); err != nil {
				t.Errorf("cleanup: remove worktree %s: %v", info.WorkspaceID, err)
			}
		}
	})

	if err != nil {
		t.Fatalf("worktree create failed: %v", err)
	}

	// THE PIN: tolerant extraction must see the real live answer. An empty
	// field means the live `worktree create` shape is NOT one the extractor
	// recognises — fail loudly with the full info struct so the shape can be
	// fixed from the output. herdr 0.9.3 has NO worktree id: the freshly
	// created worktree's identity is its live workspace id, and WorkspaceID
	// is the handle cleanup passes to worktree remove.
	if info.Path == "" {
		t.Fatalf("worktree create result lacks Path — real shape not extracted; got %+v", info)
	}
	if info.RootPaneID == "" {
		t.Fatalf("worktree create result lacks RootPaneID — real shape not extracted; got %+v", info)
	}
	if info.WorkspaceID == "" {
		t.Fatalf("worktree create result lacks WorkspaceID — real shape not extracted; got %+v", info)
	}

	// Step 7 — split a pane inside the worktree checkout for the agent.
	paneID, err := client.SplitPane(ctx, info.RootPaneID, "down", info.Path)
	require.NoError(t, err, "pane split against the live root pane must succeed")
	require.NotEmpty(t, paneID, "pane split must return a non-empty new pane id")

	// Step 8 — agy is the config factory default and is installed on the dev machine;
	// the env var exists so a live run can switch agent kind without editing
	// the test.
	kind := envOr("LIGHTS_OUT_HERDR_SMOKE_KIND", "agy")
	agentName := "factory-smoke-" + stamp

	// FIRST BOOT: a fresh worktree checkout has its OWN trust binding — the
	// tree creates a new trust binding per fresh worktree — so the FIRST
	// agent boot in every fresh checkout shows the agy project-trust dialog
	// ("Do you trust the contents of this project?"). herdr fails start fast
	// (agent_not_ready, "blocked during startup and is not ready for
	// prompts") when screen detection matches that dialog, but the blocked
	// agent name stays reachable for `agent read --source visible` and
	// `agent send-keys` until detection reports idle. That makes this
	// recovery the factory's NORMAL first-boot path, not an edge case. The
	// branch answers ONLY the pinned trust prompt — evidence-gated below —
	// and then waits for the agent to settle; a non-blocked error is a
	// genuine blocker and fails loudly.
	startErr := client.StartAgent(ctx, agentName, kind, paneID)
	switch {
	case startErr == nil:
		// A successful start means the agent is already ready for input per
		// the 0.9.3 contract: no recovery needed, go straight to the prompt.
	case !strings.Contains(startErr.Error(), "blocked during startup"):
		t.Fatalf("agent start: %v", startErr)
	default:
		// Recovery branch for the blocked-startup trust dialog.
		//
		// 1. Dialog evidence: the visible source is the only read herdr
		// serves while the agent is blocked (recent-unwrapped returns
		// agent_not_idle). It must show the CURRENT rendered screen.
		visible, err := client.ReadAgentVisible(ctx, agentName)
		require.NoError(t, err, "visible read of the blocked agent must succeed")

		// 2. EVIDENCE GATE — the safety boundary of this whole branch: the
		// screen must be EXACTLY the pinned trust prompt ("Do you trust the
		// contents of this project?" with the "Yes, I trust this folder"
		// option). Any other dialog is UNEXPECTED: fail with the actual
		// visible text and never auto-accept an unknown prompt.
		lowerVis := strings.ToLower(visible)
		if !strings.Contains(lowerVis, "do you trust the contents of this project?") ||
			!strings.Contains(lowerVis, "yes, i trust this folder") {
			t.Fatalf("blocked agent shows an UNEXPECTED dialog — refusing to send any key; visible screen:\n%s", visible)
		}

		// 3. `enter` accepts the pre-selected "Yes, I trust this folder".
		require.NoError(t, client.SendKeys(ctx, agentName, "enter"),
			"send-keys enter must accept the pinned trust prompt")

		// 4. Wait for detection to report the agent ready again: idle (the
		// seen token) or done (the headless token).
		recovered, err := client.WaitAgent(ctx, agentName, []string{"idle", "done"}, 60*time.Second)
		require.NoError(t, err, "agent wait after the trust answer must settle")
		if recovered != "idle" && recovered != "done" {
			t.Fatalf("recovered agent settled with raw status %q; want \"idle\" or \"done\"", recovered)
		}
		t.Logf("trust-dialog recovery OK: agent %q settled with status %q", agentName, recovered)
	}

	// Step 9 — 5 minutes: the FIRST agent boot on a live pane is slow (kind install,
	// model handshake), and a tighter timeout would flake exactly when the
	// session is cold.
	status, err := client.PromptAgent(ctx, agentName,
		"Run the command pwd and reply with ONLY the absolute path, nothing else.",
		5*time.Minute)
	require.NoError(t, err, "agent prompt must settle within the timeout")

	// Step 9b — the second-class dialog of the factory's real first-run
	// path: agy asks the trust dialog EITHER at startup (step 8) OR lazily
	// at first file access depending on detection timing, and the FIRST
	// command a trusted agent runs triggers the PERMISSION dialog
	// ("Requesting permission for:" / "Run this command?"). Either way
	// `agent prompt --wait` settles with agent_status "blocked" while the
	// dialog is up — a dialog state, NOT a prompt failure. This makes the
	// loop prove the FULL headless factory journey: each dialog answered
	// ONLY after its pinned evidence, then the output read from the
	// checkout. Headless operation reports "done"; "idle" is the seen
	// version of the same settled state.
	//
	// The evidence gate is the same safety boundary as the trust branch: any
	// blocked state WITHOUT the pinned permission text is an UNEXPECTED
	// dialog — fail loudly with the raw visible screen and never auto-accept
	// an unknown prompt.
	switch status {
	case "done", "idle":
		// Agent answered the pwd task without hitting a dialog.
	case "blocked":
		// Post-prompt dialog-recovery branch: the visible source is the only
		// read herdr serves while the agent is blocked, and it must show the
		// CURRENT rendered screen carrying one of the two pinned dialog
		// classes. Live, agy asks the trust dialog EITHER at startup (the
		// step-6 branch) OR lazily at first file access — the prompt can
		// settle blocked on either class depending on detection timing — and
		// the permission dialog appears on the first command.
		visible, err := client.ReadAgentVisible(ctx, agentName)
		require.NoError(t, err, "visible read of the blocked agent must succeed")

		// Evidence gate — exactly the pinned dialogs, never anything else:
		//   trust:      "Do you trust the contents of this project?" with
		//               "Yes, I trust this folder"
		//   permission: "Requesting permission for:" with
		//               "Run this command?"
		lower := strings.ToLower(visible)
		trustDialog := strings.Contains(lower, "do you trust the contents of this project?") &&
			strings.Contains(lower, "yes, i trust this folder")
		permissionDialog := strings.Contains(lower, "requesting permission for:") &&
			strings.Contains(lower, "run this command?")
		if !trustDialog && !permissionDialog {
			t.Fatalf("blocked prompt shows an UNEXPECTED dialog — refusing to send any key; visible screen:\n%s", visible)
		}

		// `enter` accepts the pre-selected option: "Yes, I trust this folder"
		// or option 1 "Yes, run command" respectively.
		require.NoError(t, client.SendKeys(ctx, agentName, "enter"),
			"send-keys enter must accept the pinned dialog")

		// Wait for detection to report the agent settled again: idle (the
		// seen token) or done (the headless token). The prompt's original
		// text was already consumed by herdr — recovery must NOT re-send it.
		settled, err := client.WaitAgent(ctx, agentName, []string{"idle", "done"}, 60*time.Second)
		require.NoError(t, err, "agent wait after the dialog answer must settle")
		if settled != "idle" && settled != "done" {
			t.Fatalf("agent settled with raw status %q; want \"idle\" or \"done\"", settled)
		}
		t.Logf("blocked-prompt dialog recovery OK: agent %q settled with status %q", agentName, settled)
	default:
		// Any other token (working, unknown, ...) is a real finding, not a
		// retry.
		t.Fatalf("agent settled with raw status %q; want \"done\" or \"idle\"", status)
	}

	// Step 10 — the agent must have run INSIDE the worktree checkout: its pwd
	// must contain the worktree path. On failure ReadAgent's compact-result
	// fallback carries the raw diagnostics.
	output, err := client.ReadAgent(ctx, agentName)
	require.NoError(t, err, "agent read must succeed")
	if !strings.Contains(output, info.Path) {
		t.Fatalf("agent output does not contain worktree path %q — agent did not run in the checkout; got: %s",
			info.Path, output)
	}

	// Step 11 — success: the live signature record — the pinned real shape of every id
	// this loop depends on, plus the settled status and the read output.
	t.Logf("live smoke OK: WorktreeInfo=%+v pane=%s agent=%q status=%q output=%q",
		info, paneID, agentName, status, output)
}
