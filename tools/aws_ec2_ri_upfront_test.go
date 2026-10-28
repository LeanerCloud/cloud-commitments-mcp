package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdkec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
	"github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/ec2"
)

const threeYearSeconds = 3 * 365 * 24 * 3600

// pricedEC2API stands in for the AWS EC2 API under the real (*ec2.Client), so
// these tests run the provider's own offering lookup, pricing and go#611
// approved-upfront guard. It serves one matching offering per request and
// counts PurchaseReservedInstancesOffering calls. Only the methods the
// purchase path uses are implemented; any other call panics on the nil
// embedded interface.
type pricedEC2API struct {
	ec2.API
	describeErr     error
	offeringType    ec2types.OfferingTypeValues
	classesAsked    []ec2types.OfferingClassType
	purchaseCalls   int
	describeCalls   int
	fixedPrice      float32 // per instance, convertible
	standardPrice   float32 // per instance, standard
	hourlyRecurring float64
}

func newPricedEC2API(payment PaymentOption, fixedPrice float32) *pricedEC2API {
	f := &pricedEC2API{fixedPrice: fixedPrice, standardPrice: fixedPrice}
	switch payment {
	case PaymentOptionAllUpfront:
		f.offeringType = ec2types.OfferingTypeValuesAllUpfront
	case PaymentOptionPartialUpfront:
		f.offeringType = ec2types.OfferingTypeValuesPartialUpfront
		f.hourlyRecurring = 0.01
	default:
		f.offeringType = ec2types.OfferingTypeValuesNoUpfront
		f.hourlyRecurring = 0.05
	}
	return f
}

func (f *pricedEC2API) DescribeReservedInstancesOfferings(_ context.Context, in *sdkec2.DescribeReservedInstancesOfferingsInput, _ ...func(*sdkec2.Options)) (*sdkec2.DescribeReservedInstancesOfferingsOutput, error) {
	f.describeCalls++
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	class := ec2types.OfferingClassTypeConvertible
	switch {
	case in.OfferingClass != "":
		f.classesAsked = append(f.classesAsked, in.OfferingClass)
		class = in.OfferingClass
	case len(in.ReservedInstancesOfferingIds) == 1:
		// The by-ID detail lookup: serve the offering that ID names.
		class = ec2types.OfferingClassType(strings.TrimPrefix(in.ReservedInstancesOfferingIds[0], "offering-"))
	}
	price := f.fixedPrice
	if class == ec2types.OfferingClassTypeStandard {
		price = f.standardPrice
	}
	o := ec2types.ReservedInstancesOffering{
		ReservedInstancesOfferingId: aws.String("offering-" + string(class)),
		InstanceType:                ec2types.InstanceTypeM5Large,
		ProductDescription:          ec2types.RIProductDescriptionLinuxUnix,
		InstanceTenancy:             ec2types.TenancyDefault,
		Scope:                       ec2types.ScopeRegional,
		Duration:                    aws.Int64(threeYearSeconds),
		OfferingClass:               class,
		OfferingType:                f.offeringType,
		FixedPrice:                  aws.Float32(price),
		UsagePrice:                  aws.Float32(0),
		CurrencyCode:                ec2types.CurrencyCodeValuesUsd,
	}
	if f.hourlyRecurring > 0 {
		o.RecurringCharges = []ec2types.RecurringCharge{{Amount: aws.Float64(f.hourlyRecurring), Frequency: ec2types.RecurringChargeFrequencyHourly}}
	}
	return &sdkec2.DescribeReservedInstancesOfferingsOutput{ReservedInstancesOfferings: []ec2types.ReservedInstancesOffering{o}}, nil
}

// DescribeReservedInstances backs the provider's idempotency lookup: no RI
// carries the token yet, so the purchase proceeds to the price guard.
func (f *pricedEC2API) DescribeReservedInstances(context.Context, *sdkec2.DescribeReservedInstancesInput, ...func(*sdkec2.Options)) (*sdkec2.DescribeReservedInstancesOutput, error) {
	return &sdkec2.DescribeReservedInstancesOutput{}, nil
}

func (f *pricedEC2API) PurchaseReservedInstancesOffering(context.Context, *sdkec2.PurchaseReservedInstancesOfferingInput, ...func(*sdkec2.Options)) (*sdkec2.PurchaseReservedInstancesOfferingOutput, error) {
	f.purchaseCalls++
	return &sdkec2.PurchaseReservedInstancesOfferingOutput{ReservedInstancesId: aws.String("ri-fake")}, nil
}

