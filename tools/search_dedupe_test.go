package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/aws/aws-sdk-go-v2/aws"
	ecsdk "github.com/aws/aws-sdk-go-v2/service/elasticache"
	ectypes "github.com/aws/aws-sdk-go-v2/service/elasticache/types"
	ossdk "github.com/aws/aws-sdk-go-v2/service/opensearch"
	ostypes "github.com/aws/aws-sdk-go-v2/service/opensearch/types"
	rdssdk "github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	rssdk "github.com/aws/aws-sdk-go-v2/service/redshift"
	rstypes "github.com/aws/aws-sdk-go-v2/service/redshift/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
	eclib "github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/elasticache"
	oslib "github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/opensearch"
	rdslib "github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/rds"
	rslib "github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/redshift"
	"github.com/LeanerCloud/cloud-commitments-go/providers/gcp/services/computeengine"
)

const (
	dedupeCaller = "111111111111"
	dedupeOther  = "222222222222"
)

var dedupeRecent = time.Now().Add(-time.Hour)

// listingClient is a ServiceClient whose existing-commitment listing is
// scripted and counted.
type listingClient struct {
	err error
	fakeServiceClient
	commitments []common.Commitment
	listCalls   int
}

func (l *listingClient) GetExistingCommitments(_ context.Context) ([]common.Commitment, error) {
	l.listCalls++
	return l.commitments, l.err
}

// svcProvider routes GetServiceClient to a per-service client and counts calls.
type svcProvider struct {
	byService map[common.ServiceType]provider.ServiceClient
	fakeProvider
	calls int
}

func (p *svcProvider) GetServiceClient(_ context.Context, s common.ServiceType, _ string) (provider.ServiceClient, error) {
	p.calls++
	return p.byService[s], nil
}

func awsProvider(clients map[common.ServiceType]provider.ServiceClient) *svcProvider {
	return &svcProvider{
		fakeProvider: fakeProvider{name: "aws", accounts: []common.Account{
			{ID: dedupeCaller, IsDefault: true}, {ID: dedupeOther},
		}},
		byService: clients,
	}
}

func runDedupe(t *testing.T, p provider.Provider, recs []common.Recommendation) ([]common.Recommendation, searchDedupe, error) {
	t.Helper()
	return dedupeSearchResults(context.Background(), p, recs)
}

// The rec shapes below mirror the parsers in cloud-commitments-go
// providers/aws/recommendations/parser_services.go (resource type, normalized
// region, linked-account id, pointer Details), with the engine strings AWS
// reports in Cost Explorer.
func rdsRec(count int, term, payment, account string) common.Recommendation {
	return common.Recommendation{
		Provider: common.ProviderAWS, Service: common.ServiceRDS, Account: account, Region: "us-east-1",
		ResourceType: "db.r6g.large", Count: count, Term: term, PaymentOption: payment, EstimatedSavings: 1234,
		Details: &common.DatabaseDetails{Engine: "MySQL", AZConfig: "multi-az"},
	}
}

type rdsAPI struct {
	rdslib.API
	out []rdstypes.ReservedDBInstance
}

func (r rdsAPI) DescribeReservedDBInstances(_ context.Context, _ *rdssdk.DescribeReservedDBInstancesInput, _ ...func(*rdssdk.Options)) (*rdssdk.DescribeReservedDBInstancesOutput, error) {
	return &rdssdk.DescribeReservedDBInstancesOutput{ReservedDBInstances: r.out}, nil
}

func rdsClient() *rdslib.Client {
	c := rdslib.NewClient(aws.Config{Region: "us-east-1"})
	c.SetRDSAPI(rdsAPI{out: []rdstypes.ReservedDBInstance{{
		ReservedDBInstanceId: aws.String("ri-1"), DBInstanceClass: aws.String("db.r6g.large"),
		ProductDescription: aws.String("mysql"), MultiAZ: aws.Bool(true), DBInstanceCount: aws.Int32(2),
		State: aws.String("active"), StartTime: aws.Time(dedupeRecent), Duration: aws.Int32(31536000),
	}}})
	return c
}

