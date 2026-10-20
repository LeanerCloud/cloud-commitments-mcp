package tools

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExecutePurchaseSameRequestDerivesSameToken proves idempotencyKeyFor
// (and therefore the derived token) is deterministic for the same
// identifying fields, so a retried call with identical arguments dedupes at
// the provider rather than double-purchasing.
func TestExecutePurchaseSameRequestDerivesSameToken(t *testing.T) {
	t.Parallel()
	rec := testRecommendation()
	fake1 := &fakeServiceClient{purchaseResult: common.PurchaseResult{Success: true}}
	fake2 := &fakeServiceClient{purchaseResult: common.PurchaseResult{Success: true}}

	_, err := ExecutePurchase(context.Background(), PurchaseRequest{
		Region: "us-east-1", Recommendation: rec, DryRun: false, Confirm: true, CredentialScope: "test-scope",
		ResolveClient: func(_ context.Context) (provider.ServiceClient, error) { return fake1, nil },
	})
	require.NoError(t, err)

	_, err = ExecutePurchase(context.Background(), PurchaseRequest{
		Region: "us-east-1", Recommendation: rec, DryRun: false, Confirm: true, CredentialScope: "test-scope",
		ResolveClient: func(_ context.Context) (provider.ServiceClient, error) { return fake2, nil },
	})
	require.NoError(t, err)

	assert.Equal(t, fake1.lastOpts.IdempotencyToken, fake2.lastOpts.IdempotencyToken)

	// A materially different request (different count) must derive a
	// different token.
	rec2 := rec
	rec2.Count = 4
	fake3 := &fakeServiceClient{purchaseResult: common.PurchaseResult{Success: true}}
	_, err = ExecutePurchase(context.Background(), PurchaseRequest{
		Region: "us-east-1", Recommendation: rec2, DryRun: false, Confirm: true, CredentialScope: "test-scope",
		ResolveClient: func(_ context.Context) (provider.ServiceClient, error) { return fake3, nil },
	})
	require.NoError(t, err)
	assert.NotEqual(t, fake1.lastOpts.IdempotencyToken, fake3.lastOpts.IdempotencyToken)
}

// TestIdempotencyKeyDistinguishesSavingsPlanHourlyCommitment proves finding
// 1 of the adversarial review of the purchase feature: two Savings Plans
// requests that differ only in hourly_commitment (a $5/hr vs a $50/hr
// Compute Savings Plan) must derive different idempotency tokens. Before the
// fix, idempotencyKeyFor never consulted rec.Details at all, so these two
// materially different purchases collided on the same token and AWS would
// have silently deduped the second call as a "retry" of the first instead
// of buying a second, larger plan.
func TestIdempotencyKeyDistinguishesSavingsPlanHourlyCommitment(t *testing.T) {
	t.Parallel()

	cheapArgs := validSavingsPlansArgs()
	cheapArgs.HourlyCommitment = 5
	expensiveArgs := validSavingsPlansArgs()
	expensiveArgs.HourlyCommitment = 50

	cheapRec, region, _, _, err := savingsPlanRecommendationFromArgs(cheapArgs)
	require.NoError(t, err)
	expensiveRec, _, _, _, err := savingsPlanRecommendationFromArgs(expensiveArgs)
	require.NoError(t, err)

	cheapKey := idempotencyKeyFor(region, cheapRec, "", "")
	expensiveKey := idempotencyKeyFor(region, expensiveRec, "", "")
	assert.NotEqual(t, cheapKey, expensiveKey,
		"a $5/hr and a $50/hr Compute Savings Plan must not derive the same idempotency key")
}

// TestIdempotencyKeyDistinguishesEC2Platform proves the second half of
// finding 1: an EC2 RI purchase for Linux vs Windows, with every other field
// (region/instance_type/count/term/payment_option) identical, must not
// collide on the same idempotency key -- Platform is a price- and
// product-affecting dimension carried in rec.Details, and the pre-fix key
// derivation ignored Details entirely.
func TestIdempotencyKeyDistinguishesEC2Platform(t *testing.T) {
	t.Parallel()

	linuxArgs := validEC2Args()
	linuxArgs.Platform = "Linux/UNIX"
	windowsArgs := validEC2Args()
	windowsArgs.Platform = "Windows"

	linuxRec, linuxRegion, _, _, err := ec2RecommendationFromArgs(linuxArgs)
	require.NoError(t, err)
	windowsRec, windowsRegion, _, _, err := ec2RecommendationFromArgs(windowsArgs)
	require.NoError(t, err)

	linuxKey := idempotencyKeyFor(linuxRegion, linuxRec, "", "")
	windowsKey := idempotencyKeyFor(windowsRegion, windowsRec, "", "")
	assert.NotEqual(t, linuxKey, windowsKey,
		"a Linux and a Windows EC2 RI purchase must not derive the same idempotency key")
}