func (f *pricedEC2API) CreateTags(context.Context, *sdkec2.CreateTagsInput, ...func(*sdkec2.Options)) (*sdkec2.CreateTagsOutput, error) {
	return &sdkec2.CreateTagsOutput{}, nil
}

// ec2ToolOverFakeAPI returns the tool wired to a real (*ec2.Client) whose
// AWS API is api, and a counter of provider resolutions.
func ec2ToolOverFakeAPI(api ec2.API) (*awsEC2RIPurchaseTool, *int) {
	client := ec2.NewClient(aws.Config{Region: "us-east-1"})
	client.SetEC2API(api)
	resolved := new(int)
	return &awsEC2RIPurchaseTool{
		createProvider: func(string, *provider.Config) (provider.Provider, error) {
			*resolved++
			return &recordingProvider{
				fakeProvider: &fakeProvider{name: "aws"}, client: client,
				gotService: new(common.ServiceType), gotRegion: new(string),
			}, nil
		},
	}, resolved
}

func upfrontEC2Args(payment PaymentOption, nonce string) ec2RIPurchaseArgs {
	args := validEC2Args()
	args.PaymentOption = string(payment)
	args.IdempotencyNonce = nonce
	return args
}

func confirmed(args ec2RIPurchaseArgs, upfront *float64, currency string) ec2RIPurchaseArgs {
	args.DryRun, args.Confirm = boolPtr(false), boolPtr(true)
	args.ApprovedUpfront, args.ApprovedCurrency = upfront, currency
	return args
}

// TestEC2UpfrontDryRunReturnsApprovedUpfront: the dry run fetches the
// offering and reports FixedPrice x count, without buying anything.
func TestEC2UpfrontDryRunReturnsApprovedUpfront(t *testing.T) {
	t.Parallel()
	for _, payment := range []PaymentOption{PaymentOptionPartialUpfront, PaymentOptionAllUpfront} {
		t.Run(string(payment), func(t *testing.T) {
			t.Parallel()
			api := newPricedEC2API(payment, 500.25)
			tool, resolved := ec2ToolOverFakeAPI(api)
			_, resp, err := tool.handle(context.Background(), nil, upfrontEC2Args(payment, "dry-"+string(payment)))
			require.NoError(t, err)
			assert.True(t, resp.DryRun)
			require.NotNil(t, resp.ApprovedUpfront)
			assert.InDelta(t, 1500.75, *resp.ApprovedUpfront, 1e-9, "3 x 500.25")
			assert.Equal(t, "USD", resp.ApprovedCurrency)
			assert.Equal(t, 1, *resolved)
			assert.Positive(t, api.describeCalls)
			assert.Zero(t, api.purchaseCalls, "a dry run must never buy")
		})
	}
}

// TestEC2UpfrontDryRunQuotesRequestedOfferingClass: a standard dry run
// quotes the standard offering, not the convertible default.
func TestEC2UpfrontDryRunQuotesRequestedOfferingClass(t *testing.T) {
	t.Parallel()
	api := newPricedEC2API(PaymentOptionAllUpfront, 900)
	api.standardPrice = 400
	tool, _ := ec2ToolOverFakeAPI(api)
	args := upfrontEC2Args(PaymentOptionAllUpfront, "dry-standard")
	args.OfferingClass = string(OfferingClassStandard)
	_, resp, err := tool.handle(context.Background(), nil, args)
	require.NoError(t, err)
	require.NotNil(t, resp.ApprovedUpfront)
	assert.InDelta(t, 1200.0, *resp.ApprovedUpfront, 1e-9)
	assert.NotEmpty(t, api.classesAsked)
	for _, c := range api.classesAsked {
		assert.Equal(t, ec2types.OfferingClassTypeStandard, c)
	}
}

