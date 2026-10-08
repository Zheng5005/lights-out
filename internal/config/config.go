// Package config loads and validates the lights-out dark factory configuration.
//
// The configuration contract lives in config.yaml at the repository root; the
// path can be overridden with the LIGHTS_OUT_CONFIG environment variable.
//
// Load order is deliberate and fixed:
//
//  1. resolve the path,
//  2. read and parse the file (unknown YAML fields are rejected),
//  3. apply defaults,
//  4. validate the resolved struct.
//
// Validation runs unconditionally, even when the factory is disabled: the
// "enabled" gate is a run-time concern of the pipeline, not a load-time one.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// EnvVar is the environment variable that overrides the config file path.
	EnvVar = "LIGHTS_OUT_CONFIG"
	// DefaultPath is used when EnvVar is unset or empty.
	DefaultPath = "./config.yaml"
)

// Default values applied to fields that are absent from the config file.
// They mirror the example configuration documented in config.yaml.
const (
	// DefaultConcurrencyLimit is how many issues may be in progress at once.
	DefaultConcurrencyLimit = 2
	// DefaultDailyLimit is how many issues may be claimed per UTC day.
	DefaultDailyLimit = 2

	DefaultTriggerLabel    = "dark-factory"
	DefaultInProgressLabel = "factory-in-progress"
	DefaultBlockedLabel    = "factory-blocked"

	DefaultDispatcher = "herdr"
	DefaultMode       = "headless"
	// DefaultAgentKind matches the dispatcher default; callers can switch to
	// "claude", "gemini", or any other Herdr agent kind.
	DefaultAgentKind = "agy"
	// DefaultAgentTimeout is the agent prompt/wait timeout. Default when omitted: 15m.
	DefaultAgentTimeout = 15 * time.Minute
	// DefaultAgentSession is the Herdr session the dispatcher targets. Default when omitted: default.
	DefaultAgentSession = "default"
	// DefaultWorktreeBase is the base ref worktrees are created from.
	DefaultWorktreeBase = "main"
)

// Config is the whole lights-out configuration.
type Config struct {
	Enabled          bool     `yaml:"enabled"`
	Repo             string   `yaml:"repo"` // "owner/repo"
	ConcurrencyLimit int      `yaml:"concurrency_limit"`
	DailyLimit       int      `yaml:"daily_limit"`
	Labels           Labels   `yaml:"labels"`
	Agent            Agent    `yaml:"agent"`
	Worktree         Worktree `yaml:"worktree"`
}

// Labels are the GitHub labels that drive the issue state machine.
type Labels struct {
	Trigger    string `yaml:"trigger"`     // opt-in marker, never removed by the factory
	InProgress string `yaml:"in_progress"` // lock + concurrency counter
	Blocked    string `yaml:"blocked"`     // terminal failure state
}

// Agent describes how Herdr is invoked to run work.
type Agent struct {
	Dispatcher string        `yaml:"dispatcher"` // "herdr"
	Mode       string        `yaml:"mode"`       // "headless"
	Kind       string        `yaml:"kind"`       // e.g. "agy", "claude", "gemini"
	Timeout    time.Duration // decoded from mirror; absent => default 15m
	Session    string        `yaml:"session"` // Herdr session the dispatcher targets
}

// Worktree configures where issue worktrees are created: the Phase 4 base ref
// (plan Open Decision #4) and the repository they are created from.
type Worktree struct {
	Base string `yaml:"base"` // defaults to "main"
	// Path is the local clone of the target repository named in repo — not a
	// clone of lights-out itself — that issue worktrees are created from.
	// Required with no default; "~" and "~/" expand to the user's home
	// directory during Parse. Existence is deliberately not checked at load
	// time: config load validates config correctness, never environment
	// existence, so the run-time worktree probe owns that check.
	Path string `yaml:"path"`
}

// ResolvePath returns the config file path: the value of EnvVar when it is set
// and non-empty, otherwise DefaultPath.
func ResolvePath() string {
	if fromEnv := os.Getenv(EnvVar); fromEnv != "" {
		return fromEnv
	}
	return DefaultPath
}

// configFile is the on-disk shape of the configuration. The two limits are
// pointers so an explicitly configured zero is distinguishable from an absent
// key: absent means "apply the default", explicit non-positive means the config
// is wrong and must be reported instead of silently replaced by a default.
// The worktree block needs no such mirror: worktree.path has no default, so an
// absent key and an explicitly empty one are the same invalid state and the
// pointer trick would have nothing to distinguish.
type configFile struct {
	Enabled          bool      `yaml:"enabled"`
	Repo             string    `yaml:"repo"`
	ConcurrencyLimit *int      `yaml:"concurrency_limit"`
	DailyLimit       *int      `yaml:"daily_limit"`
	Labels           Labels    `yaml:"labels"`
	Agent            agentFile `yaml:"agent"`
	Worktree         Worktree  `yaml:"worktree"`
}

