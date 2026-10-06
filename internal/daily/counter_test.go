package daily

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// storedState mirrors the on-disk payload for assertions.
type storedState struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

// counterPath returns a fresh counter file path inside the test's temp dir,
// so no test ever touches ~/.lights-out.
func counterPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "daily-claims.json")
}

// mustWriteRaw seeds the counter file with raw bytes.
func mustWriteRaw(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// readRaw returns the counter file contents as a string.
func readRaw(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(raw)
}

// readStored decodes the counter file into storedState.
func readStored(t *testing.T, path string) storedState {
	t.Helper()
	var s storedState
	require.NoError(t, json.Unmarshal([]byte(readRaw(t, path)), &s))
	return s
}

// mustParseRFC3339 parses a fixed timestamp or fails the test.
func mustParseRFC3339(t *testing.T, value string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, value)
	require.NoError(t, err)
	return tm
}

// clockAt builds a fixed clock from an RFC3339 literal.
func clockAt(t *testing.T, rfc3339 string) func() time.Time {
	t.Helper()
	fixed := mustParseRFC3339(t, rfc3339)
	return func() time.Time { return fixed }
}

// mutableClock is a fake clock tests can advance across the UTC boundary.
type mutableClock struct {
	mu  sync.Mutex
	now time.Time
}

func newMutableClock(now time.Time) *mutableClock {
	return &mutableClock{now: now}
}

func (c *mutableClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *mutableClock) set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// unsetEnv removes key for the duration of the test and restores it after.
// t.Setenv cannot unset a variable, so this helper does it manually. Like
// t.Setenv, it must only be used by non-parallel tests.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	previous, existed := os.LookupEnv(key)
	require.NoError(t, os.Unsetenv(key))
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(key, previous)
			return
		}
		_ = os.Unsetenv(key)
	})
}

func TestCurrentReturnsZeroOnFirstRunWhenFileIsAbsent(t *testing.T) {
	path := counterPath(t)
	c := NewWithClock(path, clockAt(t, "2026-10-02T10:00:00Z"))

	got, err := c.Current()

	require.NoError(t, err)
	assert.Equal(t, 0, got)
}

func TestIncrementCountsMonotonicallyOnTheSameUTCDay(t *testing.T) {
	path := counterPath(t)
	clock := newMutableClock(mustParseRFC3339(t, "2026-10-02T00:30:00Z"))
	c := NewWithClock(path, clock.Now)

	first, err := c.Increment()
	require.NoError(t, err)
	assert.Equal(t, 1, first)

	// Later the same UTC day: counting continues upward.
	clock.set(mustParseRFC3339(t, "2026-10-02T12:00:00Z"))
	second, err := c.Increment()
	require.NoError(t, err)
	assert.Equal(t, 2, second)

	clock.set(mustParseRFC3339(t, "2026-10-02T23:59:00Z"))
	third, err := c.Increment()
	require.NoError(t, err)
	assert.Equal(t, 3, third)

	assert.Equal(t, `{"date": "2026-10-02", "count": 3}`, readRaw(t, path))
}

func TestCounterResetsAcrossUTCMidnight(t *testing.T) {
	path := counterPath(t)
	clock := newMutableClock(mustParseRFC3339(t, "2026-10-02T23:59:00Z"))
	c := NewWithClock(path, clock.Now)

	_, err := c.Increment()
	require.NoError(t, err)
	second, err := c.Increment()
	require.NoError(t, err)
	require.Equal(t, 2, second)

	clock.set(mustParseRFC3339(t, "2026-10-03T00:01:00Z"))

	// The stored date no longer matches today, so the count resets to 0.
	current, err := c.Current()
	require.NoError(t, err)
	assert.Equal(t, 0, current)

	// Counting restarts from the reset base: the first claim of the new
	// UTC day is recorded as count 1.
	next, err := c.Increment()
	require.NoError(t, err)
	assert.Equal(t, 1, next)

	stored := readStored(t, path)
	assert.Equal(t, "2026-10-03", stored.Date)
	assert.Equal(t, 1, stored.Count)
}

func TestStoresUTCDateWhenLocalTimeHasCrossedMidnight(t *testing.T) {
	path := counterPath(t)
	// 2026-10-07T00:30:00+02:00 is still 2026-10-06 in UTC.
	c := NewWithClock(path, clockAt(t, "2026-10-07T00:30:00+02:00"))

	_, err := c.Increment()
	require.NoError(t, err)

	// Assert the stored date string: this fails if .UTC() is missing and the
	// local date leaks into the file.
	stored := readStored(t, path)
	assert.Equal(t, "2026-10-06", stored.Date)
	assert.Equal(t, 1, stored.Count)
}

