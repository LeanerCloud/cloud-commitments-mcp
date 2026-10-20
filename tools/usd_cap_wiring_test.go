package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
)

// runRealWith drives a confirmed real purchase against fake and reports
// whether the client was resolved.
func runRealWith(fake *fakeServiceClient, rec common.Recommendation) (resolved bool, err error) {
	_, err = ExecutePurchase(context.Background(), PurchaseRequest{
		Region: rec.Region, Recommendation: rec, Confirm: true, CredentialScope: "scope",
		ResolveClient: func(_ context.Context) (provider.ServiceClient, error) {
			resolved = true
			return fake, nil
		},
	})
	return resolved, err
}

// The issue's shape: a small count cap admits 50 x u-24tb1 (~$286M).
func TestUSDCapRegressionExpensiveRI(t *testing.T) {
	t.Setenv(EnvMaxCommitmentUSD, "1000")
	fake := &fakeServiceClient{
		purchaseResult: common.PurchaseResult{Success: true, CommitmentID: "id"},
		offering:       &common.OfferingDetails{TotalCost: 286e6 / 50, Currency: "USD"},
	}
	rec := testRecommendation()
	rec.ResourceType = "u-24tb1.112xlarge"
	rec.Count = 50

	resolved, err := runRealWith(fake, rec)
	require.ErrorContains(t, err, EnvMaxCommitmentUSD)
	assert.Zero(t, fake.purchaseCalls)
	assert.True(t, resolved, "pricing needs the client")
	assert.Equal(t, 1, fake.offeringCalls)
}

func TestUSDCapUnpriceableRefusedBeforePurchase(t *testing.T) {
	t.Setenv(EnvMaxCommitmentUSD, "1000")
	fake := &fakeServiceClient{offeringErr: assert.AnError}
	_, err := runRealWith(fake, testRecommendation())
	require.ErrorContains(t, err, "cannot determine USD total")
	assert.Zero(t, fake.purchaseCalls)
}

func TestUSDCapUnsetRefusesBeforeResolve(t *testing.T) {
	t.Setenv(EnvMaxCommitmentUSD, "")
	fake := &fakeServiceClient{}
	resolved, err := runRealWith(fake, testRecommendation())
	require.ErrorContains(t, err, EnvMaxCommitmentUSD)
	assert.False(t, resolved)
	assert.Zero(t, fake.offeringCalls)
	assert.Zero(t, fake.purchaseCalls)
}

func TestUSDCapEnvParsing(t *testing.T) {
	for _, v := range []string{"", " ", "abc", "NaN", "Inf", "-1", "0"} {
		t.Setenv(EnvMaxCommitmentUSD, v)
		_, err := readUSDCap()
		require.ErrorContains(t, err, EnvMaxCommitmentUSD, "value %q", v)
	}
	t.Setenv(EnvMaxCommitmentUSD, "1e3")
	got, err := readUSDCap()
	require.NoError(t, err)
	assert.InDelta(t, 1000, got, 1e-9)
}

// sequential: writes the shared EnvAuditLog file
func TestUSDCapRefusesRealGCPCUDPurchase(t *testing.T) {
	rec, _, _, _, err := gcpComputeEngineRecommendationFromArgs(validGCPCUDArgs())
	require.NoError(t, err)
	fake := &fakeServiceClient{}
	_, err = runRealWith(fake, rec)
	require.ErrorContains(t, err, "go#321")
	assert.Zero(t, fake.purchaseCalls)
	assert.Zero(t, fake.offeringCalls)
}
