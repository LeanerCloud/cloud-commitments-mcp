package tools

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmemorydb "github.com/aws/aws-sdk-go-v2/service/memorydb"
	mdbtypes "github.com/aws/aws-sdk-go-v2/service/memorydb/types"
	awsopensearch "github.com/aws/aws-sdk-go-v2/service/opensearch"
	ostypes "github.com/aws/aws-sdk-go-v2/service/opensearch/types"
	awsredshift "github.com/aws/aws-sdk-go-v2/service/redshift"
	rstypes "github.com/aws/aws-sdk-go-v2/service/redshift/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
	"github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/memorydb"
	"github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/opensearch"
	"github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/redshift"
)

// Boundary tests for the OpenSearch, Redshift and MemoryDB approved-upfront
// guards (go#611): the real provider clients run behind fakes of their AWS
// API seams. With count 1 and approved 1000, an offering at 1050.00 buys and
// one at 1050.01 is refused before any purchase call.

const guardOneYearSeconds = 365 * 24 * 3600

type guardOpenSearchAPI struct {
	opensearch.API
	offering      ostypes.ReservedInstanceOffering
	purchaseCalls int
}

func (f *guardOpenSearchAPI) DescribeReservedInstanceOfferings(context.Context, *awsopensearch.DescribeReservedInstanceOfferingsInput, ...func(*awsopensearch.Options)) (*awsopensearch.DescribeReservedInstanceOfferingsOutput, error) {
	return &awsopensearch.DescribeReservedInstanceOfferingsOutput{ReservedInstanceOfferings: []ostypes.ReservedInstanceOffering{f.offering}}, nil
}

func (f *guardOpenSearchAPI) DescribeReservedInstances(context.Context, *awsopensearch.DescribeReservedInstancesInput, ...func(*awsopensearch.Options)) (*awsopensearch.DescribeReservedInstancesOutput, error) {
	return &awsopensearch.DescribeReservedInstancesOutput{}, nil
}

func (f *guardOpenSearchAPI) PurchaseReservedInstanceOffering(context.Context, *awsopensearch.PurchaseReservedInstanceOfferingInput, ...func(*awsopensearch.Options)) (*awsopensearch.PurchaseReservedInstanceOfferingOutput, error) {
	f.purchaseCalls++
	return &awsopensearch.PurchaseReservedInstanceOfferingOutput{ReservedInstanceId: aws.String("ri-os")}, nil
}

func (f *guardOpenSearchAPI) AddTags(context.Context, *awsopensearch.AddTagsInput, ...func(*awsopensearch.Options)) (*awsopensearch.AddTagsOutput, error) {
	return &awsopensearch.AddTagsOutput{}, nil
}

type guardSTS struct{}

func (guardSTS) GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	return &sts.GetCallerIdentityOutput{Account: aws.String("fixture-account")}, nil
}

type guardRedshiftAPI struct {
	redshift.API
	offering      rstypes.ReservedNodeOffering
	purchaseCalls int
}

func (f *guardRedshiftAPI) DescribeReservedNodeOfferings(context.Context, *awsredshift.DescribeReservedNodeOfferingsInput, ...func(*awsredshift.Options)) (*awsredshift.DescribeReservedNodeOfferingsOutput, error) {
	return &awsredshift.DescribeReservedNodeOfferingsOutput{ReservedNodeOfferings: []rstypes.ReservedNodeOffering{f.offering}}, nil
}

// DescribeReservedNodes backs guardAgainstRepurchase: no node exists yet.
func (f *guardRedshiftAPI) DescribeReservedNodes(context.Context, *awsredshift.DescribeReservedNodesInput, ...func(*awsredshift.Options)) (*awsredshift.DescribeReservedNodesOutput, error) {
	return &awsredshift.DescribeReservedNodesOutput{}, nil
}

func (f *guardRedshiftAPI) PurchaseReservedNodeOffering(context.Context, *awsredshift.PurchaseReservedNodeOfferingInput, ...func(*awsredshift.Options)) (*awsredshift.PurchaseReservedNodeOfferingOutput, error) {
	f.purchaseCalls++
	return &awsredshift.PurchaseReservedNodeOfferingOutput{ReservedNode: &rstypes.ReservedNode{ReservedNodeId: aws.String("rn-1")}}, nil
}

type guardMemoryDBAPI struct {
	memorydb.API
	offering      mdbtypes.ReservedNodesOffering
	purchaseCalls int
}

