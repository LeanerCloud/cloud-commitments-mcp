//go:build linux || darwin

package tools

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const budgetHelperEnv = "CUDLY_TEST_BUDGET_HELPER"

func fixedClock(s string) func() time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return func() time.Time { return t }
}

func stateFile(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "state", "budget.json")
}

func readState(t *testing.T, path string) budgetState {
	t.Helper()
	st, err := newFileBudget(path).load()
	require.NoError(t, err)
	return st
}

// TestBudgetHelperProcess is the child of TestFileBudgetTwoProcessesShareLimit,
// not a test: it runs only when budgetHelperEnv carries "path|attempts".
// sequential: subprocess entry point driven by an env var, never a parallel test.
func TestBudgetHelperProcess(t *testing.T) {
	arg := os.Getenv(budgetHelperEnv)
	if arg == "" {
		t.Skip("helper process only")
	}
	path, attempts, _ := strings.Cut(arg, "|")
	n, err := strconv.Atoi(attempts)
	require.NoError(t, err)
	fb := newFileBudget(path)
	won := 0
	for range n {
		r, err := fb.reserve(10, 100)
		if err != nil {
			continue
		}
		r.settle(true)
		won++
	}
	fmt.Printf("WON=%d\n", won)
}

func TestFileBudgetTwoProcessesShareLimit(t *testing.T) {
	t.Parallel()
	path := stateFile(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const procs, attempts = 4, 15
	var wg sync.WaitGroup
	wins := make([]int, procs)
	errs := make([]error, procs)
	for i := range procs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBudgetHelperProcess$", "-test.timeout=80s")
			cmd.Env = append(os.Environ(), fmt.Sprintf("%s=%s|%d", budgetHelperEnv, path, attempts))
			out, err := cmd.CombinedOutput()
			if err != nil {
				errs[i] = fmt.Errorf("helper failed: %w: %s", err, out)
				return
			}
			sc := bufio.NewScanner(strings.NewReader(string(out)))
			for sc.Scan() {
				if v, ok := strings.CutPrefix(sc.Text(), "WON="); ok {
					wins[i], errs[i] = strconv.Atoi(v)
				}
			}
		}()
	}
	wg.Wait()

	total := 0
	for i := range procs {
		require.NoError(t, errs[i])
		total += wins[i]
	}
	assert.Equal(t, 10, total, "limit 100 / 10 per purchase")
	st := readState(t, path)
	assert.Equal(t, usdCents(10000), st.SpentUSD)
	assert.Empty(t, st.Reservations)
}

func TestFileBudgetRestartDoesNotRefill(t *testing.T) {
	t.Parallel()
	path := stateFile(t)
	first := newFileBudget(path)
	r, err := first.reserve(60, 100)
	require.NoError(t, err)
	r.settle(true)

	_, err = newFileBudget(path).reserve(60, 100)
	require.ErrorContains(t, err, EnvBudgetUSD)
	r2, err := newFileBudget(path).reserve(40, 100)
	require.NoError(t, err)
	r2.settle(true)
}

func TestFileBudgetUnsettledReservationCountsAsSpent(t *testing.T) {
	t.Parallel()
	path := stateFile(t)
	_, err := newFileBudget(path).reserve(70, 100) // process "crashed" before settle
	require.NoError(t, err)

	_, err = newFileBudget(path).reserve(40, 100)
	require.ErrorContains(t, err, EnvBudgetUSD)
	require.Len(t, readState(t, path).Reservations, 1)
}

func TestFileBudgetCleanNoopReleases(t *testing.T) {
	t.Parallel()
	path := stateFile(t)
	fb := newFileBudget(path)
	r, err := fb.reserve(70, 100)
	require.NoError(t, err)
	r.settle(false)
	st := readState(t, path)
	assert.Zero(t, st.SpentUSD)
	assert.Empty(t, st.Reservations)
}

