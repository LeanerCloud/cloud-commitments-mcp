package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsredshift "github.com/aws/aws-sdk-go-v2/service/redshift"
	rstypes "github.com/aws/aws-sdk-go-v2/service/redshift/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
	"github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/redshift"
)

// attemptClock points the budget/attempt clock at a settable time.
func attemptClock(t *testing.T, start time.Time) *time.Time {
	t.Helper()
	cur := start
	prev := budgetNow
	budgetNow = func() time.Time { return cur }
	t.Cleanup(func() { budgetNow = prev })
	return &cur
}

func attemptStatePath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "budget.json")
	t.Setenv(EnvBudgetState, path)
	t.Setenv(EnvBudgetUSD, "1000")
	t.Setenv(EnvMaxCommitmentUSD, "1000000")
	return path
}

func TestFirstAttemptRecordedThenReusedAfterLibraryWindow(t *testing.T) {
	attemptStatePath(t)
	clock := attemptClock(t, time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	first := *clock

	got, err := firstAttemptTime("tok", common.ServiceRedshift)
	require.NoError(t, err)
	assert.Equal(t, first, got)

	*clock = first.Add(time.Hour) // far past the library's 5 minute window
	got, err = firstAttemptTime("tok", common.ServiceRedshift)
	require.NoError(t, err)
	assert.Equal(t, first, got, "a retry must reuse the first attempt time")

	other, err := firstAttemptTime("other", common.ServiceRedshift)
	require.NoError(t, err)
	assert.Equal(t, first.Add(time.Hour), other)
}

func TestFirstAttemptRetentionDropsOldEntries(t *testing.T) {
	attemptStatePath(t)
	clock := attemptClock(t, time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	first := *clock
	_, err := firstAttemptTime("tok", common.ServiceRedshift)
	require.NoError(t, err)

	*clock = first.Add(attemptRetention + time.Hour)
	got, err := firstAttemptTime("tok", common.ServiceRedshift)
	require.NoError(t, err)
	assert.Equal(t, *clock, got)
}

func TestFirstAttemptCorruptStoreRefuses(t *testing.T) {
	path := attemptStatePath(t)
	attemptClock(t, time.Now())
	require.NoError(t, os.WriteFile(path+".attempts", []byte("{not json"), 0o600))

	_, err := firstAttemptTime("tok", common.ServiceRedshift)
	require.ErrorContains(t, err, "corrupt")

	fake := pricedFake(10, common.PurchaseResult{Success: true, CommitmentID: "id"}, nil)
	err = runWithNonce(fake, "n")
	require.ErrorContains(t, err, "first-attempt state unusable")
	assert.Zero(t, fake.purchaseCalls)
	assert.Zero(t, mustBudgetSpent(t, path), "reservation must be released")
}

func TestFirstAttemptWithoutStateFileRefusesRedshiftOnly(t *testing.T) {
	t.Setenv(EnvBudgetState, "")
	attemptClock(t, time.Now())
	_, err := firstAttemptTime("tok-mem", common.ServiceRedshift)
	require.ErrorContains(t, err, EnvBudgetState)
	_, err = firstAttemptTime("tok-mem", common.ServiceEC2)
	require.NoError(t, err)
}

func mustBudgetSpent(t *testing.T, path string) float64 {
	t.Helper()
	st, err := newFileBudget(path).load()
	require.NoError(t, err)
	return st.totalUSD()
}

type recordingProviderOpts struct {
	*fakeServiceClient
	got common.PurchaseOptions
}

func (r *recordingProviderOpts) PurchaseCommitment(ctx context.Context, rec common.Recommendation, opts common.PurchaseOptions) (common.PurchaseResult, error) {
	r.got = opts
	return r.fakeServiceClient.PurchaseCommitment(ctx, rec, opts)
}

func TestPurchasePassesFirstAttemptAsRequestedAt(t *testing.T) {
	attemptStatePath(t)
	clock := attemptClock(t, time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	first := *clock
	fake := &recordingProviderOpts{fakeServiceClient: pricedFake(10, common.PurchaseResult{Success: true, CommitmentID: "id"}, nil)}

	require.NoError(t, runWithNonce(fake, "n"))
	assert.Equal(t, first, fake.got.RequestedAt)
	*clock = first.Add(time.Hour)
	require.NoError(t, runWithNonce(fake, "n"))
	assert.Equal(t, first, fake.got.RequestedAt)
}

// fakeRedshiftAPI is a mocked Redshift API behind the real redshift.Client.
type fakeRedshiftAPI struct {
	nodes     []rstypes.ReservedNode
	purchases int
}

func (f *fakeRedshiftAPI) PurchaseReservedNodeOffering(_ context.Context, _ *awsredshift.PurchaseReservedNodeOfferingInput, _ ...func(*awsredshift.Options)) (*awsredshift.PurchaseReservedNodeOfferingOutput, error) {
	f.purchases++
	return &awsredshift.PurchaseReservedNodeOfferingOutput{ReservedNode: &rstypes.ReservedNode{ReservedNodeId: aws.String("rn-1")}}, nil
}

func (f *fakeRedshiftAPI) DescribeReservedNodeOfferings(_ context.Context, _ *awsredshift.DescribeReservedNodeOfferingsInput, _ ...func(*awsredshift.Options)) (*awsredshift.DescribeReservedNodeOfferingsOutput, error) {
	return &awsredshift.DescribeReservedNodeOfferingsOutput{ReservedNodeOfferings: []rstypes.ReservedNodeOffering{{
		ReservedNodeOfferingId: aws.String("off-1"), NodeType: aws.String("dc2.large"),
		Duration: aws.Int32(3 * 12 * 2592000), ReservedNodeOfferingType: rstypes.ReservedNodeOfferingTypeRegular,
		UsagePrice: aws.Float64(0.1),
	}}}, nil
}

func (f *fakeRedshiftAPI) DescribeReservedNodes(_ context.Context, _ *awsredshift.DescribeReservedNodesInput, _ ...func(*awsredshift.Options)) (*awsredshift.DescribeReservedNodesOutput, error) {
	return &awsredshift.DescribeReservedNodesOutput{ReservedNodes: f.nodes}, nil
}

// realRedshiftClient prices through the shared fake and purchases through the
// real redshift.Client.
type realRedshiftClient struct {
	*redshift.Client
	pricing *fakeServiceClient
}

func (c realRedshiftClient) GetOfferingDetails(ctx context.Context, rec common.Recommendation) (*common.OfferingDetails, error) {
	return c.pricing.GetOfferingDetails(ctx, rec)
}

func TestRedshiftPurchaseProceedsAndRetryAdoptsAfterWindow(t *testing.T) {
	attemptStatePath(t)
	clock := attemptClock(t, time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	first := *clock
	api := &fakeRedshiftAPI{}
	rc := redshift.NewClient(aws.Config{Region: "us-east-1"})
	rc.SetRedshiftAPI(api)
	var client provider.ServiceClient = realRedshiftClient{rc, pricedFake(10, common.PurchaseResult{}, nil)}

	run := func() (*PurchaseResponse, error) {
		rec := testRecommendation()
		rec.Service, rec.ResourceType, rec.Count, rec.PaymentOption = common.ServiceRedshift, "dc2.large", 1, "no-upfront"
		return ExecutePurchase(context.Background(), PurchaseRequest{
			Region: rec.Region, Recommendation: rec, Confirm: true, CredentialScope: "scope", Nonce: "n",
			ResolveClient: func(context.Context) (provider.ServiceClient, error) { return client, nil },
		})
	}
	resp, err := run()
	require.NoError(t, err)
	assert.True(t, resp.Success)
	assert.Equal(t, 1, api.purchases)

	// The node now exists, started right after the first attempt. A retry an
	// hour later must still find it: RequestedAt stays at the first attempt.
	api.nodes = []rstypes.ReservedNode{{
		ReservedNodeId: aws.String("rn-1"), ReservedNodeOfferingId: aws.String("off-1"),
		NodeCount: aws.Int32(1), State: aws.String("active"), StartTime: aws.Time(first.Add(time.Second)),
	}}
	*clock = first.Add(time.Hour)
	resp, err = run()
	require.NoError(t, err)
	assert.Equal(t, 1, api.purchases, "the retry must adopt the first purchase, not buy again")
	assert.Equal(t, "rn-1", resp.CommitmentID)
}
