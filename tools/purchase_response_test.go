package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestArcheraOfferOnlyAfterSuccessfulRealPurchase pins when the Archera
// underutilization-insurance offer is attached, and that it always carries
// both partnership disclosures.
//
// The offer must appear ONLY after a real purchase actually succeeds: a dry
// run bought nothing and a failed purchase bought nothing, so in both cases
// there is no commitment to insure and no enrollment window running. This
// mirrors the CLI, which calls printArcheraPitch only under `riSuccess > 0`.
//
// Both disclosures are asserted because CUDly commits to surfacing the
// sponsorship AND the works-fine-without-it fact everywhere the signup link
// appears. An MCP client renders this payload through a model, so a link
// without its disclosures would let a sponsored recommendation be presented
// as a neutral one.
func TestArcheraOfferOnlyAfterSuccessfulRealPurchase(t *testing.T) {
	t.Parallel()
	rec := testRecommendation()

	t.Run("a dry run carries no offer", func(t *testing.T) {
		t.Parallel()
		resp, err := ExecutePurchase(context.Background(), PurchaseRequest{
			Region: "us-east-1", Recommendation: rec, DryRun: true,
		})
		require.NoError(t, err)
		assert.Nil(t, resp.Archera, "nothing was bought, so there is nothing to insure")

		raw, err := json.Marshal(resp)
		require.NoError(t, err)
		assert.NotContains(t, string(raw), "archera",
			"the offer must be omitted from the payload entirely, not sent empty")
	})

	t.Run("a failed purchase carries no offer", func(t *testing.T) {
		t.Parallel()
		fake := &fakeServiceClient{purchaseResult: common.PurchaseResult{
			Success: false,
			Error:   errors.New("insufficient capacity"),
		}}
		resp, err := ExecutePurchase(context.Background(), PurchaseRequest{
			Region: "us-east-1", Recommendation: rec, DryRun: false, Confirm: true, CredentialScope: "test-scope",
			ResolveClient: func(_ context.Context) (provider.ServiceClient, error) { return fake, nil },
		})
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.Nil(t, resp.Archera, "a purchase that did not happen has no enrollment window")
	})

	t.Run("a successful real purchase carries the offer and both disclosures", func(t *testing.T) {
		t.Parallel()
		fake := &fakeServiceClient{purchaseResult: common.PurchaseResult{
			Success:      true,
			CommitmentID: "ri-abc123",
		}}
		resp, err := ExecutePurchase(context.Background(), PurchaseRequest{
			Region: "us-east-1", Recommendation: rec, DryRun: false, Confirm: true, CredentialScope: "test-scope",
			ResolveClient: func(_ context.Context) (provider.ServiceClient, error) { return fake, nil },
		})
		require.NoError(t, err)
		require.NotNil(t, resp.Archera, "a completed purchase must surface the insurance option")

		assert.Equal(t, common.ArcheraSignupURL, resp.Archera.SignupURL)
		assert.Equal(t, common.ArcheraEnrollmentWindowDays, resp.Archera.EnrollmentWindowDays)
		assert.NotEmpty(t, resp.Archera.Pitch)

		// The two disclosures CUDly commits to keeping visible wherever the
		// signup link is shown.
		assert.Equal(t, common.ArcheraNonGatingDisclosure, resp.Archera.NonGatingDisclosure)
		assert.Equal(t, common.ArcheraSponsorshipDisclosure, resp.Archera.SponsorshipDisclosure)
		assert.Contains(t, resp.Archera.NonGatingDisclosure, "work fully without Archera")
		assert.Contains(t, resp.Archera.SponsorshipDisclosure, "sponsors")

		// Whatever a client renders, the link never travels without them.
		raw, err := json.Marshal(resp)
		require.NoError(t, err)
		body := string(raw)
		require.Contains(t, body, common.ArcheraSignupURL)
		assert.Contains(t, body, "non_gating_disclosure")
		assert.Contains(t, body, "sponsorship_disclosure")
	})
}

// TestExecutePurchasePreviewOmitsUnknownCostFields proves finding 2 of the
// adversarial review: a dry-run preview built from a Recommendation that
// mirrors what real purchase tools actually construct (no cost fields set,
// since no *FromArgs constructor in this package populates them) must not
// report cost/on_demand_cost/estimated_savings/savings_percentage as a real
// 0 -- that would be indistinguishable from a confirmed $0 purchase. The
// pointer fields must be nil, and therefore omitted from the JSON payload
// entirely rather than serialized as 0.
func TestExecutePurchasePreviewOmitsUnknownCostFields(t *testing.T) {
	t.Parallel()
	resp, err := ExecutePurchase(context.Background(), PurchaseRequest{
		Region:         "us-east-1",
		Recommendation: testRecommendation(),
		DryRun:         true,
		Confirm:        false,
	})
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.Nil(t, resp.Cost)
	assert.Nil(t, resp.OnDemandCost)
	assert.Nil(t, resp.EstimatedSavings)
	assert.Nil(t, resp.SavingsPercentage)

	raw, err := json.Marshal(resp)
	require.NoError(t, err)
	body := string(raw)
	assert.NotContains(t, body, `"cost"`, "unknown cost must be omitted from the JSON payload, not reported as 0")
	assert.NotContains(t, body, `"on_demand_cost"`)
	assert.NotContains(t, body, `"estimated_savings"`)
	assert.NotContains(t, body, `"savings_percentage"`)
}
