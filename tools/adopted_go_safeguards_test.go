package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	awsrds "github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
	"github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/ec2"
	"github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/rds"
)

// Consumer regressions for the cloud-commitments-go purchase safeguards pinned
// at 8a3d92b (go#297 EC2/Redshift single send, go#300 RDS exact offering
// match, go#210 EC2 tenancy/scope). Real library clients run behind fakes of
// their AWS API seams; no credentials or network.
//
// Pin-sensitive (fail at the previous pin 9962786): the two RDS tests.
// Pin-insensitive guards (pass at both pins): the EC2 defaults test and the
// outcome-unknown wrapping test.

type fakeRDSAPI struct {
	offerings     []rdstypes.ReservedDBInstancesOffering
	purchasedIDs  []string
	describeCalls int
	purchaseCalls int
}

func (f *fakeRDSAPI) DescribeReservedDBInstancesOfferings(_ context.Context, _ *awsrds.DescribeReservedDBInstancesOfferingsInput, _ ...func(*awsrds.Options)) (*awsrds.DescribeReservedDBInstancesOfferingsOutput, error) {
	f.describeCalls++
	return &awsrds.DescribeReservedDBInstancesOfferingsOutput{ReservedDBInstancesOfferings: f.offerings}, nil
}

func (f *fakeRDSAPI) PurchaseReservedDBInstancesOffering(_ context.Context, in *awsrds.PurchaseReservedDBInstancesOfferingInput, _ ...func(*awsrds.Options)) (*awsrds.PurchaseReservedDBInstancesOfferingOutput, error) {
	f.purchaseCalls++
	f.purchasedIDs = append(f.purchasedIDs, aws.ToString(in.ReservedDBInstancesOfferingId))
	return &awsrds.PurchaseReservedDBInstancesOfferingOutput{ReservedDBInstance: &rdstypes.ReservedDBInstance{
		ReservedDBInstanceId: in.ReservedDBInstanceId,
		DBInstanceCount:      in.DBInstanceCount,
		FixedPrice:           aws.Float64(0),
	}}, nil
}

func (f *fakeRDSAPI) DescribeReservedDBInstances(_ context.Context, _ *awsrds.DescribeReservedDBInstancesInput, _ ...func(*awsrds.Options)) (*awsrds.DescribeReservedDBInstancesOutput, error) {
	return &awsrds.DescribeReservedDBInstancesOutput{}, nil
}

// rdsToolWithFake wires the RDS tool to a real rds.Client over fake.
func rdsToolWithFake(fake *fakeRDSAPI) *awsRDSRIPurchaseTool {
	return &awsRDSRIPurchaseTool{createProvider: func(string, *provider.ProviderConfig) (provider.Provider, error) {
		return rdsFakeProvider{client: fake}, nil
	}}
}

type rdsFakeProvider struct {
	provider.Provider
	client *fakeRDSAPI
}

func (p rdsFakeProvider) GetServiceClient(_ context.Context, _ common.ServiceType, _ string) (provider.ServiceClient, error) {
	c := rds.NewClient(aws.Config{Region: "us-east-1"})
	c.SetRDSAPI(p.client)
	return c, nil
}

func realRDSPurchaseArgs(engine string) rdsRIPurchaseArgs {
	dryRun, confirm := false, true
	a := validRDSArgs()
	a.DryRun, a.Confirm, a.Engine = &dryRun, &confirm, engine
	return a
}

func rdsOffering(id, engine string) rdstypes.ReservedDBInstancesOffering {
	return rdstypes.ReservedDBInstancesOffering{
		ReservedDBInstancesOfferingId: aws.String(id),
		ProductDescription:            aws.String(engine),
		DBInstanceClass:               aws.String("db.r6g.large"),
		MultiAZ:                       aws.Bool(true),
		Duration:                      aws.Int32(3 * 365 * 24 * 3600),
		OfferingType:                  aws.String("No Upfront"),
	}
}

// Pin-sensitive: at 9962786 the library did not refuse licensed engines.
func TestRDSPurchaseRefusesLicensedEngineBeforeAnyAPICall(t *testing.T) {
	for _, engine := range []string{"oracle-se2", "sqlserver-ee"} {
		t.Run(engine, func(t *testing.T) {
			t.Setenv(EnvAuditLog, filepath.Join(t.TempDir(), "audit.jsonl"))
			fake := &fakeRDSAPI{offerings: []rdstypes.ReservedDBInstancesOffering{rdsOffering("off-lic", engine)}}
			_, _, err := rdsToolWithFake(fake).handle(context.Background(), nil, realRDSPurchaseArgs(engine))
			require.Error(t, err)
			assert.Zero(t, fake.describeCalls, "licensed engines must be refused before any Describe call")
			assert.Zero(t, fake.purchaseCalls, "licensed engines must never reach Purchase")
		})
	}
}

