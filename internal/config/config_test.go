package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Zheng5005/lights-out/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeConfig writes body to a unique file inside a per-test temp dir and
// returns its absolute path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func TestResolvePath(t *testing.T) {
	t.Run("env var wins over the default path", func(t *testing.T) {
		t.Setenv(config.EnvVar, "/custom/lights-out.yaml")
		assert.Equal(t, "/custom/lights-out.yaml", config.ResolvePath())
	})

	t.Run("falls back to the default path when the env var is unset", func(t *testing.T) {
		t.Setenv(config.EnvVar, "")
		assert.Equal(t, config.DefaultPath, config.ResolvePath())
	})
}

func TestLoadMissingFileIsAnError(t *testing.T) {
	t.Setenv(config.EnvVar, filepath.Join(t.TempDir(), "does-not-exist.yaml"))

	cfg, err := config.Load()

	require.Error(t, err)
	assert.ErrorContains(t, err, "does-not-exist.yaml")
	assert.Empty(t, cfg)
}

func TestLoadReadsThePathFromTheEnvVar(t *testing.T) {
	t.Setenv(config.EnvVar, writeConfig(t, "repo: envvar/repo\n"))

	cfg, err := config.Load()

	require.NoError(t, err)
	assert.Equal(t, "envvar/repo", cfg.Repo)
}

func TestResolveDefaultsFillsEveryMissingField(t *testing.T) {
	resolved := config.ResolveDefaults(config.Config{Repo: "owner/repo"})

	assert.Equal(t, 2, resolved.ConcurrencyLimit)
	assert.Equal(t, 2, resolved.DailyLimit)
	assert.Equal(t, "dark-factory", resolved.Labels.Trigger)
	assert.Equal(t, "factory-in-progress", resolved.Labels.InProgress)
	assert.Equal(t, "factory-blocked", resolved.Labels.Blocked)
	assert.Equal(t, "herdr", resolved.Agent.Dispatcher)
	assert.Equal(t, "headless", resolved.Agent.Mode)
	assert.Equal(t, "agy", resolved.Agent.Kind)
	assert.Equal(t, 15*time.Minute, resolved.Agent.Timeout)
	assert.Equal(t, "default", resolved.Agent.Session)
	assert.Equal(t, "main", resolved.Worktree.Base)
}

func TestResolveDefaultsDoesNotOverrideExplicitValues(t *testing.T) {
	explicit := config.Config{
		Enabled:          true,
		Repo:             "owner/repo",
		ConcurrencyLimit: 7,
		DailyLimit:       9,
		Labels: config.Labels{
			Trigger:    "t",
			InProgress: "p",
			Blocked:    "b",
		},
		Agent: config.Agent{
			Dispatcher: "d",
			Mode:       "m",
			Kind:       "k",
			Timeout:    42 * time.Minute,
			Session:    "factory",
		},
		Worktree: config.Worktree{Base: "develop"},
	}

	resolved := config.ResolveDefaults(explicit)

	assert.Equal(t, explicit, resolved)
}

func TestLoadAppliesDefaultsForAMinimalConfig(t *testing.T) {
	t.Setenv(config.EnvVar, writeConfig(t, "repo: owner/repo\n"))

	cfg, err := config.Load()

	require.NoError(t, err)
	assert.Equal(t, 2, cfg.ConcurrencyLimit)
	assert.Equal(t, 2, cfg.DailyLimit)
	assert.Equal(t, "dark-factory", cfg.Labels.Trigger)
	assert.Equal(t, "factory-in-progress", cfg.Labels.InProgress)
	assert.Equal(t, "factory-blocked", cfg.Labels.Blocked)
	assert.Equal(t, "herdr", cfg.Agent.Dispatcher)
	assert.Equal(t, "headless", cfg.Agent.Mode)
	assert.Equal(t, "agy", cfg.Agent.Kind)
	assert.Equal(t, 15*time.Minute, cfg.Agent.Timeout)
	assert.Equal(t, "default", cfg.Agent.Session)
	assert.Equal(t, "main", cfg.Worktree.Base)
}

