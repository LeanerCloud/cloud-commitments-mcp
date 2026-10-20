package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
)

const (
	convertibleUnitUSD = 900.0
	standardUnitUSD    = 400.0
)

// classPricedClient mirrors (*ec2.Client): GetOfferingDetails prices the
// convertible offering, GetOfferingDetailsForClass the requested class.
type classPricedClient struct {
	*fakeServiceClient
	classCalls []string
}

func newClassPricedClient() *classPricedClient {
	return &classPricedClient{fakeServiceClient: &fakeServiceClient{
		purchaseResult: common.PurchaseResult{Success: true, CommitmentID: "ri-abc"},
		offering:       &common.OfferingDetails{TotalCost: convertibleUnitUSD, Currency: "USD"},
	}}
}

func (c *classPricedClient) GetOfferingDetailsForClass(_ context.Context, _ common.Recommendation, class string) (*common.OfferingDetails, error) {
	c.classCalls = append(c.classCalls, class)
	if class == string(OfferingClassStandard) {
		return &common.OfferingDetails{TotalCost: standardUnitUSD, Currency: "USD"}, nil
	}
	return &common.OfferingDetails{TotalCost: convertibleUnitUSD, Currency: "USD"}, nil
}

func purchaseWithClass(client provider.ServiceClient, class string) error {
	rec := testRecommendation()
	rec.Count = 1
	_, err := ExecutePurchase(context.Background(), PurchaseRequest{
		Region: rec.Region, Recommendation: rec, Confirm: true, CredentialScope: "scope",
		OfferingClass: class,
		ResolveClient: func(context.Context) (provider.ServiceClient, error) { return client, nil },
	})
	return err
}

func TestStandardClassIsCappedAndBudgetedAtStandardPrice(t *testing.T) {
	resetBudget(t, "1000")
	t.Setenv(EnvMaxCommitmentUSD, "500") // between standard (400) and convertible (900)
	c := newClassPricedClient()

	require.NoError(t, purchaseWithClass(c, "standard"))
	assert.Equal(t, 1, c.purchaseCalls)
	assert.Equal(t, []string{"standard"}, c.classCalls)
	assert.Zero(t, c.offeringCalls, "convertible pricing must not be consulted")
	assert.InDelta(t, standardUnitUSD, processBudget.committedUSD(), 1e-9)
}

func TestStandardClassCapRefusalReportsStandardPrice(t *testing.T) {
	resetBudget(t, "1000")
	t.Setenv(EnvMaxCommitmentUSD, "399")
	c := newClassPricedClient()

	err := purchaseWithClass(c, "standard")
	require.ErrorContains(t, err, "400.00")
	assert.Zero(t, c.purchaseCalls)
}

func TestStandardClassWithoutClassPricingIsRefused(t *testing.T) {
	resetBudget(t, "1000")
	c := &fakeServiceClient{purchaseResult: common.PurchaseResult{Success: true, CommitmentID: "ri-abc"}}

	err := purchaseWithClass(c, "standard")
	require.ErrorContains(t, err, "GetOfferingDetailsForClass")
	assert.Zero(t, c.purchaseCalls, "must be refused before any purchase")
	assert.Zero(t, c.offeringCalls, "must not fall back to convertible pricing")
	assert.Zero(t, processBudget.committedUSD())
}

func TestConvertibleAndDefaultClassKeepConvertiblePricing(t *testing.T) {
	for _, class := range []string{"", "convertible"} {
		t.Run("class="+class, func(t *testing.T) {
			resetBudget(t, "1000")
			c := newClassPricedClient()

			require.NoError(t, purchaseWithClass(c, class))
			assert.Empty(t, c.classCalls)
			assert.Equal(t, 1, c.offeringCalls)
			assert.InDelta(t, convertibleUnitUSD, processBudget.committedUSD(), 1e-9)
		})
	}
}

func TestStandardClassOnNonEC2StaysOnGenericPricing(t *testing.T) {
	t.Parallel()
	c := newClassPricedClient()
	rec := testRecommendation()
	rec.Service = common.ServiceRDS
	rec.Count = 1

	got, err := committedUSD(context.Background(), c, rec, "standard")
	require.NoError(t, err)
	assert.InDelta(t, convertibleUnitUSD, got, 1e-9)
	assert.Empty(t, c.classCalls)
}