func (f *guardMemoryDBAPI) DescribeReservedNodesOfferings(context.Context, *awsmemorydb.DescribeReservedNodesOfferingsInput, ...func(*awsmemorydb.Options)) (*awsmemorydb.DescribeReservedNodesOfferingsOutput, error) {
	return &awsmemorydb.DescribeReservedNodesOfferingsOutput{ReservedNodesOfferings: []mdbtypes.ReservedNodesOffering{f.offering}}, nil
}

func (f *guardMemoryDBAPI) DescribeReservedNodes(context.Context, *awsmemorydb.DescribeReservedNodesInput, ...func(*awsmemorydb.Options)) (*awsmemorydb.DescribeReservedNodesOutput, error) {
	return &awsmemorydb.DescribeReservedNodesOutput{}, nil
}

func (f *guardMemoryDBAPI) PurchaseReservedNodesOffering(context.Context, *awsmemorydb.PurchaseReservedNodesOfferingInput, ...func(*awsmemorydb.Options)) (*awsmemorydb.PurchaseReservedNodesOfferingOutput, error) {
	f.purchaseCalls++
	return &awsmemorydb.PurchaseReservedNodesOfferingOutput{ReservedNode: &mdbtypes.ReservedNode{ReservationId: aws.String("rn-1"), NodeCount: 1}}, nil
}

// guardService builds a real provider client over a fake serving one offering
// of the payment option at fixedPrice, and reports its purchase count.
type guardService struct {
	client  provider.ServiceClient
	ctor    func() Registration
	history func() int
}

func guardOpenSearch(payment PaymentOption, fixedPrice float64) guardService {
	o := ostypes.ReservedInstanceOffering{
		ReservedInstanceOfferingId: aws.String("off-os"), InstanceType: "r6g.large", Duration: guardOneYearSeconds,
		FixedPrice: aws.Float64(fixedPrice), CurrencyCode: aws.String("USD"),
		PaymentOption: ostypes.ReservedInstancePaymentOptionAllUpfront,
	}
	if payment == PaymentOptionNoUpfront {
		o.PaymentOption = ostypes.ReservedInstancePaymentOptionNoUpfront
		o.RecurringCharges = []ostypes.RecurringCharge{{RecurringChargeAmount: aws.Float64(0.05), RecurringChargeFrequency: aws.String("Hourly")}}
	}
	api := &guardOpenSearchAPI{offering: o}
	c := opensearch.NewClient(aws.Config{Region: "us-east-1"})
	c.SetOpenSearchAPI(api)
	c.SetSTSAPI(guardSTS{})
	return guardService{client: c, ctor: NewAWSOpenSearchRIPurchaseTool, history: func() int { return api.purchaseCalls }}
}

func guardRedshift(payment PaymentOption, fixedPrice float64) guardService {
	o := rstypes.ReservedNodeOffering{
		ReservedNodeOfferingId: aws.String("off-rs"), NodeType: aws.String("r6g.large"), Duration: aws.Int32(guardOneYearSeconds),
		ReservedNodeOfferingType: rstypes.ReservedNodeOfferingTypeRegular,
		FixedPrice:               aws.Float64(fixedPrice), CurrencyCode: aws.String("USD"),
	}
	if payment == PaymentOptionNoUpfront {
		o.FixedPrice = aws.Float64(0)
		o.RecurringCharges = []rstypes.RecurringCharge{{RecurringChargeAmount: aws.Float64(0.05), RecurringChargeFrequency: aws.String("Hourly")}}
	}
	api := &guardRedshiftAPI{offering: o}
	c := redshift.NewClient(aws.Config{Region: "us-east-1"})
	c.SetRedshiftAPI(api)
	return guardService{client: c, ctor: NewAWSRedshiftRIPurchaseTool, history: func() int { return api.purchaseCalls }}
}

func guardMemoryDBIn(region string, payment PaymentOption, fixedPrice float64) guardService {
	o := mdbtypes.ReservedNodesOffering{
		ReservedNodesOfferingId: aws.String("off-mdb"), NodeType: aws.String("r6g.large"), Duration: guardOneYearSeconds,
		OfferingType: aws.String("All Upfront"), FixedPrice: fixedPrice,
	}
	if payment == PaymentOptionNoUpfront {
		o.OfferingType = aws.String("No Upfront")
		o.FixedPrice = 0
		o.RecurringCharges = []mdbtypes.RecurringCharge{{RecurringChargeAmount: 0.05, RecurringChargeFrequency: aws.String("Hourly")}}
	}
	api := &guardMemoryDBAPI{offering: o}
	c := memorydb.NewClient(aws.Config{Region: region})
	c.SetMemoryDBAPI(api)
	return guardService{client: c, ctor: NewAWSMemoryDBRIPurchaseTool, history: func() int { return api.purchaseCalls }}
}