func TestEC2UpfrontDryRunFailsWhenOfferingCannotBePriced(t *testing.T) {
	t.Parallel()
	api := newPricedEC2API(PaymentOptionPartialUpfront, 500)
	api.describeErr = errors.New("throttled")
	tool, _ := ec2ToolOverFakeAPI(api)
	_, _, err := tool.handle(context.Background(), nil, upfrontEC2Args(PaymentOptionPartialUpfront, "dry-fail"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dry run: cannot fetch the EC2 offering's upfront price")
	assert.Contains(t, err.Error(), "throttled")
	assert.Zero(t, api.purchaseCalls)
}

func TestEC2UpfrontQuoteFromOfferingRejectsUnusablePrices(t *testing.T) {
	t.Parallel()
	rec := common.Recommendation{Count: 2, PaymentOption: string(PaymentOptionAllUpfront)}
	for name, od := range map[string]*common.OfferingDetails{
		"nil":      nil,
		"zero":     {UpfrontCost: 0, Currency: "USD"},
		"negative": {UpfrontCost: -1, Currency: "USD"},
		"nan":      {UpfrontCost: math.NaN(), Currency: "USD"},
		"inf":      {UpfrontCost: math.Inf(1), Currency: "USD"},
		"overflow": {UpfrontCost: math.MaxFloat64, Currency: "USD"},
		"eur":      {UpfrontCost: 10, Currency: "EUR"},
		"blank":    {UpfrontCost: 10, Currency: ""},
	} {
		_, err := quoteFromOffering(od, rec)
		assert.Error(t, err, name)
	}
	q, err := quoteFromOffering(&common.OfferingDetails{UpfrontCost: 0.1, Currency: "USD"}, common.Recommendation{Count: 3})
	require.NoError(t, err)
	assert.InDelta(t, 0.3, q.total, 1e-12, "rounded to cents")
}

// TestEC2UpfrontPurchaseAtApprovedBuys: confirming with the dry run's own
// figure (offering unchanged, +0%) buys exactly once, and the figure reaches
// the provider as CommitmentCost/CommitmentCurrency.
func TestEC2UpfrontPurchaseAtApprovedBuys(t *testing.T) {
	t.Parallel()
	for _, payment := range []PaymentOption{PaymentOptionPartialUpfront, PaymentOptionAllUpfront} {
		t.Run(string(payment), func(t *testing.T) {
			t.Parallel()
			api := newPricedEC2API(payment, 500)
			tool, _ := ec2ToolOverFakeAPI(api)
			args := upfrontEC2Args(payment, "buy-at-approved-"+string(payment))
			_, preview, err := tool.handle(context.Background(), nil, args)
			require.NoError(t, err)
			require.NotNil(t, preview.ApprovedUpfront)

			_, resp, err := tool.handle(context.Background(), nil, confirmed(args, preview.ApprovedUpfront, preview.ApprovedCurrency))
			require.NoError(t, err)
			assert.True(t, resp.Success)
			assert.Equal(t, "ri-fake", resp.CommitmentID)
			assert.Equal(t, 1, api.purchaseCalls)
		})
	}
}

// TestEC2UpfrontPurchaseAboveToleranceIsRefused: the offering is repriced
// more than 5% above the approved figure between the dry run and the
// confirming call, so go#611's guard refuses and nothing is bought.
func TestEC2UpfrontPurchaseAboveToleranceIsRefused(t *testing.T) {
	t.Parallel()
	for _, payment := range []PaymentOption{PaymentOptionPartialUpfront, PaymentOptionAllUpfront} {
		t.Run(string(payment), func(t *testing.T) {
			t.Parallel()
			api := newPricedEC2API(payment, 500)
			tool, _ := ec2ToolOverFakeAPI(api)
			args := upfrontEC2Args(payment, "refuse-above-"+string(payment))
			_, preview, err := tool.handle(context.Background(), nil, args)
			require.NoError(t, err)
			require.NotNil(t, preview.ApprovedUpfront)

			api.fixedPrice = 526 // +5.2%: 3 x 526 = 1578 > 1500 x 1.05 = 1575
			_, _, err = tool.handle(context.Background(), nil, confirmed(args, preview.ApprovedUpfront, preview.ApprovedCurrency))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "exceeds the approved upfront")
			assert.Zero(t, api.purchaseCalls, "no PurchaseReservedInstancesOffering call")
		})
	}
}

// TestEC2UpfrontPurchaseWithinToleranceBuys pins the other side of the
// boundary: +4% is within go#611's tolerance.
func TestEC2UpfrontPurchaseWithinToleranceBuys(t *testing.T) {
	t.Parallel()
	api := newPricedEC2API(PaymentOptionAllUpfront, 520) // 3 x 520 = 1560 <= 1575
	tool, _ := ec2ToolOverFakeAPI(api)
	_, resp, err := tool.handle(context.Background(), nil,
		confirmed(upfrontEC2Args(PaymentOptionAllUpfront, "within-tolerance"), floatPtr(1500), "USD"))
	require.NoError(t, err)
	assert.True(t, resp.Success)
	assert.Equal(t, 1, api.purchaseCalls)
}

// TestEC2UpfrontPurchaseCarriesApprovedFigure pins the wiring the guard
// depends on: CommitmentCost and CommitmentCurrency are what the caller
// approved.
func TestEC2UpfrontPurchaseCarriesApprovedFigure(t *testing.T) {
	t.Parallel()
	fake := &fakeServiceClient{purchaseResult: common.PurchaseResult{Success: true, CommitmentID: "ri-abc"}}
	tool := &awsEC2RIPurchaseTool{createProvider: func(string, *provider.Config) (provider.Provider, error) {
		return &recordingProvider{fakeProvider: &fakeProvider{name: "aws"}, client: fake, gotService: new(common.ServiceType), gotRegion: new(string)}, nil
	}}
	_, _, err := tool.handle(context.Background(), nil,
		confirmed(upfrontEC2Args(PaymentOptionPartialUpfront, "carries"), floatPtr(1234.56), "USD"))
	require.NoError(t, err)
	require.Equal(t, 1, fake.purchaseCalls)
	got := fake.purchaseResult.Recommendation
	assert.InDelta(t, 1234.56, got.CommitmentCost, 1e-9)
	assert.Equal(t, "USD", got.CommitmentCurrency)
}

// TestEC2UpfrontPurchaseWithoutFigureIsRefused: a confirmed upfront
// purchase missing either field is refused before any provider resolution,
// so neither credentials nor AWS are touched and nothing is bought.
func TestEC2UpfrontPurchaseWithoutFigureIsRefused(t *testing.T) {
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
				api := newPricedEC2API(payment, 500)
				tool, resolved := ec2ToolOverFakeAPI(api)
				_, _, err := tool.handle(context.Background(), nil,
					confirmed(upfrontEC2Args(payment, "missing"), tc.upfront, tc.currency))
				require.Error(t, err)
				assert.Contains(t, err.Error(), "dry_run=true first")
				assert.Contains(t, err.Error(), "approved_upfront")
				assert.Zero(t, *resolved, "refused before resolving a provider")
				assert.Zero(t, api.describeCalls+api.purchaseCalls, "no AWS call")
			})
		}
	}
}

