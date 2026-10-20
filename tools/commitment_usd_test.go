package tools

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
)

// offeringClient embeds the shared fake and only overrides GetOfferingDetails.
type offeringClient struct {
	err error
	od  *common.OfferingDetails
	fakeServiceClient
	calls int
}

func (o *offeringClient) GetOfferingDetails(_ context.Context, _ common.Recommendation) (*common.OfferingDetails, error) {
	o.calls++
	return o.od, o.err
}

func usdOffering(total float64) *common.OfferingDetails {
	return &common.OfferingDetails{TotalCost: total, Currency: "USD"}
}

func usdSPRec(term string, details common.ServiceDetails) common.Recommendation {
	return common.Recommendation{
		Provider: common.ProviderAWS, Service: common.ServiceSavingsPlansCompute,
		CommitmentType: common.CommitmentSavingsPlan, Term: term, Count: 1, Details: details,
	}
}

func TestCommittedUSDPerProvider(t *testing.T) {
	t.Parallel()
	for _, svc := range []common.ServiceType{
		common.ServiceEC2, common.ServiceRDS, common.ServiceElastiCache,
		common.ServiceOpenSearch, common.ServiceRedshift, common.ServiceMemoryDB,
	} {
		t.Run("aws-"+string(svc), func(t *testing.T) {
			c := &offeringClient{od: usdOffering(1234.5)}
			rec := common.Recommendation{Provider: common.ProviderAWS, Service: svc, CommitmentType: common.CommitmentReservedInstance, Count: 3}
			got, err := committedUSD(context.Background(), c, rec, "")
			require.NoError(t, err)
			assert.InDelta(t, 3703.5, got, 1e-9)
			assert.Equal(t, 1, c.calls)
		})
	}
	t.Run("azure-compute", func(t *testing.T) {
		c := &offeringClient{od: usdOffering(500)}
		rec := common.Recommendation{Provider: common.ProviderAzure, Service: common.ServiceCompute, CommitmentType: common.CommitmentReservedInstance, Count: 4}
		got, err := committedUSD(context.Background(), c, rec, "")
		require.NoError(t, err)
		assert.InDelta(t, 2000, got, 1e-9)
	})
	for _, tc := range []struct {
		term  string
		years float64
	}{{"1yr", 1}, {"3yr", 3}} {
		t.Run("savings-plan-"+tc.term, func(t *testing.T) {
			c := &offeringClient{}
			got, err := committedUSD(context.Background(), c, usdSPRec(tc.term, &common.SavingsPlanDetails{HourlyCommitment: 2.5}), "")
			require.NoError(t, err)
			assert.InDelta(t, 2.5*8760*tc.years, got, 1e-9)
			assert.Zero(t, c.calls)
		})
	}
}

