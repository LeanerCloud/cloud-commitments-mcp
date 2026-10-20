package tools

import (
	"context"
	"errors"
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

	cheapKey := idempotencyKeyFor(region, cheapRec, "", "", "")
	expensiveKey := idempotencyKeyFor(region, expensiveRec, "", "", "")
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

	linuxKey := idempotencyKeyFor(linuxRegion, linuxRec, "", "", "")
	windowsKey := idempotencyKeyFor(windowsRegion, windowsRec, "", "", "")
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

	key1 := idempotencyKeyFor(region, rec, "", "", "")
	key2 := idempotencyKeyFor(region, rec, "", "", "")
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

	noNonceKey := idempotencyKeyFor(region, rec, "", "", "")
	nonceAKey1 := idempotencyKeyFor(region, rec, "", "nonce-a", "")
	nonceAKey2 := idempotencyKeyFor(region, rec, "", "nonce-a", "")
	nonceBKey := idempotencyKeyFor(region, rec, "", "nonce-b", "")

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

// TestExecutePurchaseAmbientScopeCannotDoubleBuy is the regression guard for
// the credential-scope aliasing double-purchase found in the independent
// re-review of #1495.
//
// idempotencyKeyFor folds CredentialScope into the token, and CredentialScope
// returns "" when neither the explicit argument nor the ambient environment
// variable is set. So the SAME target account reached two ways derived two
// DIFFERENT tokens: omitting azure_subscription_id (ambient resolves to
// sub-X) gave "", while passing azure_subscription_id="sub-X" gave "sub-X".
// Every provider's dedupe is token-keyed -- Azure's
// FindReservationOrderByIdempotencyToken, GCP's idempotentCommitmentName, and
// on AWS the EC2/Redshift tag lookups, the RDS/ElastiCache/OpenSearch/MemoryDB
// idempotencyGuard, and Savings Plans' ClientToken -- so the second call's
// lookup missed and bought a SECOND commitment.
//
// The sequence below is the realistic one, not a contrived edge: a model
// previews or purchases without naming the account, then re-calls the same
// purchase with the account explicit (self-correction, or a retry after a
// timeout). Before the fix that bought twice.
//
// The earlier review reasoned only about the opposite direction (one token
// spanning two accounts) and concluded an empty scope was safe because a
// purely-ambient provider is unambiguous for the process lifetime. That is
// true and irrelevant: the hazard is two tokens for ONE account.
func TestExecutePurchaseAmbientScopeCannotDoubleBuy(t *testing.T) {
	t.Parallel()
	rec := testRecommendation()
	realPurchase := func(scope string, fake *fakeServiceClient) (*PurchaseResponse, error) {
		return ExecutePurchase(context.Background(), PurchaseRequest{
			Region: "us-east-1", Recommendation: rec, DryRun: false, Confirm: true,
			CredentialScope: scope,
			ResolveClient:   func(_ context.Context) (provider.ServiceClient, error) { return fake, nil },
		})
	}

	t.Run("explicit account then the same account omitted does not buy twice", func(t *testing.T) {
		t.Parallel()
		// Call 1: the account named explicitly. This is a real purchase.
		first := &fakeServiceClient{purchaseResult: common.PurchaseResult{Success: true, CommitmentID: "ri-1"}}
		resp, err := realPurchase("sub-X", first)
		require.NoError(t, err)
		require.NotNil(t, resp)
		require.Equal(t, 1, first.purchaseCalls)

		// Call 2: identical purchase, but the account left to ambient
		// credentials. It resolves to the SAME sub-X, so this must not
		// become a second commitment.
		second := &fakeServiceClient{purchaseResult: common.PurchaseResult{Success: true, CommitmentID: "ri-2"}}
		_, err = realPurchase("", second)

		require.Error(t, err, "an undeterminable account must not reach the provider on a real purchase")
		assert.Equal(t, 0, second.purchaseCalls,
			"the second call must not purchase: it targets the same account as the first and would double-buy")
		assert.Contains(t, err.Error(), "aws_profile",
			"the refusal must name the argument the caller has to pass")
	})

	t.Run("a real purchase with no determinable account is refused before any provider call", func(t *testing.T) {
		t.Parallel()
		resolveCalled := false
		_, err := ExecutePurchase(context.Background(), PurchaseRequest{
			Region: "us-east-1", Recommendation: rec, DryRun: false, Confirm: true,
			CredentialScope: "",
			ResolveClient: func(_ context.Context) (provider.ServiceClient, error) {
				resolveCalled = true
				return nil, errors.New("ResolveClient must not be called when the scope is undeterminable")
			},
		})
		require.Error(t, err)
		assert.False(t, resolveCalled, "the refusal must not resolve credentials")
	})

	t.Run("whitespace-only scope counts as absent", func(t *testing.T) {
		t.Parallel()
		fake := &fakeServiceClient{purchaseResult: common.PurchaseResult{Success: true}}
		_, err := realPurchase("   ", fake)
		require.Error(t, err)
		assert.Equal(t, 0, fake.purchaseCalls)
	})

	t.Run("a dry run is unaffected and needs no account", func(t *testing.T) {
		t.Parallel()
		resolveCalled := false
		resp, err := ExecutePurchase(context.Background(), PurchaseRequest{
			Region: "us-east-1", Recommendation: rec, DryRun: true, Confirm: true,
			CredentialScope: "",
			ResolveClient: func(_ context.Context) (provider.ServiceClient, error) {
				resolveCalled = true
				return nil, errors.New("dry run must not resolve a client")
			},
		})
		require.NoError(t, err, "previewing must not require naming an account")
		require.NotNil(t, resp)
		assert.True(t, resp.DryRun)
		assert.False(t, resolveCalled)
	})
}

// TestExecutePurchaseExplicitAndAmbientAccountDedupe is the other half of the
// invariant TestExecutePurchaseAmbientScopeCannotDoubleBuy guards. Refusing an
// undeterminable account removes the aliasing only if the two ways of naming a
// KNOWN account still converge: CredentialScope falls back to the same
// environment variable the provider factory itself consults, so passing
// aws_profile="prod" and inheriting AWS_PROFILE=prod must derive ONE token.
// Without this, the fix would merely move the aliasing rather than remove it.
//
// Deliberately not parallel: t.Setenv cannot be used by a test with a parallel
// ancestor.
func TestExecutePurchaseExplicitAndAmbientAccountDedupe(t *testing.T) {
	const envVar = "CUDLY_TEST_ALIAS_PROFILE"
	t.Setenv(envVar, "prod")
	rec := testRecommendation()

	realPurchase := func(scope string, fake *fakeServiceClient) error {
		_, err := ExecutePurchase(context.Background(), PurchaseRequest{
			Region: "us-east-1", Recommendation: rec, DryRun: false, Confirm: true,
			CredentialScope: scope,
			ResolveClient:   func(_ context.Context) (provider.ServiceClient, error) { return fake, nil },
		})
		return err
	}

	explicit := &fakeServiceClient{purchaseResult: common.PurchaseResult{Success: true}}
	require.NoError(t, realPurchase(CredentialScope("prod", envVar), explicit))

	ambient := &fakeServiceClient{purchaseResult: common.PurchaseResult{Success: true}}
	require.NoError(t, realPurchase(CredentialScope("", envVar), ambient))

	assert.Equal(t, explicit.lastOpts.IdempotencyToken, ambient.lastOpts.IdempotencyToken,
		"naming the account explicitly and inheriting it from the environment target the same account and must dedupe")
}
