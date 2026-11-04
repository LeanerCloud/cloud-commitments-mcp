//go:build linux || darwin

package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	sdkec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
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

// TestPersistedBudgetReleasedWhenUpfrontGuardRefuses runs the real EC2 client
// behind its API fake: the go#611 guard refuses before any purchase call, so
// nothing may stay committed.
func TestPersistedBudgetReleasedWhenUpfrontGuardRefuses(t *testing.T) {
	path := persistedBudget(t)
	api := newPricedEC2API(PaymentOptionAllUpfront, 500)
	tool, _ := ec2ToolOverFakeAPI(api)
	args := upfrontEC2Args(PaymentOptionAllUpfront, "budget-refused")
	_, preview, err := tool.handle(context.Background(), nil, args)
	require.NoError(t, err)

	api.fixedPrice = 526 // +5.2% over the approved 1500
	t.Setenv(EnvBudgetUSD, "100000")
	_, _, err = tool.handle(context.Background(), nil, confirmed(args, preview.ApprovedUpfront, preview.ApprovedCurrency))
	require.ErrorContains(t, err, "exceeds the approved upfront")
	assert.Zero(t, api.purchaseCalls)
	st := readState(t, path)
	assert.Zero(t, st.SpentUSD)
	assert.Empty(t, st.Reservations)
}

// TestPersistedBudgetKeepsCountedWhenPurchaseAPIFailsAfterGuard: the guard
// passes and the purchase call itself fails, so money may have moved.
func TestPersistedBudgetKeepsCountedWhenPurchaseAPIFailsAfterGuard(t *testing.T) {
	path := persistedBudget(t)
	t.Setenv(EnvBudgetUSD, "100000")
	api := &failingPurchaseEC2API{pricedEC2API: newPricedEC2API(PaymentOptionAllUpfront, 500)}
	tool, _ := ec2ToolOverFakeAPI(api)
	args := upfrontEC2Args(PaymentOptionAllUpfront, "budget-api-fail")
	_, preview, err := tool.handle(context.Background(), nil, args)
	require.NoError(t, err)

	_, _, err = tool.handle(context.Background(), nil, confirmed(args, preview.ApprovedUpfront, preview.ApprovedCurrency))
	require.Error(t, err)
	assert.Equal(t, 1, api.purchaseCalls)
	assert.Greater(t, readState(t, path).SpentUSD, 0.0)
}

type failingPurchaseEC2API struct{ *pricedEC2API }

func (f *failingPurchaseEC2API) PurchaseReservedInstancesOffering(context.Context, *sdkec2.PurchaseReservedInstancesOfferingInput, ...func(*sdkec2.Options)) (*sdkec2.PurchaseReservedInstancesOfferingOutput, error) {
	f.purchaseCalls++
	return nil, errors.New("api down")
}
