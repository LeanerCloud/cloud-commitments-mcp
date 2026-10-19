//go:build linux || darwin

package tools

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
)

func persistedBudget(t *testing.T) string {
	t.Helper()
	resetBudget(t, "100")
	path := filepath.Join(t.TempDir(), "budget.json")
	t.Setenv(EnvBudgetState, path)
	t.Setenv(EnvAuditLog, "")
	return path
}

func TestPersistedBudgetSurvivesRestart(t *testing.T) {
	path := persistedBudget(t)
	fake := pricedFake(60, common.PurchaseResult{Success: true, CommitmentID: "id"}, nil)
	require.NoError(t, runWithNonce(fake, "a"))

	processBudget = &budgetLedger{} // a restart loses memory, not the file
	err := runWithNonce(fake, "b")
	require.ErrorContains(t, err, EnvBudgetUSD)
	assert.Equal(t, 1, fake.purchaseCalls)
	assert.InDelta(t, 60, readState(t, path).SpentUSD, 1e-9)
	assert.Zero(t, processBudget.committedUSD(), "persisted mode must not use the in-memory ledger")
}

func TestPersistedBudgetFailedPurchaseKeepsCounted(t *testing.T) {
	path := persistedBudget(t)
	fake := pricedFake(60, common.PurchaseResult{}, assert.AnError)
	require.Error(t, runWithNonce(fake, "a"))
	st := readState(t, path)
	assert.InDelta(t, 60, st.SpentUSD, 1e-9)
	assert.Empty(t, st.Reservations)
}

func TestPersistedBudgetResetsNextMonth(t *testing.T) {
	persistedBudget(t)
	old := budgetNow
	t.Cleanup(func() { budgetNow = old })
	fake := pricedFake(100, common.PurchaseResult{Success: true, CommitmentID: "id"}, nil)

	budgetNow = fixedClock("2026-10-31T23:00:00Z")
	require.NoError(t, runWithNonce(fake, "a"))
	require.ErrorContains(t, runWithNonce(fake, "b"), EnvBudgetUSD)

	budgetNow = func() time.Time { return time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) }
	require.NoError(t, runWithNonce(fake, "c"))
	assert.Equal(t, 2, fake.purchaseCalls)
}

func TestPersistedBudgetCorruptFileRefusesBeforePurchase(t *testing.T) {
	path := persistedBudget(t)
	require.NoError(t, os.WriteFile(path, []byte("{broken"), 0o600))
	fake := pricedFake(1, common.PurchaseResult{Success: true, CommitmentID: "id"}, nil)
	err := runWithNonce(fake, "a")
	require.ErrorContains(t, err, "refusing real purchase")
	require.ErrorContains(t, err, "corrupt")
	assert.Zero(t, fake.purchaseCalls)
}

func TestUnsetBudgetStateStaysInMemory(t *testing.T) {
	resetBudget(t, "100")
	t.Setenv(EnvBudgetState, "  ")
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("HOME", state)
	t.Setenv(EnvAuditLog, "")
	fake := pricedFake(60, common.PurchaseResult{Success: true, CommitmentID: "id"}, nil)
	require.NoError(t, runWithNonce(fake, "a"))
	assert.InDelta(t, 60, processBudget.committedUSD(), 1e-9)
	entries, err := os.ReadDir(state)
	require.NoError(t, err)
	assert.Empty(t, entries, "no state file may be created when %s is unset", EnvBudgetState)
}