func guardMemoryDB(payment PaymentOption, fixedPrice float64) guardService {
	return guardMemoryDBIn("us-east-1", payment, fixedPrice)
}

var guardServices = map[string]func(PaymentOption, float64) guardService{
	"opensearch": guardOpenSearch,
	"redshift":   guardRedshift,
	"memorydb":   guardMemoryDB,
}

func guardArgs(nonce string, payment PaymentOption, upfront *float64, currency string) simpleAWSRIPurchaseArgs {
	args := upfrontSimpleArgs(payment, nonce)
	args.Count, args.ResourceType = 1, "r6g.large"
	return confirmedSimple(args, upfront, currency)
}

// Not parallel: Redshift's durable first-attempt store reads EnvBudgetState.
func guardEnv(t *testing.T) {
	t.Helper()
	t.Setenv(EnvBudgetState, filepath.Join(t.TempDir(), "budget.json"))
	t.Setenv(EnvAuditLog, filepath.Join(t.TempDir(), "audit.jsonl"))
}

func TestSimpleUpfrontGuardBoundaryOnRealClients(t *testing.T) {
	for product, build := range guardServices {
		for _, tc := range []struct {
			name  string
			price float64
			buys  bool
		}{
			{"at approved", 1000, true},
			{"at tolerance edge", 1050.00, true},
			{"just above tolerance", 1050.01, false},
		} {
			t.Run(product+"/"+tc.name, func(t *testing.T) {
				guardEnv(t)
				svc := build(PaymentOptionAllUpfront, tc.price)
				tool, _ := simpleToolOver(svc.ctor, svc.client)
				_, resp, err := tool.handle(context.Background(), nil,
					guardArgs("guard-"+product+tc.name, PaymentOptionAllUpfront, floatPtr(1000), "USD"))
				if tc.buys {
					require.NoError(t, err)
					assert.True(t, resp.Success)
					assert.Equal(t, 1, svc.history())
					return
				}
				require.Error(t, err)
				assert.Contains(t, err.Error(), "exceeds the approved upfront")
				assert.Zero(t, svc.history(), "a refused guard must never reach Purchase")
			})
		}
	}
}

func TestSimpleUpfrontDryRunQuotesRealClientOffering(t *testing.T) {
	for product, build := range guardServices {
		t.Run(product, func(t *testing.T) {
			guardEnv(t)
			svc := build(PaymentOptionAllUpfront, 525.5)
			tool, _ := simpleToolOver(svc.ctor, svc.client)
			args := upfrontSimpleArgs(PaymentOptionAllUpfront, "dry-"+product)
			args.Count, args.ResourceType = 2, "r6g.large"
			_, resp, err := tool.handle(context.Background(), nil, args)
			require.NoError(t, err)
			require.NotNil(t, resp.ApprovedUpfront)
			assert.InDelta(t, 1051.0, *resp.ApprovedUpfront, 1e-9)
			assert.Equal(t, "USD", resp.ApprovedCurrency)
			assert.Zero(t, svc.history())
		})
	}
}

// TestSimpleNoUpfrontConfirmedBuysOnRealClients: a no-upfront purchase needs
// no figure and still buys exactly once.
func TestSimpleNoUpfrontConfirmedBuysOnRealClients(t *testing.T) {
	for product, build := range guardServices {
		t.Run(product, func(t *testing.T) {
			guardEnv(t)
			svc := build(PaymentOptionNoUpfront, 0)
			tool, _ := simpleToolOver(svc.ctor, svc.client)
			_, resp, err := tool.handle(context.Background(), nil,
				guardArgs("no-upfront-"+product, PaymentOptionNoUpfront, nil, ""))
			require.NoError(t, err)
			assert.True(t, resp.Success)
			assert.Equal(t, 1, svc.history())
		})
	}
}

// TestMemoryDBUpfrontDryRunFailsOutsideUSDRegions: MemoryDB offerings carry
// no currency, so the client assumes USD by region and a cn-north-1 dry run
// fails instead of quoting an unusable figure.
func TestMemoryDBUpfrontDryRunFailsOutsideUSDRegions(t *testing.T) {
	guardEnv(t)
	svc := guardMemoryDBIn("cn-north-1", PaymentOptionAllUpfront, 1000)
	tool, _ := simpleToolOver(svc.ctor, svc.client)
	args := upfrontSimpleArgs(PaymentOptionAllUpfront, "dry-cn")
	args.Region, args.Count, args.ResourceType = "cn-north-1", 1, "r6g.large"
	_, _, err := tool.handle(context.Background(), nil, args)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MemoryDB")
	assert.Zero(t, svc.history())
}