func TestFileBudgetMonthRollover(t *testing.T) {
	t.Parallel()
	path := stateFile(t)
	fb := newFileBudget(path)
	fb.now = fixedClock("2026-10-31T23:59:59Z")
	r, err := fb.reserve(100, 100)
	require.NoError(t, err)
	r.settle(true)
	_, err = fb.reserve(1, 100)
	require.ErrorContains(t, err, "2026-10")

	fb.now = fixedClock("2026-11-01T00:00:00Z")
	r, err = fb.reserve(100, 100)
	require.NoError(t, err)
	r.settle(true)
	st := readState(t, path)
	assert.Equal(t, "2026-11", st.Month)
	assert.Equal(t, usdCents(10000), st.SpentUSD)
}

func TestFileBudgetMonthIsUTC(t *testing.T) {
	t.Parallel()
	fb := newFileBudget(stateFile(t))
	zone := time.FixedZone("UTC+14", 14*3600)
	local := time.Date(2026, 11, 1, 5, 0, 0, 0, zone) // still 2026-10-31 in UTC
	fb.now = func() time.Time { return local }
	assert.Equal(t, "2026-10", fb.month())
}

func TestFileBudgetSettleAfterRolloverCountsInNewMonth(t *testing.T) {
	t.Parallel()
	path := stateFile(t)
	fb := newFileBudget(path)
	fb.now = fixedClock("2026-10-31T23:59:59Z")
	r, err := fb.reserve(30, 100)
	require.NoError(t, err)
	fb.now = fixedClock("2026-11-01T00:00:01Z")
	r.settle(true)
	st := readState(t, path)
	assert.Equal(t, "2026-11", st.Month)
	assert.Equal(t, usdCents(3000), st.SpentUSD)
	assert.Empty(t, st.Reservations)
}

