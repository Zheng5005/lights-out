// Package daily tracks how many issues the factory has claimed during the
// current UTC day. The counter is a small JSON file guarded by flock so that
// concurrent factory runs cannot lose an increment or overrun daily_limit.
package daily

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// envDailyClaims overrides the counter file location.
	envDailyClaims = "LIGHTS_OUT_DAILY_CLAIMS"

	// lockSuffix names the sidecar lock file kept next to the counter.
	lockSuffix = ".lock"
)

// Path resolves the counter file: $LIGHTS_OUT_DAILY_CLAIMS, else
// $HOME/.lights-out/daily-claims.json.
func Path() (string, error) {
	if p := os.Getenv(envDailyClaims); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".lights-out", "daily-claims.json"), nil
}

// Counter persists the number of issues claimed on the current UTC day.
// It is safe for concurrent use: every call locks and closes its own file
// descriptors, and the Counter itself holds no mutable state.
type Counter struct {
	path string
	now  func() time.Time
}

// New returns a Counter writing to path, using time.Now as its clock.
func New(path string) *Counter {
	return NewWithClock(path, time.Now)
}

// NewWithClock is the test seam. now must never be nil in production callers.
func NewWithClock(path string, now func() time.Time) *Counter {
	return &Counter{path: path, now: now}
}

// state is the on-disk payload: {"date": "2026-10-02", "count": 1}.
type state struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

// Current reads the count for the current UTC day without mutating the
// counter file. An absent, empty, or stale-date file yields 0; corrupt JSON
// fails loud with the file path in the error.
func (c *Counter) Current() (int, error) {
	var count int
	err := c.withLock(unix.LOCK_SH, func() error {
		s, err := c.read()
		if err != nil {
			return err
		}
		if s.Date == c.today() {
			count = s.Count
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		// The counter directory does not exist yet: first run.
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return count, nil
}

// Increment bumps the count for the current UTC day, resetting to 0 when the
// UTC date differs from the stored date, and returns the new count.
func (c *Counter) Increment() (int, error) {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return 0, fmt.Errorf("create daily counter directory: %w", err)
	}
	var count int
	err := c.withLock(unix.LOCK_EX, func() error {
		s, err := c.read()
		if err != nil {
			return err
		}
		today := c.today()
		if s.Date != today {
			// Rollover or first run: the base resets to 0 before the bump,
			// so the first claim of a UTC day is recorded as count 1.
			s = state{Date: today}
		}
		s.Count++
		if err := c.write(s); err != nil {
			return err
		}
		count = s.Count
		return nil
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

// withLock runs fn while holding flock on the sidecar lock file next to the
// counter: LOCK_EX for writers, LOCK_SH for readers. Both block. The kernel
// drops flock automatically when the fd closes, so a killed process never
// leaves a stale lock behind; the explicit LOCK_UN just releases it earlier
// than the deferred close.
//
// Two traps make this function's shape load-bearing:
//
//  1. flock belongs to the open file description, not the process. If a
//     Counter shared one fd for its lifetime, concurrent callers would share
//     that description, flock would succeed instantly for everyone, and the
//     lock would protect nothing while still looking correct. So every call
//     opens its own fd here.
//  2. The lock is taken on a sidecar ("<path>.lock"), never on the counter
//     file itself, because writes replace the counter via os.Rename. Rename
//     repoints the path at a NEW inode while flock still guards the old one,
//     so the next caller would lock the new inode (uncontended) and read
//     stale data — a lost update even though everyone took "the lock". The
//     sidecar inode never changes, so every caller contends on one lock.
func (c *Counter) withLock(how int, fn func() error) error {
	lock, err := os.OpenFile(c.path+lockSuffix, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("open daily counter lock %q: %w", c.path+lockSuffix, err)
	}
	defer lock.Close()

	if err := unix.Flock(int(lock.Fd()), how); err != nil {
		return fmt.Errorf("lock daily counter %q: %w", c.path, err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)

	return fn()
}

// read loads the counter file. An absent or zero-byte file is the zero state
// (first run).
func (c *Counter) read() (state, error) {
	raw, err := os.ReadFile(c.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return state{}, nil
		}
		return state{}, fmt.Errorf("read daily counter %q: %w", c.path, err)
	}
	if len(raw) == 0 {
		return state{}, nil
	}
	var s state
	if err := json.Unmarshal(raw, &s); err != nil {
		// Corruption means a real bug: fail loud instead of silently
		// resetting, because a silent reset would over-claim past the limit.
		return state{}, fmt.Errorf("parse daily counter %q: %w", c.path, err)
	}
	return s, nil
}

// write serializes s to a temp file in the same directory and renames it over
// the counter while the caller still holds LOCK_EX. The rename keeps readers
// from ever observing a half-written file, even if the process dies mid-write.
func (c *Counter) write(s state) error {
	tmp, err := os.CreateTemp(filepath.Dir(c.path), ".daily-claims-*.tmp")
	if err != nil {
		return fmt.Errorf("create daily counter temp file: %w", err)
	}
	// No-op after a successful rename; removes the litter on failure paths.
	defer os.Remove(tmp.Name())

	// Exact documented storage shape. s.Date is always time.DateOnly output,
	// so %q produces valid JSON for this field.
	payload := fmt.Sprintf(`{"date": %q, "count": %d}`, s.Date, s.Count)
	if _, err := tmp.WriteString(payload); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write daily counter %q: %w", c.path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close daily counter temp file: %w", err)
	}
	if err := os.Rename(tmp.Name(), c.path); err != nil {
		return fmt.Errorf("replace daily counter %q: %w", c.path, err)
	}
	return nil
}

// today is the current UTC calendar date as a time.DateOnly string. The
// stored date is compared as a string, never parsed: a malformed stored date
// can never equal today, so a broken file simply rolls over instead of
// erroring. UTC is taken explicitly so the boundary is UTC midnight, not the
// host's local midnight.
func (c *Counter) today() string {
	return c.now().UTC().Format(time.DateOnly)
}