func TestDedupeRDSFullyCoveredIsSuppressedAndListed(t *testing.T) {
	p := awsProvider(map[common.ServiceType]provider.ServiceClient{common.ServiceRDS: rdsClient()})
	kept, d, err := runDedupe(t, p, []common.Recommendation{rdsRec(2, "1yr", "all-upfront", dedupeCaller)})
	require.NoError(t, err)
	assert.Empty(t, kept)
	require.Len(t, d.Suppressed, 1)
	s := d.Suppressed[0]
	assert.Equal(t, "1yr", s.Term)
	assert.Equal(t, "all-upfront", s.Payment)
	assert.Equal(t, dedupeCaller, s.Account)
	assert.Equal(t, 2, s.Count)
	assert.NotEmpty(t, s.Reason)
}

func TestDedupePartialCoverKeepsRecUnchangedWithCoveredCount(t *testing.T) {
	p := awsProvider(map[common.ServiceType]provider.ServiceClient{common.ServiceRDS: rdsClient()})
	rec := rdsRec(3, "1yr", "all-upfront", dedupeCaller)
	kept, d, err := runDedupe(t, p, []common.Recommendation{rec})
	require.NoError(t, err)
	require.Len(t, kept, 1)
	assert.Equal(t, rec, kept[0], "a partially covered rec must keep its count, cost and savings")
	require.Len(t, d.Flagged, 1)
	assert.Equal(t, statusPartiallyCovered, d.Flagged[0].Status)
	assert.Equal(t, 2, d.Flagged[0].CoveredCount)
	assert.Equal(t, 0, d.Flagged[0].RecommendationIndex)
	assert.Empty(t, d.Suppressed)
}

func TestDedupeVariantsOfSameDemandAreTreatedAlike(t *testing.T) {
	p := awsProvider(map[common.ServiceType]provider.ServiceClient{common.ServiceRDS: rdsClient()})
	recs := make([]common.Recommendation, 0, 6)
	for _, term := range []string{"1yr", "3yr"} {
		for _, pay := range []string{"all-upfront", "partial-upfront", "no-upfront"} {
			recs = append(recs, rdsRec(2, term, pay, dedupeCaller))
		}
	}
	kept, d, err := runDedupe(t, p, recs)
	require.NoError(t, err)
	assert.Empty(t, kept, "all six alternatives for the same covered demand must be suppressed, not just the first")
	assert.Len(t, d.Suppressed, 6)
}

func TestDedupeOtherAccountIsNeverSuppressed(t *testing.T) {
	p := awsProvider(map[common.ServiceType]provider.ServiceClient{common.ServiceRDS: rdsClient()})
	kept, d, err := runDedupe(t, p, []common.Recommendation{
		rdsRec(2, "1yr", "all-upfront", dedupeOther),
		rdsRec(2, "1yr", "no-upfront", ""),
	})
	require.NoError(t, err)
	assert.Len(t, kept, 2)
	assert.Empty(t, d.Suppressed)
	require.Len(t, d.Flagged, 2)
	for _, f := range d.Flagged {
		assert.Equal(t, statusNotCheckedAccount, f.Status)
	}
}

func TestDedupeNothingRecentLeavesRecsUntouched(t *testing.T) {
	p := awsProvider(map[common.ServiceType]provider.ServiceClient{common.ServiceRDS: &listingClient{}})
	rec := rdsRec(2, "1yr", "all-upfront", dedupeCaller)
	kept, d, err := runDedupe(t, p, []common.Recommendation{rec})
	require.NoError(t, err)
	assert.Equal(t, []common.Recommendation{rec}, kept)
	assert.Empty(t, d.Flagged)
	assert.Empty(t, d.Suppressed)
	require.Len(t, d.Groups, 1)
	assert.Equal(t, "checked", d.Groups[0].Status)
}