// TestIdempotencyKeySameDimensionsNoNonceAlwaysMatch is the regression guard
// for the fail-safe design: identical purchase dimensions with no nonce must
// ALWAYS derive the same key, with no dependence on time at all. This is the
// inverse of, and replaces, a prior design that folded an automatic hourly
// time bucket into the key when no nonce was supplied -- under that design a
// retry that happened to straddle an hour boundary (e.g. issued at
// 12:59:58, retried four seconds later at 13:00:02) derived a DIFFERENT key,
// so the provider could treat the retry as a brand new purchase instead of
// deduping it, resulting in a double purchase. idempotencyKeyFor no longer
// reads a clock at all when nonce is empty, so this is not merely "same
// bucket" but unconditionally the same key for the life of the process.
func TestIdempotencyKeySameDimensionsNoNonceAlwaysMatch(t *testing.T) {
	t.Parallel()
	rec := testRecommendation()
	region := "us-east-1"

	key1 := idempotencyKeyFor(region, rec, "", "")
	key2 := idempotencyKeyFor(region, rec, "", "")
	assert.Equal(t, key1, key2,
		"identical dimensions with no nonce must always derive the same key, so a retry never double-buys")
}

// TestIdempotencyKeyNonceAuthorizesDistinctRepeat proves the nonce is the
// caller's explicit opt-in to a deliberate repeat purchase: a non-empty
// nonce derives a key different from the no-nonce key and from a different
// nonce, but the SAME nonce with the SAME dimensions still dedupes (a
// nonce'd retry is still safe against double-buying).
func TestIdempotencyKeyNonceAuthorizesDistinctRepeat(t *testing.T) {
	t.Parallel()
	rec := testRecommendation()
	region := "us-east-1"

	noNonceKey := idempotencyKeyFor(region, rec, "", "")
	nonceAKey1 := idempotencyKeyFor(region, rec, "", "nonce-a")
	nonceAKey2 := idempotencyKeyFor(region, rec, "", "nonce-a")
	nonceBKey := idempotencyKeyFor(region, rec, "", "nonce-b")

	assert.NotEqual(t, noNonceKey, nonceAKey1,
		"supplying a nonce must authorize a purchase distinct from the no-nonce default")
	assert.NotEqual(t, nonceAKey1, nonceBKey,
		"two different nonces must derive two different keys")
	assert.Equal(t, nonceAKey1, nonceAKey2,
		"the same nonce with the same dimensions must still dedupe a nonce'd retry")
}

// TestExecutePurchaseNonceThreadedThroughToToken proves PurchaseRequest.Nonce
// is actually wired end to end into ExecutePurchase's derived token, not
// just exercised at the idempotencyKeyFor level in isolation.
func TestExecutePurchaseNonceThreadedThroughToToken(t *testing.T) {
	t.Parallel()
	rec := testRecommendation()

	fake1 := &fakeServiceClient{purchaseResult: common.PurchaseResult{Success: true}}
	_, err := ExecutePurchase(context.Background(), PurchaseRequest{
		Region: "us-east-1", Recommendation: rec, DryRun: false, Confirm: true, Nonce: "call-1", CredentialScope: "test-scope",
		ResolveClient: func(_ context.Context) (provider.ServiceClient, error) { return fake1, nil },
	})
	require.NoError(t, err)

	fake2 := &fakeServiceClient{purchaseResult: common.PurchaseResult{Success: true}}
	_, err = ExecutePurchase(context.Background(), PurchaseRequest{
		Region: "us-east-1", Recommendation: rec, DryRun: false, Confirm: true, Nonce: "call-2", CredentialScope: "test-scope",
		ResolveClient: func(_ context.Context) (provider.ServiceClient, error) { return fake2, nil },
	})
	require.NoError(t, err)

	assert.NotEqual(t, fake1.lastOpts.IdempotencyToken, fake2.lastOpts.IdempotencyToken,
		"different nonces must derive different idempotency tokens")

	fake3 := &fakeServiceClient{purchaseResult: common.PurchaseResult{Success: true}}
	_, err = ExecutePurchase(context.Background(), PurchaseRequest{
		Region: "us-east-1", Recommendation: rec, DryRun: false, Confirm: true, Nonce: "call-1", CredentialScope: "test-scope",
		ResolveClient: func(_ context.Context) (provider.ServiceClient, error) { return fake3, nil },
	})
	require.NoError(t, err)

	assert.Equal(t, fake1.lastOpts.IdempotencyToken, fake3.lastOpts.IdempotencyToken,
		"the same nonce must derive the same idempotency token")
}