func TestLoadKeepsConfiguredValues(t *testing.T) {
	t.Setenv(config.EnvVar, writeConfig(t, `
enabled: true
repo: fernando/lights-out
concurrency_limit: 5
daily_limit: 11
labels:
  trigger: opt-in
  in_progress: working
  blocked: stuck
agent:
  dispatcher: herdr
  mode: headless
  kind: claude
worktree:
  base: develop
`))

	cfg, err := config.Load()

	require.NoError(t, err)
	assert.True(t, cfg.Enabled)
	assert.Equal(t, "fernando/lights-out", cfg.Repo)
	assert.Equal(t, 5, cfg.ConcurrencyLimit)
	assert.Equal(t, 11, cfg.DailyLimit)
	assert.Equal(t, "opt-in", cfg.Labels.Trigger)
	assert.Equal(t, "working", cfg.Labels.InProgress)
	assert.Equal(t, "stuck", cfg.Labels.Blocked)
	assert.Equal(t, "claude", cfg.Agent.Kind)
	assert.Equal(t, "develop", cfg.Worktree.Base)
}

func TestLoadAcceptsADisabledConfig(t *testing.T) {
	t.Setenv(config.EnvVar, writeConfig(t, "enabled: false\nrepo: owner/repo\n"))

	cfg, err := config.Load()

	require.NoError(t, err)
	assert.False(t, cfg.Enabled)
}

func TestLoadValidatesEvenWhenDisabled(t *testing.T) {
	// The enabled gate is a run-time concern (Phase 5); a disabled factory
	// still has to carry a fully valid configuration.
	t.Setenv(config.EnvVar, writeConfig(t, "enabled: false\nrepo: owner-without-slash\n"))

	_, err := config.Load()

	require.Error(t, err)
	assert.ErrorContains(t, err, "owner/repo")
}

func TestValidateAcceptsAMinimalResolvedConfig(t *testing.T) {
	assert.NoError(t, config.Validate(config.ResolveDefaults(config.Config{Repo: "owner/repo"})))
}

