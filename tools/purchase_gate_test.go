package tools

import (
	"context"
	"os"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setPurchaseGateEnv sets EnvEnableRealPurchases to value for the calling
// test, restoring whatever the variable held before (present or absent) on
// cleanup. value == "" removes the variable entirely rather than setting it
// to an empty string, since this suite needs to pin the real "operator never
// configured this" default, not merely an empty value that happens to behave
// the same way. Written by hand rather than via t.Setenv because t.Setenv
// cannot represent "the variable was absent".
func setPurchaseGateEnv(t *testing.T, value string) {
	t.Helper()
	prev, had := os.LookupEnv(EnvEnableRealPurchases)
	if value == "" {
		require.NoError(t, os.Unsetenv(EnvEnableRealPurchases))
	} else {
		require.NoError(t, os.Setenv(EnvEnableRealPurchases, value))
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(EnvEnableRealPurchases, prev)
		} else {
			_ = os.Unsetenv(EnvEnableRealPurchases)
		}
	})
}

// TestExecutePurchaseRealPurchaseGate is the regression guard for the
// operator authorization gap found in review: before EnvEnableRealPurchases
// existed, a confirmed real purchase (dry_run=false, confirm=true) executed
// immediately no matter what the operator running this MCP server process
// wanted -- the model's own confirm flag was the only thing standing between
// a prompt-injected or simply hallucinating model with ambient production
// credentials and a real purchase. This pins that ExecutePurchase now
// refuses a real purchase, and never resolves a client or calls the
// provider, unless the operator has explicitly set EnvEnableRealPurchases to
// "1" or "true"; and that dry runs are unaffected either way.
//
// Deliberately not parallel: every subtest mutates the process-wide
// environment variable this package's TestMain also sets a default for.
// Go's serial tests all finish before any t.Parallel() test in this binary
// begins, so running this test (and its subtests) serially guarantees no
// parallel test ever observes one of these transient overrides.
func TestExecutePurchaseRealPurchaseGate(t *testing.T) {
	rec := testRecommendation()
	realPurchaseRequest := func(fake *fakeServiceClient, dryRun bool) PurchaseRequest {
		return PurchaseRequest{
			Region: "us-east-1", Recommendation: rec, DryRun: dryRun, Confirm: true, CredentialScope: "test-scope",
			ResolveClient: func(_ context.Context) (provider.ServiceClient, error) { return fake, nil },
		}
	}

	t.Run("unset refuses a real purchase and never calls the provider", func(t *testing.T) {
		setPurchaseGateEnv(t, "")
		fake := &fakeServiceClient{purchaseResult: common.PurchaseResult{Success: true}}

		resp, err := ExecutePurchase(context.Background(), realPurchaseRequest(fake, false))

		require.Error(t, err)
		assert.Nil(t, resp)
		assert.Contains(t, err.Error(), EnvEnableRealPurchases,
			"the refusal must name the flag the operator needs to set")
		assert.Equal(t, 0, fake.purchaseCalls, "the provider must never be called while the gate is disabled")
	})

	t.Run("\"1\" enables a real purchase", func(t *testing.T) {
		setPurchaseGateEnv(t, "1")
		fake := &fakeServiceClient{purchaseResult: common.PurchaseResult{Success: true, CommitmentID: "ri-gate-1"}}

		resp, err := ExecutePurchase(context.Background(), realPurchaseRequest(fake, false))

		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.True(t, resp.Success)
		assert.Equal(t, 1, fake.purchaseCalls)
	})

	t.Run("\"true\" enables a real purchase regardless of case", func(t *testing.T) {
		setPurchaseGateEnv(t, "TrUe")
		fake := &fakeServiceClient{purchaseResult: common.PurchaseResult{Success: true}}

		resp, err := ExecutePurchase(context.Background(), realPurchaseRequest(fake, false))

		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.Equal(t, 1, fake.purchaseCalls)
	})

	t.Run("0, false, and garbage all refuse a real purchase", func(t *testing.T) {
		for _, v := range []string{"0", "false", "False", "yes", "enabled", "  "} {
			setPurchaseGateEnv(t, v)
			fake := &fakeServiceClient{purchaseResult: common.PurchaseResult{Success: true}}

			resp, err := ExecutePurchase(context.Background(), realPurchaseRequest(fake, false))

			require.Errorf(t, err, "%q must not enable real purchases", v)
			assert.Nil(t, resp)
			assert.Equalf(t, 0, fake.purchaseCalls, "%q must not enable real purchases", v)
		}
	})

	t.Run("dry run is unaffected by the gate either way", func(t *testing.T) {
		for _, v := range []string{"", "1", "0"} {
			setPurchaseGateEnv(t, v)
			fake := &fakeServiceClient{purchaseResult: common.PurchaseResult{Success: true}}

			resp, err := ExecutePurchase(context.Background(), realPurchaseRequest(fake, true))

			require.NoError(t, err)
			require.NotNil(t, resp)
			assert.True(t, resp.DryRun)
			assert.Equal(t, 0, fake.purchaseCalls, "a preview must never call the provider regardless of the gate")
		}
	})
}
