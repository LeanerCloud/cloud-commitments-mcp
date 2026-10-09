package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
)

// These tests use t.Setenv and so are deliberately not parallel.

func setCaps(t *testing.T, count, hourly, memory string) {
	t.Helper()
	t.Setenv(EnvMaxCount, count)
	t.Setenv(EnvMaxHourlyCommitment, hourly)
	t.Setenv(EnvMaxMemoryGB, memory)
}

// runReal drives ExecutePurchase for a real, confirmed purchase and reports
// whether the provider client was ever resolved or called.
func runReal(t *testing.T, rec common.Recommendation) (resp *PurchaseResponse, resolved bool, calls int, err error) {
	t.Helper()
	fake := &fakeServiceClient{purchaseResult: common.PurchaseResult{Success: true, CommitmentID: "id"}}
	resp, err = ExecutePurchase(context.Background(), PurchaseRequest{
		Region: rec.Region, Recommendation: rec, Confirm: true, CredentialScope: "scope",
		ResolveClient: func(_ context.Context) (provider.ServiceClient, error) {
			resolved = true
			return fake, nil
		},
	})
	return resp, resolved, fake.purchaseCalls, err
}

func spRec(t *testing.T, hourly float64) common.Recommendation {
	t.Helper()
	args := validSavingsPlansArgs()
	args.HourlyCommitment = hourly
	args.PaymentOption = "all-upfront"
	rec, _, _, _, err := savingsPlanRecommendationFromArgs(args)
	require.NoError(t, err)
	return rec
}

func TestSpendCapSavingsPlanHourly(t *testing.T) {
	// The issue's failing shape: 50000/h, 3y, all-upfront.
	setCaps(t, "1", "100", "1")
	rec := spRec(t, 50000)
	require.Equal(t, 0, rec.Count, "a Savings Plan carries Count 0; it must still hit the hourly cap")

	resp, resolved, calls, err := runReal(t, rec)
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), EnvMaxHourlyCommitment)
	assert.Contains(t, err.Error(), "50000")
	assert.Contains(t, err.Error(), "100")
	assert.False(t, resolved, "a refusal must not resolve credentials")
	assert.Zero(t, calls)

	_, _, calls, err = runReal(t, spRec(t, 100))
	require.NoError(t, err, "exactly at the cap is allowed")
	assert.Equal(t, 1, calls)

	_, _, _, err = runReal(t, spRec(t, 100.01))
	require.Error(t, err, "just over the cap is refused")
}

func TestSpendCapSavingsPlanWithoutDetailsRefused(t *testing.T) {
	setCaps(t, "1000", "1000", "1000")
	rec := spRec(t, 1)
	rec.Details = nil
	_, resolved, calls, err := runReal(t, rec)
	require.Error(t, err)
	assert.False(t, resolved)
	assert.Zero(t, calls)
}

func TestSpendCapRoutesByCommitmentTypeNotCount(t *testing.T) {
	// Mutation probe: a Savings Plan with Count 0 must not skip the hourly cap
	// and a non-SP with Count 0 must be refused, not waved through.
	setCaps(t, "1000", "1", "1000")
	rec := spRec(t, 5)
	rec.Count = 0
	_, _, _, err := runReal(t, rec)
	require.ErrorContains(t, err, EnvMaxHourlyCommitment)

	rec.Count = 5 // a non-zero count must not reroute it to the count cap
	_, _, _, err = runReal(t, rec)
	require.ErrorContains(t, err, EnvMaxHourlyCommitment)

	ec2, _, _, _, err2 := ec2RecommendationFromArgs(validEC2Args())
	require.NoError(t, err2)
	ec2.Count = 0
	_, _, calls, err := runReal(t, ec2)
	require.Error(t, err)
	assert.Zero(t, calls)
}

func TestSpendCapRejectsUnsetAndInvalid(t *testing.T) {
	ec2, _, _, _, err := ec2RecommendationFromArgs(validEC2Args())
	require.NoError(t, err)
	gcp, _, _, _, err := gcpComputeEngineRecommendationFromArgs(validGCPCUDArgs())
	require.NoError(t, err)
	gcp.Count = 1

	bad := []string{"", "   ", "abc", "0", "-1", "1e3", "1.5", "${user_config.max_count}"}
	for _, v := range bad {
		setCaps(t, v, "10", "10")
		_, resolved, calls, runErr := runReal(t, ec2)
		require.ErrorContains(t, runErr, EnvMaxCount, "count cap %q", v)
		if v != "" && v != "   " {
			require.ErrorContains(t, runErr, "must be a positive integer", "count cap %q", v)
		}
		assert.NotContains(t, runErr.Error(), "abc", "the invalid value must not be echoed")
		assert.False(t, resolved)
		assert.Zero(t, calls)
	}
	badFloat := append([]string{"NaN", "Inf", "-Inf"}, bad...)
	for _, v := range badFloat {
		if v == "1e3" || v == "1.5" {
			continue // valid positive numbers for float caps
		}
		setCaps(t, "10", v, "10")
		_, _, _, runErr := runReal(t, spRec(t, 1))
		require.ErrorContains(t, runErr, EnvMaxHourlyCommitment, "hourly cap %q", v)

		setCaps(t, "10", "10", v)
		_, _, _, runErr = runReal(t, gcp)
		require.ErrorContains(t, runErr, EnvMaxMemoryGB, "memory cap %q", v)
	}
	// Whitespace around a valid value is trimmed.
	setCaps(t, " 5 ", " 5 ", " 5 ")
	_, _, calls, err := runReal(t, ec2)
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
}