// TestEC2UpfrontRejectsUnusableFigures: every value go#611 would skip or
// cannot compare (zero, negative, NaN, Inf) and any currency but USD is
// refused before any provider call, in a dry run as well.
func TestEC2UpfrontRejectsUnusableFigures(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		upfront  *float64
		currency string
		want     string
	}{
		"zero":        {floatPtr(0), "USD", "invalid approved_upfront"},
		"negative":    {floatPtr(-1), "USD", "invalid approved_upfront"},
		"nan":         {floatPtr(math.NaN()), "USD", "invalid approved_upfront"},
		"inf":         {floatPtr(math.Inf(1)), "USD", "invalid approved_upfront"},
		"eur":         {floatPtr(1500), "EUR", "invalid approved_currency"},
		"lower usd":   {floatPtr(1500), "usd", "invalid approved_currency"},
		"padded usd":  {floatPtr(1500), " USD", "invalid approved_currency"},
		"blank space": {floatPtr(1500), " ", "invalid approved_currency"},
	} {
		for _, dryRun := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/dry_run=%t", name, dryRun), func(t *testing.T) {
				t.Parallel()
				api := newPricedEC2API(PaymentOptionAllUpfront, 500)
				tool, resolved := ec2ToolOverFakeAPI(api)
				args := confirmed(upfrontEC2Args(PaymentOptionAllUpfront, "bad"), tc.upfront, tc.currency)
				args.DryRun = boolPtr(dryRun)
				_, _, err := tool.handle(context.Background(), nil, args)
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.want)
				assert.Zero(t, *resolved)
				assert.Zero(t, api.purchaseCalls)
			})
		}
	}
}