func TestNonUTCZoneDoesNotShiftTheBoundary(t *testing.T) {
	path := counterPath(t)
	// Asia/Kolkata is UTC+05:30, so its calendar day differs from UTC near
	// midnight.
	zone := time.FixedZone("UTC+05:30", 5*3600+30*60)
	clock := newMutableClock(time.Date(2026, 10, 2, 23, 0, 0, 0, zone)) // UTC 2026-10-02T17:30:00Z
	c := NewWithClock(path, clock.Now)

	got, err := c.Increment()
	require.NoError(t, err)
	require.Equal(t, 1, got)
	require.Equal(t, "2026-10-02", readStored(t, path).Date)

	// Local calendar day flips to 10-03, but UTC is still 10-02: no reset.
	clock.set(time.Date(2026, 10, 3, 4, 0, 0, 0, zone)) // UTC 2026-10-02T22:30:00Z
	current, err := c.Current()
	require.NoError(t, err)
	assert.Equal(t, 1, current, "local midnight must not reset the counter")

	// Local date is unchanged (still 10-03) but UTC crosses midnight: reset.
	clock.set(time.Date(2026, 10, 3, 6, 0, 0, 0, zone)) // UTC 2026-10-03T00:30:00Z
	current, err = c.Current()
	require.NoError(t, err)
	assert.Equal(t, 0, current, "UTC midnight must reset the counter")
}

func TestCorruptJSONFailsLoudlyWithPathInMessage(t *testing.T) {
	corrupt := "{not json"

	t.Run("Current", func(t *testing.T) {
		path := counterPath(t)
		mustWriteRaw(t, path, corrupt)
		c := NewWithClock(path, clockAt(t, "2026-10-02T10:00:00Z"))

		got, err := c.Current()

		require.Error(t, err)
		assert.Contains(t, err.Error(), path)
		assert.Equal(t, 0, got)
	})

	t.Run("Increment", func(t *testing.T) {
		path := counterPath(t)
		mustWriteRaw(t, path, corrupt)
		c := NewWithClock(path, clockAt(t, "2026-10-02T10:00:00Z"))

		got, err := c.Increment()

		require.Error(t, err)
		assert.Contains(t, err.Error(), path)
		assert.Equal(t, 0, got)
		// Corruption must be reported, not overwritten with a fresh count.
		assert.Equal(t, corrupt, readRaw(t, path))
	})
}

func TestEmptyFileReadsAsZero(t *testing.T) {
	path := counterPath(t)
	mustWriteRaw(t, path, "")
	c := NewWithClock(path, clockAt(t, "2026-10-02T09:00:00Z"))

	got, err := c.Current()
	require.NoError(t, err)
	assert.Equal(t, 0, got)

	next, err := c.Increment()
	require.NoError(t, err)
	assert.Equal(t, 1, next)
}

func TestCurrentTreatsUnusableStoredDateAsRollover(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{name: "missing date", raw: `{"count": 3}`},
		{name: "empty date", raw: `{"date": "", "count": 3}`},
		{name: "garbage date", raw: `{"date": "banana", "count": 3}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := counterPath(t)
			mustWriteRaw(t, path, tc.raw)
			c := NewWithClock(path, clockAt(t, "2026-10-02T10:00:00Z"))

			got, err := c.Current()

			require.NoError(t, err)
			assert.Equal(t, 0, got)
		})
	}
}

func TestPathPrefersTheEnvironmentVariable(t *testing.T) {
	// No t.Parallel: t.Setenv panics under parallel tests.
	want := filepath.Join(t.TempDir(), "custom-claims.json")
	t.Setenv("LIGHTS_OUT_DAILY_CLAIMS", want)

	got, err := Path()

	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestPathDefaultsToHomeWhenEnvVarIsUnset(t *testing.T) {
	// No t.Parallel: this test mutates process-wide environment variables.
	home := t.TempDir()
	t.Setenv("HOME", home)
	unsetEnv(t, "LIGHTS_OUT_DAILY_CLAIMS")

	got, err := Path()

	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".lights-out", "daily-claims.json"), got)
}

func TestConcurrentIncrementsAllLandOnDisk(t *testing.T) {
	const workers = 50
	path := counterPath(t)
	c := NewWithClock(path, clockAt(t, "2026-10-02T15:00:00Z"))

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Increment(); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	// The assertion is on the value read back from disk, not on any in-memory
	// bookkeeping: only a real per-call lock can guarantee this.
	stored := readStored(t, path)
	assert.Equal(t, storedState{Date: "2026-10-02", Count: workers}, stored)
}

func TestCurrentDoesNotMutateTheFile(t *testing.T) {
	path := counterPath(t)
	mustWriteRaw(t, path, `{"date": "2026-10-02", "count": 7}`)
	before := readRaw(t, path)
	c := NewWithClock(path, clockAt(t, "2026-10-02T18:00:00Z"))

	got, err := c.Current()

	require.NoError(t, err)
	assert.Equal(t, 7, got)
	assert.Equal(t, before, readRaw(t, path), "Current must not rewrite the file")
}
