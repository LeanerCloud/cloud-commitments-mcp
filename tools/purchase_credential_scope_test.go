package tools

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIdempotencyKeyDistinguishesCredentialScope is the regression guard for
// the cross-account false-dedupe found in review. The product dimensions
// folded into the key describe WHAT is bought, never WHERE it lands, so two
// identical purchases aimed at different accounts derived the SAME token.
//
// On Azure that silently skips a real purchase rather than merely being
// imprecise: reservations.FindReservationOrderByIdempotencyToken lists
// reservation orders from the TENANT-wide endpoint (ReservationOrdersListURL
// has no subscription prefix), so buying the same VM reservation for a second
// subscription in the same tenant matched the FIRST subscription's order by
// token, short-circuited, and reported success without buying anything for
// the second subscription. This test fails on the pre-fix key, which took no
// scope argument at all.
func TestIdempotencyKeyDistinguishesCredentialScope(t *testing.T) {
	t.Parallel()
	rec := testRecommendation()
	region := "us-east-1"

	subAKey := idempotencyKeyFor(region, rec, "subscription-a", "")
	subBKey := idempotencyKeyFor(region, rec, "subscription-b", "")
	subAKeyAgain := idempotencyKeyFor(region, rec, "subscription-a", "")

	assert.NotEqual(t, subAKey, subBKey,
		"identical purchases billed to different accounts must derive different tokens")
	assert.Equal(t, subAKey, subAKeyAgain,
		"a retry against the same account must still dedupe")
}

// TestExecutePurchaseCredentialScopeThreadedThroughToToken proves
// PurchaseRequest.CredentialScope reaches the token the provider actually
// dedupes on, not just idempotencyKeyFor in isolation.
func TestExecutePurchaseCredentialScopeThreadedThroughToToken(t *testing.T) {
	t.Parallel()
	rec := testRecommendation()

	purchaseInScope := func(scope string) *fakeServiceClient {
		fake := &fakeServiceClient{purchaseResult: common.PurchaseResult{Success: true}}
		_, err := ExecutePurchase(context.Background(), PurchaseRequest{
			Region: "us-east-1", Recommendation: rec, DryRun: false, Confirm: true,
			CredentialScope: scope,
			ResolveClient:   func(_ context.Context) (provider.ServiceClient, error) { return fake, nil },
		})
		require.NoError(t, err)
		return fake
	}

	subA := purchaseInScope("subscription-a")
	subB := purchaseInScope("subscription-b")
	subARetry := purchaseInScope("subscription-a")

	assert.NotEqual(t, subA.lastOpts.IdempotencyToken, subB.lastOpts.IdempotencyToken,
		"different credential scopes must derive different idempotency tokens")
	assert.Equal(t, subA.lastOpts.IdempotencyToken, subARetry.lastOpts.IdempotencyToken,
		"the same credential scope must derive the same idempotency token")
}

// TestCredentialScopeResolution pins CredentialScope's precedence: an
// explicit caller-supplied override always wins, an ambient environment
// variable is the fallback (matching what the provider factory itself
// consults), whitespace-only input counts as absent, and "" is a legitimate
// result rather than an error (see the CredentialScope doc comment).
func TestCredentialScopeResolution(t *testing.T) {
	const envVar = "CUDLY_TEST_SUBSCRIPTION_ID"

	t.Run("explicit override wins over the environment", func(t *testing.T) {
		t.Setenv(envVar, "from-env")
		assert.Equal(t, "explicit", CredentialScope("explicit", envVar))
	})

	t.Run("falls back to the environment when no override is given", func(t *testing.T) {
		t.Setenv(envVar, "from-env")
		assert.Equal(t, "from-env", CredentialScope("", envVar))
	})

	t.Run("whitespace-only values count as absent", func(t *testing.T) {
		t.Setenv(envVar, "  ")
		assert.Empty(t, CredentialScope("   ", envVar))
	})

	t.Run("surrounding whitespace is trimmed so it cannot fork the key", func(t *testing.T) {
		assert.Equal(t, "sub-a", CredentialScope(" sub-a "))
	})

	t.Run("empty when neither override nor environment is set", func(t *testing.T) {
		assert.Empty(t, CredentialScope("", envVar))
	})
}

// TestCredentialScopeArgNamesEveryProvider pins that every provider a purchase
// tool can build a Recommendation for has a scope argument to name in the
// refusal, and that an unrecognized provider is an explicit internal error
// rather than a refusal the caller cannot act on.
func TestCredentialScopeArgNamesEveryProvider(t *testing.T) {
	t.Parallel()
	for prov, want := range map[common.ProviderType]string{
		common.ProviderAWS:   "aws_profile",
		common.ProviderAzure: "azure_subscription_id",
		common.ProviderGCP:   "gcp_project_id",
	} {
		got, err := credentialScopeSourceFor(prov)
		require.NoErrorf(t, err, "provider %q must have a scope source", prov)
		assert.Equal(t, want, got.arg)
	}

	_, err := credentialScopeSourceFor(common.ProviderType("nimbus"))
	require.Error(t, err, "an unknown provider must fail loud rather than emit an unactionable refusal")
}

// TestRequireCredentialScopeGCPMessageIsActionable pins the GCP contract found
// by review after the fail-closed gate landed.
//
// gcp_computeengine_cud.go calls CredentialScope(args.GCPProjectID) with no
// environment fallback, so omitting gcp_project_id ALWAYS yields "" and the
// gate refuses every real GCP purchase. That is the intended contract, not a
// bug -- providers/gcp reads no project environment variable, and with none
// configured it falls back to getDefaultProject, i.e. the first ACTIVE project
// in the caller's ListProjects response. Spending money in "whichever project
// happened to be listed first" is not a defensible default, and an
// env-supplied scope would be worse still: it could name project A while the
// purchase landed in project B, making the token assert an account it never
// touched.
//
// Since the refusal is the contract, the message has to be actionable. Telling
// a GCP caller their account "could not be determined" would send them looking
// for an environment variable that does not exist, so GCP gets a plain
// "required" instead.
func TestRequireCredentialScopeGCPMessageIsActionable(t *testing.T) {
	t.Parallel()

	gcpErr := requireCredentialScope(common.ProviderGCP, "")
	require.Error(t, gcpErr)
	assert.Contains(t, gcpErr.Error(), "gcp_project_id", "the refusal must name the argument to pass")
	assert.Contains(t, gcpErr.Error(), "is required",
		"GCP has no environment fallback, so the message must say the argument is required")
	assert.NotContains(t, gcpErr.Error(), "could not be determined",
		"that phrasing implies an ambient source exists, sending a GCP caller after a variable nothing reads")

	// Providers that DO have a fallback must still name it, so a caller who
	// set the environment variable is not told to pass an argument they do
	// not need.
	for _, tc := range []struct {
		provider common.ProviderType
		arg      string
		envVar   string
	}{
		{common.ProviderAWS, "aws_profile", "AWS_PROFILE"},
		{common.ProviderAzure, "azure_subscription_id", "AZURE_SUBSCRIPTION_ID"},
	} {
		err := requireCredentialScope(tc.provider, "")
		require.Errorf(t, err, "provider %q", tc.provider)
		assert.Containsf(t, err.Error(), tc.arg, "provider %q must name its argument", tc.provider)
		assert.Containsf(t, err.Error(), tc.envVar,
			"provider %q has an ambient fallback and must name it", tc.provider)
	}
}
