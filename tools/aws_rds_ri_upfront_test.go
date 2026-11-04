package tools

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
)

// rdsToolOver returns the RDS tool wired to client, and a counter of
// provider resolutions.
func rdsToolOver(client provider.ServiceClient) (*awsRDSRIPurchaseTool, *int) {
	resolved := new(int)
	return &awsRDSRIPurchaseTool{
		createProvider: func(string, *provider.Config) (provider.Provider, error) {
			*resolved++
			return &recordingProvider{
				fakeProvider: &fakeProvider{name: "aws"}, client: client,
				gotService: new(common.ServiceType), gotRegion: new(string),
			}, nil
		},
	}, resolved
}

func upfrontRDSArgs(payment PaymentOption, nonce string) rdsRIPurchaseArgs {
	args := validRDSArgs()
	args.PaymentOption = string(payment)
	args.IdempotencyNonce = nonce
	return args
}

func confirmedRDS(args rdsRIPurchaseArgs, upfront *float64, currency string) rdsRIPurchaseArgs {
	args.DryRun, args.Confirm = boolPtr(false), boolPtr(true)
	args.ApprovedUpfront, args.ApprovedCurrency = upfront, currency
	return args
}

func upfrontOffering(unit float64, currency string) *common.OfferingDetails {
	return &common.OfferingDetails{UpfrontCost: unit, TotalCost: unit * 2, Currency: currency}
}

func TestRDSUpfrontDryRunReturnsApprovedUpfront(t *testing.T) {
	t.Parallel()
	for _, payment := range []PaymentOption{PaymentOptionPartialUpfront, PaymentOptionAllUpfront} {
		t.Run(string(payment), func(t *testing.T) {
			t.Parallel()
			fake := &fakeServiceClient{offering: upfrontOffering(500.25, "USD")}
			tool, resolved := rdsToolOver(fake)
			_, resp, err := tool.handle(context.Background(), nil, upfrontRDSArgs(payment, "dry"))
			require.NoError(t, err)
			assert.True(t, resp.DryRun)
			require.NotNil(t, resp.ApprovedUpfront)
			assert.InDelta(t, 1000.5, *resp.ApprovedUpfront, 1e-9, "2 x 500.25")
			assert.Equal(t, "USD", resp.ApprovedCurrency)
			assert.Equal(t, 1, *resolved)
			assert.Equal(t, 1, fake.offeringCalls)
			assert.Zero(t, fake.purchaseCalls, "a dry run must never buy")
		})
	}
}