func TestDedupeListsOncePerServiceRegion(t *testing.T) {
	lc := &listingClient{}
	p := awsProvider(map[common.ServiceType]provider.ServiceClient{common.ServiceRDS: lc})
	recs := []common.Recommendation{
		rdsRec(2, "1yr", "all-upfront", dedupeCaller), rdsRec(2, "3yr", "all-upfront", dedupeCaller),
		rdsRec(2, "1yr", "no-upfront", dedupeCaller),
	}
	_, _, err := runDedupe(t, p, recs)
	require.NoError(t, err)
	assert.Equal(t, 1, lc.listCalls, "one listing per (service, region) per search, however many variants")
	assert.Equal(t, 1, p.calls)
}

func TestDedupeEC2IsFlagOnlyNeverHidden(t *testing.T) {
	lc := &listingClient{commitments: []common.Commitment{{
		Provider: common.ProviderAWS, Service: common.ServiceEC2, Region: "us-east-1", ResourceType: "m5.large",
		Count: 10, State: common.CommitmentStateActive, StartDate: dedupeRecent,
	}}}
	p := awsProvider(map[common.ServiceType]provider.ServiceClient{common.ServiceEC2: lc})
	rec := common.Recommendation{
		Provider: common.ProviderAWS, Service: common.ServiceEC2, Account: dedupeCaller, Region: "us-east-1",
		ResourceType: "m5.large", Count: 4, Term: "1yr", PaymentOption: "no-upfront",
		Details: &common.ComputeDetails{InstanceType: "m5.large", Platform: "Linux/UNIX"},
	}
	kept, d, err := runDedupe(t, p, []common.Recommendation{rec})
	require.NoError(t, err)
	assert.Equal(t, []common.Recommendation{rec}, kept, "a lossy key must never hide a recommendation")
	require.Len(t, d.Flagged, 1)
	assert.Equal(t, statusPossiblyCovered, d.Flagged[0].Status)
	assert.Empty(t, d.Suppressed)
}

func TestDedupeSavingsPlansAreNotApplied(t *testing.T) {
	p := awsProvider(nil)
	rec := common.Recommendation{Provider: common.ProviderAWS, Service: common.ServiceSavingsPlansCompute, Account: dedupeCaller, Region: "us-east-1", Count: 1}
	kept, d, err := runDedupe(t, p, []common.Recommendation{rec})
	require.NoError(t, err)
	assert.Equal(t, []common.Recommendation{rec}, kept)
	require.Len(t, d.Groups, 1)
	assert.Equal(t, "not_applied", d.Groups[0].Status)
	assert.Equal(t, 0, p.calls, "no client is resolved for an unchecked group")
}

func TestDedupeListingFailureFailsLoudNamingThePermission(t *testing.T) {
	lc := &listingClient{err: errors.New("AccessDenied")}
	p := awsProvider(map[common.ServiceType]provider.ServiceClient{common.ServiceRDS: lc})
	kept, _, err := runDedupe(t, p, []common.Recommendation{rdsRec(2, "1yr", "all-upfront", dedupeCaller)})
	require.Error(t, err)
	assert.Nil(t, kept)
	assert.Contains(t, err.Error(), "rds:DescribeReservedDBInstances")
	assert.Contains(t, err.Error(), "AccessDenied")
}

func TestDedupeNoCallerAccountFlagsEverything(t *testing.T) {
	p := awsProvider(map[common.ServiceType]provider.ServiceClient{common.ServiceRDS: &listingClient{}})
	p.accounts = nil
	// No default account: nothing may be suppressed, everything is flagged.
	kept, d, err := runDedupe(t, p, []common.Recommendation{rdsRec(2, "1yr", "all-upfront", dedupeCaller)})
	require.NoError(t, err)
	assert.Len(t, kept, 1)
	assert.Equal(t, statusNotCheckedAccount, d.Flagged[0].Status)
}