// Pin-sensitive: at 9962786 the first partial-match offering was purchased.
func TestRDSPurchaseRequiresExactOfferingMatch(t *testing.T) {
	t.Run("aurora-mysql offering is never bought for a mysql request", func(t *testing.T) {
		t.Setenv(EnvAuditLog, filepath.Join(t.TempDir(), "audit.jsonl"))
		fake := &fakeRDSAPI{offerings: []rdstypes.ReservedDBInstancesOffering{rdsOffering("off-aurora", "aurora-mysql")}}
		_, _, err := rdsToolWithFake(fake).handle(context.Background(), nil, realRDSPurchaseArgs("mysql"))
		require.Error(t, err)
		assert.Zero(t, fake.purchaseCalls)
	})
	t.Run("exact match purchases exactly that offering once", func(t *testing.T) {
		t.Setenv(EnvAuditLog, filepath.Join(t.TempDir(), "audit.jsonl"))
		fake := &fakeRDSAPI{offerings: []rdstypes.ReservedDBInstancesOffering{
			rdsOffering("off-aurora", "aurora-mysql"),
			rdsOffering("off-mysql", "mysql"),
		}}
		_, resp, err := rdsToolWithFake(fake).handle(context.Background(), nil, realRDSPurchaseArgs("mysql"))
		require.NoError(t, err)
		assert.True(t, resp.Success)
		assert.Equal(t, []string{"off-mysql"}, fake.purchasedIDs)
	})
}

type fakeEC2API struct {
	ec2.API
	describeCalls int
}

func (f *fakeEC2API) DescribeReservedInstancesOfferings(_ context.Context, _ *awsec2.DescribeReservedInstancesOfferingsInput, _ ...func(*awsec2.Options)) (*awsec2.DescribeReservedInstancesOfferingsOutput, error) {
	f.describeCalls++
	return &awsec2.DescribeReservedInstancesOfferingsOutput{}, nil
}

// Pin-insensitive guard for go#210 (the library now rejects an empty or
// unknown EC2 tenancy/scope): the tool always fills both, so an omitted value
// reaches the library as default/region and clears its parsing. An invalid
// value is rejected by the tool's own validation, before any library call.
func TestEC2OmittedTenancyAndScopeStillAcceptedByLibrary(t *testing.T) {
	t.Parallel()
	args := ec2RIPurchaseArgs{Region: "us-east-1", InstanceType: "m5.large", Count: 1, TermYears: 1, PaymentOption: "no-upfront"}
	rec, _, _, _, err := ec2RecommendationFromArgs(args)
	require.NoError(t, err)
	d, ok := rec.Details.(*common.ComputeDetails)
	require.True(t, ok)
	assert.Equal(t, "default", d.Tenancy)
	assert.Equal(t, "region", d.Scope)

	fake := &fakeEC2API{}
	client := ec2.NewClient(aws.Config{Region: "us-east-1"})
	client.SetEC2API(fake)
	err = client.ValidateOffering(context.Background(), rec)
	require.Error(t, err, "the empty fake lists no offerings")
	assert.NotContains(t, err.Error(), "unsupported EC2 RI", "tenancy/scope must clear the library's parsing")
	assert.Positive(t, fake.describeCalls, "the lookup must have reached the API")

	for _, bad := range []struct{ field, val string }{{"tenancy", "host"}, {"scope", "zone"}} {
		a := args
		if bad.field == "tenancy" {
			a.Tenancy = bad.val
		} else {
			a.Scope = bad.val
		}
		_, _, _, _, err := ec2RecommendationFromArgs(a)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid "+bad.field, "rejected by the tool before the library is reached")
	}
}

// Pin-insensitive, fake-based. providers/aws/internal/purchasecfg is
// unimportable, so the library's ErrOutcomeUnknown wording is reproduced
// verbatim from purchasecfg.ClassifyPurchaseError at 8a3d92b. ExecutePurchase
// must surface it intact (%w) and audit the attempt as an error.
func TestExecutePurchaseSurfacesOutcomeUnknownVerbatim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	t.Setenv(EnvAuditLog, path)
	cause := errors.New("RequestCanceled: request context canceled")
	providerErr := fmt.Errorf("%w: the request may have been sent and the commitment may exist; reconcile before retrying: %w",
		errors.New("purchase outcome unknown"), cause)
	fake := &fakeServiceClient{purchaseErr: providerErr}

	_, err := ExecutePurchase(context.Background(), PurchaseRequest{
		Region: "us-east-1", Recommendation: testRecommendation(), DryRun: false, Confirm: true,
		CredentialScope: "test-scope",
		ResolveClient:   func(_ context.Context) (provider.ServiceClient, error) { return fake, nil },
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "purchase outcome unknown: the request may have been sent and the commitment may exist; reconcile before retrying")
	assert.ErrorIs(t, err, cause)
	assert.Equal(t, 1, fake.purchaseCalls, "no automatic re-send")

	lines := readAuditLines(t, path)
	require.Len(t, lines, 1)
	var record common.AuditRecord
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &record))
	assert.Equal(t, "error", record.Status)
}