func TestRDSUpfrontDryRunQuoteErrorsNameRDS(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		od   *common.OfferingDetails
		want string
	}{
		"eur":  {upfrontOffering(10, "EUR"), "the RDS offering is priced in"},
		"zero": {upfrontOffering(0, "USD"), "RDS offering reports upfront"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tool, _ := rdsToolOver(&fakeServiceClient{offering: tc.od})
			_, _, err := tool.handle(context.Background(), nil, upfrontRDSArgs(PaymentOptionAllUpfront, "q"))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestRDSUpfrontDryRunFailsWhenOfferingCannotBePriced(t *testing.T) {
	t.Parallel()
	fake := &fakeServiceClient{offeringErr: fmt.Errorf("throttled")}
	tool, _ := rdsToolOver(fake)
	_, _, err := tool.handle(context.Background(), nil, upfrontRDSArgs(PaymentOptionPartialUpfront, "fail"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dry run: cannot fetch the RDS offering's upfront price")
	assert.Contains(t, err.Error(), "throttled")
	assert.Zero(t, fake.purchaseCalls)
}

func TestRDSUpfrontPurchaseCarriesApprovedFigure(t *testing.T) {
	t.Parallel()
	fake := &fakeServiceClient{purchaseResult: common.PurchaseResult{Success: true, CommitmentID: "rds-ri"}}
	tool, _ := rdsToolOver(fake)
	_, _, err := tool.handle(context.Background(), nil,
		confirmedRDS(upfrontRDSArgs(PaymentOptionPartialUpfront, "carries"), floatPtr(1234.56), "USD"))
	require.NoError(t, err)
	require.Equal(t, 1, fake.purchaseCalls)
	got := fake.purchaseResult.Recommendation
	assert.InDelta(t, 1234.56, got.CommitmentCost, 1e-9)
	assert.Equal(t, "USD", got.CommitmentCurrency)
}

func TestRDSUpfrontPurchaseWithoutFigureIsRefused(t *testing.T) {
	t.Parallel()
	for _, payment := range []PaymentOption{PaymentOptionPartialUpfront, PaymentOptionAllUpfront} {
		for name, tc := range map[string]struct {
			upfront  *float64
			currency string
		}{
			"both missing":     {nil, ""},
			"upfront missing":  {nil, "USD"},
			"currency missing": {floatPtr(1500), ""},
		} {
			t.Run(string(payment)+"/"+name, func(t *testing.T) {
				t.Parallel()
				fake := &fakeServiceClient{}
				tool, resolved := rdsToolOver(fake)
				_, _, err := tool.handle(context.Background(), nil,
					confirmedRDS(upfrontRDSArgs(payment, "missing"), tc.upfront, tc.currency))
				require.Error(t, err)
				assert.Contains(t, err.Error(), "dry_run=true first")
				assert.Zero(t, *resolved, "refused before resolving a provider")
				assert.Zero(t, fake.purchaseCalls+fake.offeringCalls)
			})
		}
	}
}

func TestRDSUpfrontRejectsUnusableFigures(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		upfront  *float64
		currency string
		want     string
	}{
		"zero":     {floatPtr(0), "USD", "invalid approved_upfront"},
		"negative": {floatPtr(-1), "USD", "invalid approved_upfront"},
		"nan":      {floatPtr(math.NaN()), "USD", "invalid approved_upfront"},
		"inf":      {floatPtr(math.Inf(1)), "USD", "invalid approved_upfront"},
		"eur":      {floatPtr(1500), "EUR", "the only currency the RDS price check supports"},
		"lower":    {floatPtr(1500), "usd", "invalid approved_currency"},
	} {
		for _, dryRun := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/dry_run=%t", name, dryRun), func(t *testing.T) {
				t.Parallel()
				fake := &fakeServiceClient{}
				tool, resolved := rdsToolOver(fake)
				args := confirmedRDS(upfrontRDSArgs(PaymentOptionAllUpfront, "bad"), tc.upfront, tc.currency)
				args.DryRun = boolPtr(dryRun)
				_, _, err := tool.handle(context.Background(), nil, args)
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.want)
				assert.Zero(t, *resolved)
				assert.Zero(t, fake.purchaseCalls)
			})
		}
	}
}

func TestRDSNoUpfrontNeedsNoFigure(t *testing.T) {
	t.Parallel()
	fake := &fakeServiceClient{purchaseResult: common.PurchaseResult{Success: true, CommitmentID: "rds-ri"}}
	tool, resolved := rdsToolOver(fake)
	args := upfrontRDSArgs(PaymentOptionNoUpfront, "no-upfront")

	_, preview, err := tool.handle(context.Background(), nil, args)
	require.NoError(t, err)
	assert.Nil(t, preview.ApprovedUpfront)
	assert.Empty(t, preview.ApprovedCurrency)
	assert.Zero(t, *resolved, "a no-upfront dry run never contacts AWS")

	_, resp, err := tool.handle(context.Background(), nil, confirmedRDS(args, nil, ""))
	require.NoError(t, err)
	assert.True(t, resp.Success)
	assert.Equal(t, 1, fake.purchaseCalls)

	for name, bad := range map[string]rdsRIPurchaseArgs{
		"upfront":  confirmedRDS(args, floatPtr(10), ""),
		"currency": confirmedRDS(args, nil, "USD"),
	} {
		_, _, err := tool.handle(context.Background(), nil, bad)
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "apply only to partial-upfront and all-upfront", name)
	}
}