// validBase returns a fully resolved, valid configuration. Each rejection case
// starts from it and breaks exactly one field, so the reported error always
// names the field under test instead of an earlier one.
func validBase() config.Config {
	return config.ResolveDefaults(config.Config{Repo: "owner/repo"})
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name    string
		break_  func(c *config.Config)
		wantErr string
	}{
		{
			name:    "repo without a slash",
			break_:  func(c *config.Config) { c.Repo = "owner" },
			wantErr: "owner/repo",
		},
		{
			name:    "repo with an empty owner",
			break_:  func(c *config.Config) { c.Repo = "/repo" },
			wantErr: "owner/repo",
		},
		{
			name:    "repo with an empty name",
			break_:  func(c *config.Config) { c.Repo = "owner/" },
			wantErr: "owner/repo",
		},
		{
			name:    "repo with more than one slash",
			break_:  func(c *config.Config) { c.Repo = "owner/group/repo" },
			wantErr: "owner/repo",
		},
		{
			name:    "repo with a blank owner",
			break_:  func(c *config.Config) { c.Repo = "   /repo" },
			wantErr: "owner/repo",
		},
		{
			name:    "missing repo",
			break_:  func(c *config.Config) { c.Repo = "" },
			wantErr: "owner/repo",
		},
		{
			name:    "zero concurrency limit",
			break_:  func(c *config.Config) { c.ConcurrencyLimit = 0 },
			wantErr: "concurrency_limit must be greater than 0, got 0",
		},
		{
			name:    "negative concurrency limit",
			break_:  func(c *config.Config) { c.ConcurrencyLimit = -1 },
			wantErr: "concurrency_limit must be greater than 0, got -1",
		},
		{
			name:    "zero daily limit",
			break_:  func(c *config.Config) { c.DailyLimit = 0 },
			wantErr: "daily_limit must be greater than 0, got 0",
		},
		{
			name:    "negative daily limit",
			break_:  func(c *config.Config) { c.DailyLimit = -3 },
			wantErr: "daily_limit must be greater than 0, got -3",
		},
		{
			name:    "empty trigger label",
			break_:  func(c *config.Config) { c.Labels.Trigger = "  " },
			wantErr: "labels.trigger must not be empty",
		},
		{
			name:    "empty in progress label",
			break_:  func(c *config.Config) { c.Labels.InProgress = "" },
			wantErr: "labels.in_progress must not be empty",
		},
		{
			name:    "empty blocked label",
			break_:  func(c *config.Config) { c.Labels.Blocked = "\t" },
			wantErr: "labels.blocked must not be empty",
		},
		{
			name:    "blank agent dispatcher",
			break_:  func(c *config.Config) { c.Agent.Dispatcher = " " },
			wantErr: "agent.dispatcher must not be empty",
		},
		{
			name:    "blank agent mode",
			break_:  func(c *config.Config) { c.Agent.Mode = " " },
			wantErr: "agent.mode must not be empty",
		},
		{
			name:    "blank agent kind",
			break_:  func(c *config.Config) { c.Agent.Kind = " " },
			wantErr: "agent.kind must not be empty",
		},
		{
			name:    "zero agent timeout",
			break_:  func(c *config.Config) { c.Agent.Timeout = 0 },
			wantErr: "agent.timeout must be greater than 0, got 0s",
		},
		{
			name:    "negative agent timeout",
			break_:  func(c *config.Config) { c.Agent.Timeout = -5 * time.Minute },
			wantErr: "agent.timeout must be greater than 0, got -5m",
		},
		{
			name:    "blank agent session",
			break_:  func(c *config.Config) { c.Agent.Session = " " },
			wantErr: "agent.session must not be empty",
		},
		{
			name:    "blank worktree base",
			break_:  func(c *config.Config) { c.Worktree.Base = " " },
			wantErr: "worktree.base must not be empty",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validBase()
			tt.break_(&cfg)

			err := config.Validate(cfg)

			require.Error(t, err)
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{
			name: "misspelled top level key",
			yaml: "repo: owner/repo\nconcurrency_limmit: 3\n",
		},
		{
			name: "misspelled nested key",
			yaml: "repo: owner/repo\nlabels:\n  trigerr: dark-factory\n",
		},
		{
			name: "misspelled worktree key",
			yaml: "repo: owner/repo\nworktree:\n  based: main\n",
		},
		{
			name: "misspelled agent timeout key",
			yaml: "repo: owner/repo\nagent:\n  timout: 15m\n",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(config.EnvVar, writeConfig(t, tt.yaml))

			_, err := config.Load()

			require.Error(t, err, "an unknown field must fail the load instead of silently disabling it")
			assert.ErrorContains(t, err, "field")
		})
	}
}

func TestLoadRejectsMalformedYAMLWithoutPanicking(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{name: "unterminated quote", yaml: "repo: \"owner/repo\n"},
		{name: "unclosed flow sequence", yaml: "repo: [owner/repo\n"},
		{name: "tab indentation", yaml: "repo: owner/repo\nlabels:\n\ttrigger: dark-factory\n"},
		{name: "top level is not a mapping", yaml: "- owner/repo\n- second\n"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(config.EnvVar, writeConfig(t, tt.yaml))

			require.NotPanics(t, func() {
				_, err := config.Load()
				assert.Error(t, err)
			})
		})
	}
}

func TestLoadRejectsAnEmptyFile(t *testing.T) {
	t.Setenv(config.EnvVar, writeConfig(t, ""))

	_, err := config.Load()

	require.Error(t, err)
	assert.ErrorContains(t, err, "repo")
}

// TestLoadFallsBackToTheDefaultPathInTheWorkingDirectory covers the documented
// fallback: with no env var set, the factory reads ./config.yaml.
func TestLoadFallsBackToTheDefaultPathInTheWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "config.yaml"),
		[]byte("repo: fallback/repo\n"),
		0o600,
	))

	previous, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { require.NoError(t, os.Chdir(previous)) })

	t.Setenv(config.EnvVar, "")

	cfg, err := config.Load()

	require.NoError(t, err)
	assert.Equal(t, "fallback/repo", cfg.Repo)
	assert.Equal(t, 2, cfg.DailyLimit)
}