func TestDedupeElastiCacheWildcardEngineNeverSuppresses(t *testing.T) {
	cache := func(engine string) common.Recommendation {
		return common.Recommendation{
			Provider: common.ProviderAWS, Service: common.ServiceElastiCache, Account: dedupeCaller, Region: "us-east-1",
			ResourceType: "cache.r6g.large", Count: 2, Term: "1yr", PaymentOption: "no-upfront",
			Details: &common.CacheDetails{NodeType: "cache.r6g.large", Engine: engine},
		}
	}
	for engine, wantSuppressed := range map[string]bool{"redis": true, "": false} {
		c := eclib.NewClient(aws.Config{Region: "us-east-1"})
		c.SetElastiCacheAPI(ecAPI{engine: engine})
		p := awsProvider(map[common.ServiceType]provider.ServiceClient{common.ServiceElastiCache: c})
		kept, d, err := runDedupe(t, p, []common.Recommendation{cache("Redis")})
		require.NoError(t, err)
		if wantSuppressed {
			assert.Empty(t, kept, "engine %q", engine)
			assert.Len(t, d.Suppressed, 1)
		} else {
			assert.Len(t, kept, 1, "an engine-less reservation must not hide a rec")
			require.Len(t, d.Flagged, 1)
			assert.Equal(t, statusPossiblyCovered, d.Flagged[0].Status)
		}
	}
}

type ecAPI struct {
	eclib.API
	engine string
}

func (e ecAPI) DescribeReservedCacheNodes(_ context.Context, _ *ecsdk.DescribeReservedCacheNodesInput, _ ...func(*ecsdk.Options)) (*ecsdk.DescribeReservedCacheNodesOutput, error) {
	return &ecsdk.DescribeReservedCacheNodesOutput{ReservedCacheNodes: []ectypes.ReservedCacheNode{{
		ReservedCacheNodeId: aws.String("n1"), CacheNodeType: aws.String("cache.r6g.large"),
		ProductDescription: aws.String(e.engine), CacheNodeCount: aws.Int32(2), State: aws.String("active"),
		StartTime: aws.Time(dedupeRecent), Duration: aws.Int32(31536000),
	}}}, nil
}

type osAPI struct{ oslib.API }

func (osAPI) DescribeReservedInstances(_ context.Context, _ *ossdk.DescribeReservedInstancesInput, _ ...func(*ossdk.Options)) (*ossdk.DescribeReservedInstancesOutput, error) {
	return &ossdk.DescribeReservedInstancesOutput{ReservedInstances: []ostypes.ReservedInstance{{
		ReservedInstanceId: aws.String("o1"), InstanceType: ostypes.OpenSearchPartitionInstanceType("r6g.large.search"),
		InstanceCount: 3, State: aws.String("active"), StartTime: aws.Time(dedupeRecent), Duration: 31536000,
	}}}, nil
}

type rsAPI struct{ rslib.API }

func (rsAPI) DescribeReservedNodes(_ context.Context, _ *rssdk.DescribeReservedNodesInput, _ ...func(*rssdk.Options)) (*rssdk.DescribeReservedNodesOutput, error) {
	return &rssdk.DescribeReservedNodesOutput{ReservedNodes: []rstypes.ReservedNode{{
		ReservedNodeId: aws.String("r1"), NodeType: aws.String("ra3.xlplus"), NodeCount: aws.Int32(3),
		State: aws.String("active"), StartTime: aws.Time(dedupeRecent), Duration: aws.Int32(31536000),
	}}}, nil
}