func TestFileBudgetCorruptFileRefuses(t *testing.T) {
	t.Parallel()
	for name, content := range map[string]string{
		"garbage":         "not json",
		"empty":           "",
		"trailing":        `{"version":1,"month":"2026-10","spent_usd":0,"reservations":[]} x`,
		"unknown field":   `{"version":1,"month":"2026-10","spent_usd":0,"reservations":[],"extra":1}`,
		"duplicate ids":   `{"version":1,"month":"2026-10","spent_usd":0,"reservations":[{"id":"a","usd":1},{"id":"a","usd":1}]}`,
		"bad version":     `{"version":9,"month":"2026-10","spent_usd":0,"reservations":[]}`,
		"bad month":       `{"version":1,"month":"October","spent_usd":0,"reservations":[]}`,
		"negative spent":  `{"version":1,"month":"2026-10","spent_usd":-5,"reservations":[]}`,
		"negative reserv": `{"version":1,"month":"2026-10","spent_usd":0,"reservations":[{"id":"a","usd":-1}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := stateFile(t)
			require.NoError(t, ensureAuditLogDirectory(path))
			require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
			_, err := newFileBudget(path).reserve(1, 100)
			require.ErrorContains(t, err, "corrupt")
			got, rerr := os.ReadFile(path)
			require.NoError(t, rerr)
			assert.Equal(t, content, string(got), "a corrupt file is never overwritten")
		})
	}
}

func TestFileBudgetUnreadableFileRefuses(t *testing.T) {
	t.Parallel()
	path := stateFile(t)
	require.NoError(t, ensureAuditLogDirectory(path))
	require.NoError(t, os.Mkdir(path, 0o700)) // a directory where the file should be
	_, err := newFileBudget(path).reserve(1, 100)
	require.ErrorContains(t, err, "budget state file")
}

func TestFileBudgetPermissions(t *testing.T) {
	t.Parallel()
	path := stateFile(t)
	r, err := newFileBudget(path).reserve(1, 100)
	require.NoError(t, err)
	r.settle(true)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	dir, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), dir.Mode().Perm())
	left, err := filepath.Glob(path + ".tmp-*")
	require.NoError(t, err)
	assert.Empty(t, left)
}

func TestFileBudgetRolloverCarriesUnsettledReservations(t *testing.T) {
	t.Parallel()
	path := stateFile(t)
	fb := newFileBudget(path)
	fb.now = fixedClock("2026-10-15T12:00:00Z")
	r, err := fb.reserve(90, 100) // never settled: the process crashed
	require.NoError(t, err)

	fb.now = fixedClock("2026-11-01T00:00:00Z")
	_, err = fb.reserve(20, 100)
	require.ErrorContains(t, err, EnvBudgetUSD)
	require.Len(t, readState(t, path).Reservations, 1)

	r.settle(false) // resolving it in the new month frees the headroom
	_, err = fb.reserve(20, 100)
	require.NoError(t, err)
}

func TestFileBudgetFutureMonthFileRefusesUnchanged(t *testing.T) {
	t.Parallel()
	const content = `{"version":1,"month":"2099-01","spent_usd":5,"reservations":[]}`
	path := stateFile(t)
	require.NoError(t, ensureAuditLogDirectory(path))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	_, err := newFileBudget(path).reserve(1, 100)
	require.ErrorContains(t, err, "2099-01")
	require.ErrorContains(t, err, "current UTC month")
	got, rerr := os.ReadFile(path)
	require.NoError(t, rerr)
	assert.Equal(t, content, string(got))
}

func TestFileBudgetClockBehindFileRefuses(t *testing.T) {
	t.Parallel()
	path := stateFile(t)
	fb := newFileBudget(path)
	fb.now = fixedClock("2026-11-02T00:00:00Z")
	r, err := fb.reserve(10, 100)
	require.NoError(t, err)
	r.settle(true)
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	fb.now = fixedClock("2026-10-31T23:00:00Z")
	_, err = fb.reserve(1, 100)
	require.ErrorContains(t, err, "2026-11")
	require.ErrorContains(t, err, "2026-10")
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
}

func TestFileBudgetLockFileSymlinkRefused(t *testing.T) {
	t.Parallel()
	path := stateFile(t)
	require.NoError(t, ensureAuditLogDirectory(path))
	target := filepath.Join(t.TempDir(), "elsewhere")
	require.NoError(t, os.WriteFile(target, nil, 0o600))
	require.NoError(t, os.Symlink(target, path+".lock"))
	_, err := newFileBudget(path).reserve(1, 100)
	require.ErrorContains(t, err, "budget lock file")
}

// #82: the persisted ledger sums whole cents and writes exact decimals.
func TestFileBudgetExactCents(t *testing.T) {
	path := stateFile(t)
	fb := newFileBudget(path)
	a, err := fb.reserve(0.1, 0.3)
	require.NoError(t, err)
	b, err := fb.reserve(0.2, 0.3)
	require.NoError(t, err, "0.10 + 0.20 is exactly the 0.30 budget")
	a.settle(true)
	b.settle(true)
	raw, err := os.ReadFile(path) // #nosec G304 -- test temp file
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"spent_usd": 0.3,`)
}

// A file written before #82 may hold a non-cent amount; it reads rounded up.
func TestFileBudgetLegacyAmountRoundsUp(t *testing.T) {
	path := stateFile(t)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	month := time.Now().UTC().Format("2006-01")
	legacy := fmt.Sprintf(`{"month": %q, "reservations": [{"id": "r1", "usd": 0.001}], "spent_usd": 0.105, "version": 1}`, month)
	require.NoError(t, os.WriteFile(path, []byte(legacy), 0o600))
	_, err := newFileBudget(path).reserve(0.01, 0.129)
	require.Error(t, err, "0.105 reads as 0.11 and 0.001 as 0.01, so 0.01 more exceeds 0.12")
	_, err = newFileBudget(path).reserve(0.01, 0.131)
	require.NoError(t, err, "0.11 + 0.01 + 0.01 fits 0.13")
}

func TestFileBudgetNullAmountIsCorrupt(t *testing.T) {
	path := stateFile(t)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	month := time.Now().UTC().Format("2006-01")
	require.NoError(t, os.WriteFile(path, []byte(fmt.Sprintf(`{"month": %q, "reservations": [], "spent_usd": null, "version": 1}`, month)), 0o600))
	_, err := newFileBudget(path).reserve(1, 100)
	require.ErrorContains(t, err, "corrupt")
}