// TestLoadRejectsNonPositiveLimits guards the boundary between "absent" and
// "explicitly zero": an absent limit falls back to the default, an explicitly
// configured non-positive limit is an authoring error and must not be silently
// promoted to a default that hides it.
func TestLoadRejectsNonPositiveLimits(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "zero concurrency limit",
			yaml:    "repo: owner/repo\nconcurrency_limit: 0\n",
			wantErr: "concurrency_limit must be greater than 0, got 0",
		},
		{
			name:    "negative concurrency limit",
			yaml:    "repo: owner/repo\nconcurrency_limit: -2\n",
			wantErr: "concurrency_limit must be greater than 0, got -2",
		},
		{
			name:    "zero daily limit",
			yaml:    "repo: owner/repo\ndaily_limit: 0\n",
			wantErr: "daily_limit must be greater than 0, got 0",
		},
		{
			name:    "negative daily limit",
			yaml:    "repo: owner/repo\ndaily_limit: -4\n",
			wantErr: "daily_limit must be greater than 0, got -4",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(config.EnvVar, writeConfig(t, tt.yaml))

			cfg, err := config.Load()

			require.Error(t, err)
			assert.ErrorContains(t, err, tt.wantErr)
			assert.Empty(t, cfg)
		})
	}
}

// TestLoadParsesAgentTimeout covers the Phase 4 Open Decision #5: the
// agent.timeout key is a Go duration string that decodes into
// time.Duration.
func TestLoadParsesAgentTimeout(t *testing.T) {
	t.Setenv(config.EnvVar, writeConfig(t, "repo: owner/repo\nagent:\n  timeout: 15m\n"))

	cfg, err := config.Load()

	require.NoError(t, err)
	assert.Equal(t, 15*time.Minute, cfg.Agent.Timeout)
}

// TestLoadRejectsInvalidAgentTimeout guards the absent/explicit boundary for
// agent.timeout: an absent key falls back to the default, but an explicitly
// configured unusable value is an authoring error and must name the key
// instead of being silently promoted to a default that hides it.
func TestLoadRejectsInvalidAgentTimeout(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "explicit zero timeout",
			yaml:    "repo: owner/repo\nagent:\n  timeout: 0s\n",
			wantErr: "agent.timeout must be greater than 0, got 0s",
		},
		{
			name:    "explicit negative timeout",
			yaml:    "repo: owner/repo\nagent:\n  timeout: -5m\n",
			wantErr: "agent.timeout must be greater than 0, got -5m",
		},
		{
			name:    "unparsable timeout",
			yaml:    "repo: owner/repo\nagent:\n  timeout: banana\n",
			wantErr: "agent.timeout: invalid duration \"banana\"",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(config.EnvVar, writeConfig(t, tt.yaml))

			cfg, err := config.Load()

			require.Error(t, err)
			assert.ErrorContains(t, err, tt.wantErr)
			assert.Empty(t, cfg)
		})
	}
}

// TestLoadKeepsConfiguredAgentSession documents the string philosophy: an
// explicitly configured session is kept as written, and an explicit empty
// string is indistinguishable from an absent key, so it falls back to the
// default instead of being an error.
func TestLoadKeepsConfiguredAgentSession(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "explicit session is kept",
			yaml: "repo: owner/repo\nagent:\n  session: factory\n",
			want: "factory",
		},
		{
			name: "explicit default session is kept",
			yaml: "repo: owner/repo\nagent:\n  session: default\n",
			want: "default",
		},
		{
			name: "explicit empty session gets the default",
			yaml: "repo: owner/repo\nagent:\n  session: \"\"\n",
			want: "default",
		},
		{
			name: "omitted session gets the default",
			yaml: "repo: owner/repo\n",
			want: "default",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(config.EnvVar, writeConfig(t, tt.yaml))

			cfg, err := config.Load()

			require.NoError(t, err)
			assert.Equal(t, tt.want, cfg.Agent.Session)
		})
	}
}

// TestShippedConfigExampleIsValid keeps the documented example in sync with the
// loader: the repository config.yaml must parse and validate as written.
func TestShippedConfigExampleIsValid(t *testing.T) {
	file, err := os.Open(filepath.Join("..", "..", "config.yaml"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })

	cfg, err := config.Parse(file)
	require.NoError(t, err)

	cfg = config.ResolveDefaults(cfg)
	require.NoError(t, config.Validate(cfg))
}