// Each "exact" service must actually suppress end to end through its real
// listing: a key mismatch (an engine on one side only) would make the feature
// silently do nothing.
func TestDedupeEverySuppressingServiceSuppressesThroughItsRealListing(t *testing.T) {
	osc := oslib.NewClient(aws.Config{Region: "us-east-1"})
	osc.SetOpenSearchAPI(osAPI{})
	rsc := rslib.NewClient(aws.Config{Region: "us-east-1"})
	rsc.SetRedshiftAPI(rsAPI{})
	p := awsProvider(map[common.ServiceType]provider.ServiceClient{
		common.ServiceOpenSearch: osc, common.ServiceRedshift: rsc, common.ServiceRDS: rdsClient(),
	})
	recs := []common.Recommendation{
		{Provider: common.ProviderAWS, Service: common.ServiceOpenSearch, Account: dedupeCaller, Region: "us-east-1",
			ResourceType: "r6g.large.search", Count: 3, Term: "1yr", PaymentOption: "no-upfront", Details: &common.SearchDetails{InstanceType: "r6g.large.search"}},
		{Provider: common.ProviderAWS, Service: common.ServiceRedshift, Account: dedupeCaller, Region: "us-east-1",
			ResourceType: "ra3.xlplus", Count: 3, Term: "1yr", PaymentOption: "no-upfront"},
		rdsRec(2, "1yr", "no-upfront", dedupeCaller),
	}
	kept, d, err := runDedupe(t, p, recs)
	require.NoError(t, err)
	assert.Empty(t, kept)
	assert.Len(t, d.Suppressed, 3)
}

func TestDedupeMemoryDBIsFlagOnly(t *testing.T) {
	lc := &listingClient{commitments: []common.Commitment{{
		Provider: common.ProviderAWS, Service: common.ServiceMemoryDB, Region: "us-east-1", ResourceType: "db.r6g.large",
		Engine: "redis", Count: 2, State: common.CommitmentStateActive, StartDate: dedupeRecent,
	}}}
	p := awsProvider(map[common.ServiceType]provider.ServiceClient{common.ServiceMemoryDB: lc})
	rec := common.Recommendation{Provider: common.ProviderAWS, Service: common.ServiceMemoryDB, Account: dedupeCaller,
		Region: "us-east-1", ResourceType: "db.r6g.large", Count: 2, Term: "1yr", PaymentOption: "no-upfront",
		Details: &common.CacheDetails{NodeType: "db.r6g.large", Engine: "redis"}}
	kept, d, err := runDedupe(t, p, []common.Recommendation{rec})
	require.NoError(t, err)
	assert.Len(t, kept, 1)
	assert.Empty(t, d.Suppressed)
	require.Len(t, d.Flagged, 1)
	assert.Equal(t, statusPossiblyCovered, d.Flagged[0].Status)
}

// GCP is flag-only, and the provider's own family filter must still be
// reached through the memoizing client.
func TestDedupeGCPForwardsTheFamilyHookAndOnlyFlags(t *testing.T) {
	ctx := context.Background()
	client, err := computeengine.NewClient(ctx, "test-project", "us-central1")
	require.NoError(t, err)
	client.SetCommitmentsService(&fakeGCPCommitmentsService{commitments: []*computepb.Commitment{{
		Name:           proto.String("existing-n2-cud"),
		Type:           proto.String(computepb.Commitment_GENERAL_PURPOSE_N2.String()),
		Status:         proto.String(computepb.Commitment_ACTIVE.String()),
		StartTimestamp: proto.String(dedupeRecent.UTC().Format(time.RFC3339)),
		Resources: []*computepb.ResourceCommitment{
			{Type: proto.String(computepb.ResourceCommitment_VCPU.String()), Amount: proto.Int64(2)},
			{Type: proto.String(computepb.ResourceCommitment_MEMORY.String()), Amount: proto.Int64(8192)},
		},
	}}})
	rec, _, _, _, err := gcpComputeEngineRecommendationFromArgs(gcpComputeEngineCUDPurchaseArgs{
		Region: "us-central1", MachineType: "n2-standard-64", VCPUCount: 64, MemoryGB: 256, TermYears: 1,
	})
	require.NoError(t, err)
	rec.Account = "test-project"

	p := &svcProvider{fakeProvider: fakeProvider{name: "gcp"}, byService: map[common.ServiceType]provider.ServiceClient{common.ServiceCompute: client}}
	kept, d, err := runDedupe(t, p, []common.Recommendation{rec})
	require.NoError(t, err)
	assert.Len(t, kept, 1, "2 vCPU bought must not hide a 64 vCPU recommendation")
	require.Len(t, d.Flagged, 1)
	assert.Equal(t, statusPossiblyCovered, d.Flagged[0].Status)
	assert.Contains(t, d.Flagged[0].Reason, "recent CUD")
}