// TestEC2NoUpfrontBuysWithoutFigure: no-upfront purchases need no figure,
// and their dry run still never resolves a provider.
func TestEC2NoUpfrontBuysWithoutFigure(t *testing.T) {
	t.Parallel()
	api := newPricedEC2API(PaymentOptionNoUpfront, 0)
	tool, resolved := ec2ToolOverFakeAPI(api)
	args := upfrontEC2Args(PaymentOptionNoUpfront, "no-upfront")

	_, preview, err := tool.handle(context.Background(), nil, args)
	require.NoError(t, err)
	assert.Nil(t, preview.ApprovedUpfront)
	assert.Empty(t, preview.ApprovedCurrency)
	assert.Zero(t, *resolved, "a no-upfront dry run never contacts AWS")

	_, resp, err := tool.handle(context.Background(), nil, confirmed(args, nil, ""))
	require.NoError(t, err)
	assert.True(t, resp.Success)
	assert.Equal(t, 1, api.purchaseCalls)
}

func TestEC2NoUpfrontRejectsApprovedFigure(t *testing.T) {
	t.Parallel()
	for name, args := range map[string]ec2RIPurchaseArgs{
		"upfront":  confirmed(upfrontEC2Args(PaymentOptionNoUpfront, "x"), floatPtr(10), ""),
		"currency": confirmed(upfrontEC2Args(PaymentOptionNoUpfront, "x"), nil, "USD"),
	} {
		tool, resolved := ec2ToolOverFakeAPI(newPricedEC2API(PaymentOptionNoUpfront, 0))
		_, _, err := tool.handle(context.Background(), nil, args)
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "apply only to partial-upfront and all-upfront", name)
		assert.Zero(t, *resolved, name)
	}
}

// An unconfirmed real call keeps its existing confirm=true refusal rather
// than the missing-figure one.
func TestEC2UpfrontUnconfirmedKeepsConfirmRefusal(t *testing.T) {
	t.Parallel()
	tool, resolved := ec2ToolOverFakeAPI(newPricedEC2API(PaymentOptionAllUpfront, 500))
	args := upfrontEC2Args(PaymentOptionAllUpfront, "unconfirmed")
	args.DryRun, args.Confirm = boolPtr(false), boolPtr(false)
	_, _, err := tool.handle(context.Background(), nil, args)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dry_run=false requires confirm=true")
	assert.Zero(t, *resolved)
}

// TestEC2UpfrontRoundTripOverMCP drives both calls through the MCP protocol:
// the registered schema accepts the new arguments, the dry run's structured
// output carries approved_upfront/approved_currency, and copying them into
// the confirming call buys.
func TestEC2UpfrontRoundTripOverMCP(t *testing.T) {
	t.Parallel()
	api := newPricedEC2API(PaymentOptionPartialUpfront, 500)
	tool, _ := ec2ToolOverFakeAPI(api)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server"}, nil)
	require.NoError(t, tool.Register(server))
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	require.NoError(t, err)
	defer serverSession.Close()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test-client"}, nil).Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	defer session.Close()

	call := func(args map[string]any) map[string]any {
		t.Helper()
		result, callErr := session.CallTool(ctx, &mcp.CallToolParams{Name: awsEC2RIPurchaseName, Arguments: args})
		require.NoError(t, callErr)
		require.False(t, result.IsError, "%v", result.Content)
		raw, marshalErr := json.Marshal(result.StructuredContent)
		require.NoError(t, marshalErr)
		var fields map[string]any
		require.NoError(t, json.Unmarshal(raw, &fields))
		return fields
	}
	args := map[string]any{
		"region": "us-east-1", "instance_type": "m5.large", "count": 3, "term_years": 3,
		"payment_option": "partial-upfront", "aws_profile": "test-profile", "idempotency_nonce": "mcp-round-trip",
	}
	preview := call(args)
	assert.Equal(t, 1500.0, preview["approved_upfront"])
	assert.Equal(t, "USD", preview["approved_currency"])

	args["dry_run"], args["confirm"] = false, true
	args["approved_upfront"], args["approved_currency"] = preview["approved_upfront"], preview["approved_currency"]
	bought := call(args)
	assert.Equal(t, true, bought["success"])
	assert.NotContains(t, bought, "approved_upfront", "only the dry run reports the quote")
	assert.Equal(t, 1, api.purchaseCalls)
}

func floatPtr(v float64) *float64 { return &v }