func TestCommittedUSDFailsClosed(t *testing.T) {
	t.Parallel()
	aws := common.Recommendation{Provider: common.ProviderAWS, Service: common.ServiceEC2, Count: 2}
	withCount := func(n int) common.Recommendation { r := aws; r.Count = n; return r }
	tests := []struct {
		err       error
		od        *common.OfferingDetails
		name      string
		rec       common.Recommendation
		wantCalls int
	}{
		{err: nil, od: nil, name: "nil details", rec: aws, wantCalls: 1},
		{err: errors.New("boom"), od: nil, name: "offering error", rec: aws, wantCalls: 1},
		{err: nil, od: usdOffering(0), name: "zero total", rec: aws, wantCalls: 1},
		{err: nil, od: usdOffering(-1), name: "negative total", rec: aws, wantCalls: 1},
		{err: nil, od: usdOffering(math.NaN()), name: "NaN total", rec: aws, wantCalls: 1},
		{err: nil, od: usdOffering(math.Inf(1)), name: "Inf total", rec: aws, wantCalls: 1},
		{err: nil, od: &common.OfferingDetails{TotalCost: 1, Currency: "EUR"}, name: "EUR", rec: aws, wantCalls: 1},
		{err: nil, od: &common.OfferingDetails{TotalCost: 1}, name: "empty currency", rec: aws, wantCalls: 1},
		{err: nil, od: usdOffering(1), name: "count 0", rec: withCount(0), wantCalls: 1},
		{err: nil, od: usdOffering(math.MaxFloat64), name: "overflow", rec: aws, wantCalls: 1},
		{err: nil, od: nil, name: "sp nil details", rec: usdSPRec("1yr", nil), wantCalls: 0},
		{err: nil, od: nil, name: "sp typed nil details", rec: usdSPRec("1yr", (*common.SavingsPlanDetails)(nil)), wantCalls: 0},
		{err: nil, od: nil, name: "sp zero hourly", rec: usdSPRec("1yr", &common.SavingsPlanDetails{}), wantCalls: 0},
		{err: nil, od: nil, name: "sp NaN hourly", rec: usdSPRec("1yr", &common.SavingsPlanDetails{HourlyCommitment: math.NaN()}), wantCalls: 0},
		{err: nil, od: nil, name: "sp 2yr term", rec: usdSPRec("2yr", &common.SavingsPlanDetails{HourlyCommitment: 1}), wantCalls: 0},
		{err: nil, od: nil, name: "sp empty term", rec: usdSPRec("", &common.SavingsPlanDetails{HourlyCommitment: 1}), wantCalls: 0},
		{err: nil, od: usdOffering(1), name: "gcp provider with RI type", rec: common.Recommendation{Provider: common.ProviderGCP, Service: common.ServiceCompute, CommitmentType: common.CommitmentReservedInstance, Count: 1}, wantCalls: 0},
		{err: nil, od: usdOffering(1), name: "gcp provider with empty type", rec: common.Recommendation{Provider: common.ProviderGCP, Service: common.ServiceCompute, Count: 1}, wantCalls: 0},
		{err: nil, od: usdOffering(1), name: "gcp cud", rec: common.Recommendation{Provider: common.ProviderGCP, Service: common.ServiceCompute, CommitmentType: common.CommitmentCUD, Count: 1}, wantCalls: 0},
		{err: nil, od: usdOffering(1), name: "cud on non-gcp provider", rec: common.Recommendation{Provider: common.ProviderAWS, CommitmentType: common.CommitmentCUD, Count: 1}, wantCalls: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &offeringClient{od: tc.od, err: tc.err}
			got, err := committedUSD(context.Background(), c, tc.rec, "")
			require.Error(t, err)
			assert.Zero(t, got)
			assert.Equal(t, tc.wantCalls, c.calls)
		})
	}

	t.Run("padded lowercase usd accepted", func(t *testing.T) {
		c := &offeringClient{od: &common.OfferingDetails{TotalCost: 10, Currency: " usd "}}
		got, err := committedUSD(context.Background(), c, aws, "")
		require.NoError(t, err)
		assert.InDelta(t, 20, got, 1e-9)
	})
	t.Run("lowercase usd accepted", func(t *testing.T) {
		c := &offeringClient{od: &common.OfferingDetails{TotalCost: 10, Currency: "usd"}}
		got, err := committedUSD(context.Background(), c, aws, "")
		require.NoError(t, err)
		assert.InDelta(t, 20, got, 1e-9)
	})
	t.Run("offering error is wrapped", func(t *testing.T) {
		cause := errors.New("boom")
		_, err := committedUSD(context.Background(), &offeringClient{err: cause}, aws, "")
		require.ErrorIs(t, err, cause)
		assert.Contains(t, err.Error(), "cannot determine USD total")
	})
}

func TestUSDCapBoundary(t *testing.T) {
	t.Parallel()
	rec := common.Recommendation{Provider: common.ProviderAWS, Service: common.ServiceEC2, Count: 2}
	c := &offeringClient{od: usdOffering(500)}

	got, err := enforceUSDCap(context.Background(), c, rec, "", 1000)
	require.NoError(t, err)
	assert.InDelta(t, 1000, got, 1e-9)

	_, err = enforceUSDCap(context.Background(), c, rec, "", 999.99)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CUDLY_MCP_MAX_COMMITMENT_USD")
	assert.Contains(t, err.Error(), "1000.00")
	assert.Contains(t, err.Error(), "999.99")

	_, err = enforceUSDCap(context.Background(), &offeringClient{err: errors.New("x")}, rec, "", 1e12)
	require.Error(t, err)
}

func TestUSDCapInvalidLimit(t *testing.T) {
	t.Parallel()
	rec := common.Recommendation{Provider: common.ProviderAWS, Service: common.ServiceEC2, Count: 1}
	for _, limit := range []float64{math.NaN(), math.Inf(1), 0, -1} {
		c := &offeringClient{od: usdOffering(1)}
		_, err := enforceUSDCap(context.Background(), c, rec, "", limit)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "CUDLY_MCP_MAX_COMMITMENT_USD")
		assert.Zero(t, c.calls)
	}
}