func TestSpendCapMessagesNameDesktopSetting(t *testing.T) {
	setCaps(t, "", "", "")
	ec2, _, _, _, err := ec2RecommendationFromArgs(validEC2Args())
	require.NoError(t, err)
	_, _, _, err = runReal(t, ec2)
	require.ErrorContains(t, err, capCount.desktop)
}

func TestSpendCapCountAcrossEveryCountBearingBuilder(t *testing.T) {
	var recs = map[string]common.Recommendation{}
	var err error
	recs["ec2"], _, _, _, err = ec2RecommendationFromArgs(validEC2Args())
	require.NoError(t, err)
	recs["rds"], _, _, _, err = rdsRecommendationFromArgs(validRDSArgs())
	require.NoError(t, err)
	recs["elasticache"], _, _, _, err = elasticacheRecommendationFromArgs(validElastiCacheArgs())
	require.NoError(t, err)
	recs["azure"], _, _, _, err = azureComputeRecommendationFromArgs(validAzureComputeArgs())
	require.NoError(t, err)
	recs["simple"], _, _, _, err = (&simpleAWSRIPurchaseTool{}).recommendationFromArgs(validSimpleArgs())
	require.NoError(t, err)
	recs["gcp"], _, _, _, err = gcpComputeEngineRecommendationFromArgs(validGCPCUDArgs())
	require.NoError(t, err)

	for name, rec := range recs {
		setCaps(t, "1000", "1000", "1000")
		over := rec
		over.Count = 1001
		_, resolved, calls, err := runReal(t, over)
		require.ErrorContains(t, err, EnvMaxCount, name)
		assert.False(t, resolved, name)
		assert.Zero(t, calls, name)

		atCap := rec
		atCap.Count = 1000
		_, _, calls, err = runReal(t, atCap)
		require.NoError(t, err, name)
		assert.Equal(t, 1, calls, name)
	}
}

func TestSpendCapGCPMemory(t *testing.T) {
	setCaps(t, "1000", "1", "64")
	args := validGCPCUDArgs()
	args.MemoryGB = 6000000 // small vcpu_count, huge memory: only MEMORY_GB bounds it
	rec, _, _, _, err := gcpComputeEngineRecommendationFromArgs(args)
	require.NoError(t, err)
	_, resolved, calls, err := runReal(t, rec)
	require.ErrorContains(t, err, EnvMaxMemoryGB)
	assert.False(t, resolved)
	assert.Zero(t, calls)

	args.MemoryGB = 64
	rec, _, _, _, err = gcpComputeEngineRecommendationFromArgs(args)
	require.NoError(t, err)
	_, _, calls, err = runReal(t, rec)
	require.NoError(t, err)
	assert.Equal(t, 1, calls)

	rec.Details = &common.ComputeDetails{MemoryGB: 1}
	_, _, _, err = runReal(t, rec)
	require.Error(t, err, "unexpected Details shape fails closed")
}

func TestSavingsPlanPreviewShowsTotalCommitmentOffline(t *testing.T) {
	setCaps(t, "", "", "") // preview needs no caps
	rec := spRec(t, 50000)
	resp, err := ExecutePurchase(context.Background(), PurchaseRequest{
		Region: rec.Region, Recommendation: rec, DryRun: true,
		ResolveClient: func(_ context.Context) (provider.ServiceClient, error) {
			t.Fatal("preview must not resolve a client")
			return nil, nil
		},
	})
	require.NoError(t, err)
	require.NotNil(t, resp.Cost)
	assert.InDelta(t, 50000.0*8760*3, *resp.Cost, 0.5)
}

func TestSavingsPlanCostDoesNotChangeIdempotencyKey(t *testing.T) {
	rec := spRec(t, 10)
	withCost := rec
	withCost.CommitmentCost = 0
	assert.Equal(t,
		idempotencyKeyFor("us-east-1", rec, "s", ""),
		idempotencyKeyFor("us-east-1", withCost, "s", ""))
}