func TestSearchResultCarriesTheDedupeBlock(t *testing.T) {
	client := &fakeRecommendationsClient{recs: []common.Recommendation{rdsRec(2, "1yr", "all-upfront", dedupeCaller)}}
	fp := &fakeProvider{name: "aws", services: []common.ServiceType{common.ServiceRDS}, recClient: client,
		svc: rdsClient(), accounts: []common.Account{{ID: dedupeCaller, IsDefault: true}}}
	_, result, err := newTestSearchTool(fp).handle(context.Background(), nil, searchRecommendationsArgs{
		Provider: "aws", Service: "rds", Region: "us-east-1", TermYears: 1, PaymentOption: "all-upfront",
	})
	require.NoError(t, err)
	assert.Equal(t, 0, result.Count)
	raw, err := json.Marshal(result)
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(raw, &wire))
	d, ok := wire["dedupe"].(map[string]any)
	require.True(t, ok, "%s", raw)
	assert.Len(t, d["suppressed"], 1, "a suppressed rec is listed, never silently dropped")
}

// Azure's reservation key ignores scope and term, so it is flag-only: a
// commitment that would cover the rec must produce a flag, never a removal.
func TestDedupeAzureIsFlagOnlyNeverSuppressed(t *testing.T) {
	lc := &listingClient{commitments: []common.Commitment{{
		Provider: common.ProviderAzure, Service: common.ServiceCompute, Region: "eastus", ResourceType: "Standard_D2s_v3",
		Count: 4, State: common.CommitmentStateActive, StartDate: dedupeRecent,
	}}}
	p := &svcProvider{fakeProvider: fakeProvider{name: "azure"}, byService: map[common.ServiceType]provider.ServiceClient{common.ServiceCompute: lc}}
	rec := common.Recommendation{
		Provider: common.ProviderAzure, Service: common.ServiceCompute, Region: "eastus", ResourceType: "Standard_D2s_v3",
		Count: 4, Term: "1yr", PaymentOption: "upfront",
	}
	kept, d, err := runDedupe(t, p, []common.Recommendation{rec})
	require.NoError(t, err)
	assert.Equal(t, []common.Recommendation{rec}, kept, "an Azure rec covered by a recent reservation must stay visible")
	assert.Empty(t, d.Suppressed)
	require.Len(t, d.Flagged, 1)
	assert.Equal(t, statusPossiblyCovered, d.Flagged[0].Status)
	assert.Equal(t, 1, lc.listCalls)
}

// The error must name the permission that is actually missing.
func TestDedupeAccountLookupFailureNamesTheRightPermission(t *testing.T) {
	cases := map[string]struct{ err, want, notWant string }{
		"sts":           {"failed to get current account: AccessDenied", "sts:GetCallerIdentity", "rds:DescribeReservedDBInstances"},
		"organizations": {"organizations: list accounts: AccessDenied", "organizations:ListAccounts", "rds:DescribeReservedDBInstances"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p := awsProvider(map[common.ServiceType]provider.ServiceClient{common.ServiceRDS: &listingClient{}})
			p.acctErr = errors.New(c.err)
			kept, _, err := runDedupe(t, p, []common.Recommendation{rdsRec(2, "1yr", "all-upfront", dedupeCaller)})
			require.Error(t, err)
			assert.Nil(t, kept)
			assert.Contains(t, err.Error(), c.want)
			assert.NotContains(t, err.Error(), c.notWant)
			assert.Contains(t, err.Error(), "AccessDenied")
		})
	}
}

func TestDedupeServiceClientFailureClaimsNoPermission(t *testing.T) {
	p := awsProvider(nil)
	p.byService = map[common.ServiceType]provider.ServiceClient{}
	_, _, err := runDedupe(t, p, []common.Recommendation{rdsRec(2, "1yr", "all-upfront", dedupeCaller)})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "the credentials need")
}
