package pipeline

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestAgentArgs covers the {worktree} token expansion the pipeline applies to
// the configured agent.args before StartAgent: an empty configuration yields
// nil, the exact token is replaced by the run's absolute worktree path, every
// other element passes through, and the configured slice is never mutated.
func TestAgentArgs(t *testing.T) {
	configured := []string{"--add-dir", "{worktree}", "--dangerously-skip-permissions"}

	t.Run("empty configuration yields nil", func(t *testing.T) {
		assert.Nil(t, agentArgs(nil, "/w/repo"))
		assert.Nil(t, agentArgs([]string{}, "/w/repo"))
	})

	t.Run("worktree token expands and other tokens pass through", func(t *testing.T) {
		got := agentArgs(configured, "/w/repo")
		assert.Equal(t,
			[]string{"--add-dir", "/w/repo", "--dangerously-skip-permissions"},
			got)
	})

	t.Run("the configured slice is not mutated", func(t *testing.T) {
		_ = agentArgs(configured, "/w/repo")
		assert.Equal(t,
			[]string{"--add-dir", "{worktree}", "--dangerously-skip-permissions"},
			configured)
	})

	t.Run("a token merely embedded in a larger argument is not replaced", func(t *testing.T) {
		got := agentArgs([]string{"--dir={worktree}"}, "/w/repo")
		assert.Equal(t, []string{"--dir={worktree}"}, got)
	})
}