// agentFile is the on-disk shape of the agent block. Timeout stays a string
// here for the same reason the limits are pointers above: an absent key must
// be distinguishable from an explicit unparsable value, so the strict-loader
// philosophy is preserved — an explicit "0s", negative, or garbage duration
// fails loudly instead of silently falling back to the default.
type agentFile struct {
	Dispatcher string `yaml:"dispatcher"`
	Mode       string `yaml:"mode"`
	Kind       string `yaml:"kind"`
	Timeout    string `yaml:"timeout"`
	Session    string `yaml:"session"`
}

// Parse decodes a YAML document into a Config. Unknown fields are rejected so
// a typo (for example concurrency_limmit) fails loudly instead of silently
// falling back to a default, and an explicitly configured non-positive limit is
// rejected instead of being promoted to its default.
func Parse(r io.Reader) (Config, error) {
	var file configFile

	decoder := yaml.NewDecoder(r)
	decoder.KnownFields(true)
	if err := decoder.Decode(&file); err != nil {
		if errors.Is(err, io.EOF) {
			// An empty document decodes to the zero Config; defaults plus
			// validation then report the missing fields.
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("parse yaml: %w", err)
	}

	cfg := Config{
		Enabled:  file.Enabled,
		Repo:     file.Repo,
		Labels:   file.Labels,
		Worktree: Worktree{Base: file.Worktree.Base},
		Agent: Agent{
			Dispatcher: file.Agent.Dispatcher,
			Mode:       file.Agent.Mode,
			Kind:       file.Agent.Kind,
			Session:    file.Agent.Session,
		},
	}
	if err := assignTimeout("agent.timeout", file.Agent.Timeout, &cfg.Agent.Timeout); err != nil {
		return Config{}, err
	}
	if err := assignLimit("concurrency_limit", file.ConcurrencyLimit, &cfg.ConcurrencyLimit); err != nil {
		return Config{}, err
	}
	if err := assignLimit("daily_limit", file.DailyLimit, &cfg.DailyLimit); err != nil {
		return Config{}, err
	}
	if err := assignPath("worktree.path", file.Worktree.Path, &cfg.Worktree.Path); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// assignLimit copies an explicitly configured limit into dst. A nil value means
// the key was absent, so dst stays zero and ResolveDefaults fills it later.
func assignLimit(key string, value *int, dst *int) error {
	if value == nil {
		return nil
	}
	if *value <= 0 {
		return fmt.Errorf("%s must be greater than 0, got %d", key, *value)
	}
	*dst = *value
	return nil
}

// assignTimeout parses an explicitly configured Go duration string into dst.
// An empty value means the key was absent (or explicitly empty), so dst stays
// zero and ResolveDefaults fills it later. An unparsable or non-positive
// value is an authoring error that names the key.
func assignTimeout(key string, value string, dst *time.Duration) error {
	if value == "" {
		return nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("%s: invalid duration %q", key, value)
	}
	if parsed <= 0 {
		return fmt.Errorf("%s must be greater than 0, got %v", key, parsed)
	}
	*dst = parsed
	return nil
}

// assignPath expands an explicitly configured path into dst. A value that is
// exactly "~" or starts with "~/" is resolved against the user's home
// directory, so downstream validation only ever sees the expanded value. An
// empty value means the key was absent (or explicitly empty) and dst stays
// zero: worktree.path has no default, so Validate reports it instead of
// silently substituting a path. Expansion that is required but impossible is
// an authoring error that names the key.
func assignPath(key string, value string, dst *string) error {
	if value == "" {
		return nil
	}
	if value == "~" || strings.HasPrefix(value, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("%s: cannot resolve home directory: %w", key, err)
		}
		value = filepath.Join(home, strings.TrimPrefix(value, "~"))
	}
	*dst = value
	return nil
}

// Load resolves the config path, parses it, applies defaults, and validates the
// result. Any failure is returned as an error; a missing file is an error.
func Load() (Config, error) {
	path := ResolvePath()

	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config %q: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	cfg, err := Parse(file)
	if err != nil {
		return Config{}, fmt.Errorf("config %q: %w", path, err)
	}

	cfg = ResolveDefaults(cfg)

	if err := Validate(cfg); err != nil {
		return Config{}, fmt.Errorf("config %q: %w", path, err)
	}
	return cfg, nil
}

// ResolveDefaults fills every field left empty by the config file. Explicit
// values are never overwritten, so the function is safe to call twice. A zero
// limit reaching this point always means the key was absent: Parse already
// rejects an explicitly configured non-positive limit.
func ResolveDefaults(c Config) Config {
	if c.ConcurrencyLimit == 0 {
		c.ConcurrencyLimit = DefaultConcurrencyLimit
	}
	if c.DailyLimit == 0 {
		c.DailyLimit = DefaultDailyLimit
	}
	if c.Labels.Trigger == "" {
		c.Labels.Trigger = DefaultTriggerLabel
	}
	if c.Labels.InProgress == "" {
		c.Labels.InProgress = DefaultInProgressLabel
	}
	if c.Labels.Blocked == "" {
		c.Labels.Blocked = DefaultBlockedLabel
	}
	if c.Agent.Dispatcher == "" {
		c.Agent.Dispatcher = DefaultDispatcher
	}
	if c.Agent.Mode == "" {
		c.Agent.Mode = DefaultMode
	}
	if c.Agent.Kind == "" {
		c.Agent.Kind = DefaultAgentKind
	}
	if c.Agent.Timeout == 0 {
		c.Agent.Timeout = DefaultAgentTimeout
	}
	if c.Agent.Session == "" {
		c.Agent.Session = DefaultAgentSession
	}
	if c.Worktree.Base == "" {
		c.Worktree.Base = DefaultWorktreeBase
	}
	// worktree.path deliberately gets no default: it is required, so an
	// absent key must fail validation instead of silently resolving to a
	// path the operator never chose.
	return c
}

// Validate checks the resolved config and returns the first violation found,
// with the offending key named so the fix is obvious. Rules:
//
//   - repo must be exactly "owner/repo" (one slash, both parts non-empty),
//   - concurrency_limit and daily_limit must be greater than zero,
//   - agent.timeout must be greater than zero,
//   - labels.trigger, labels.in_progress, labels.blocked must be non-blank,
//   - agent.dispatcher, agent.mode, agent.kind, agent.session and worktree.base
//     must be non-blank,
//   - worktree.path must be non-blank and an absolute path (the "~" expansion
//     already happened in Parse). Its existence is never checked here: config
//     load validates config correctness, never environment existence — the
//     run-time worktree probe owns that check.
func Validate(c Config) error {
	if err := validateRepo(c.Repo); err != nil {
		return err
	}
	if c.ConcurrencyLimit <= 0 {
		return fmt.Errorf("concurrency_limit must be greater than 0, got %d", c.ConcurrencyLimit)
	}
	if c.DailyLimit <= 0 {
		return fmt.Errorf("daily_limit must be greater than 0, got %d", c.DailyLimit)
	}
	if err := validateNonBlank("labels.trigger", c.Labels.Trigger); err != nil {
		return err
	}
	if err := validateNonBlank("labels.in_progress", c.Labels.InProgress); err != nil {
		return err
	}
	if err := validateNonBlank("labels.blocked", c.Labels.Blocked); err != nil {
		return err
	}
	if err := validateNonBlank("agent.dispatcher", c.Agent.Dispatcher); err != nil {
		return err
	}
	if err := validateNonBlank("agent.mode", c.Agent.Mode); err != nil {
		return err
	}
	if err := validateNonBlank("agent.kind", c.Agent.Kind); err != nil {
		return err
	}
	if c.Agent.Timeout <= 0 {
		// Defensive: Parse already rejects an explicit non-positive or
		// unparsable timeout, and ResolveDefaults fills an absent one, so a
		// zero reaching here means a caller built the Config by hand.
		return fmt.Errorf("agent.timeout must be greater than 0, got %v", c.Agent.Timeout)
	}
	if err := validateNonBlank("agent.session", c.Agent.Session); err != nil {
		return err
	}
	if err := validateNonBlank("worktree.base", c.Worktree.Base); err != nil {
		return err
	}
	return validateWorktreePath(c.Worktree.Path)
}

func validateRepo(repo string) error {
	owner, name, found := strings.Cut(repo, "/")
	if !found || strings.Contains(name, "/") {
		return fmt.Errorf("repo must be in %q form, got %q", "owner/repo", repo)
	}
	if strings.TrimSpace(owner) == "" || strings.TrimSpace(name) == "" {
		return fmt.Errorf("repo must have a non-empty owner and name in %q form, got %q", "owner/repo", repo)
	}
	return nil
}

func validateNonBlank(key, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s must not be empty", key)
	}
	return nil
}

// validateWorktreePath enforces the worktree.path rules on the value Parse
// already expanded: it must be non-blank and absolute. The filesystem is never
// consulted — existence, git-repo-ness, and writability are run-time probe
// concerns, not load-time ones.
func validateWorktreePath(path string) error {
	if err := validateNonBlank("worktree.path", path); err != nil {
		return err
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("worktree.path must be an absolute path, got %q", path)
	}
	return nil
}
