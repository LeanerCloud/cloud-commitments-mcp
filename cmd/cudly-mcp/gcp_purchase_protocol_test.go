package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/googleapis/gax-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/cloudbilling/v1"
	"google.golang.org/api/iterator"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
	gcpprovider "github.com/LeanerCloud/cloud-commitments-go/providers/gcp"
	"github.com/LeanerCloud/cloud-commitments-go/providers/gcp/services/computeengine"

	cudlymcp "github.com/LeanerCloud/cloud-commitments-mcp"
	"github.com/LeanerCloud/cloud-commitments-mcp/tools"

	gosdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeComputeEngineCommitmentsService implements computeengine.CommitmentsService
// (the exported library seam for tests; the library's own MockCommitmentsService
// is test-only) so the library's real computeengine client can complete a
// purchase without GCP credentials or network.
type fakeComputeEngineCommitmentsService struct {
	insertCalls atomic.Int32
}

func (f *fakeComputeEngineCommitmentsService) List(_ context.Context, _ *computepb.ListRegionCommitmentsRequest) computeengine.CommitmentsIterator {
	return &fakeComputeEngineCommitmentsIterator{}
}

func (f *fakeComputeEngineCommitmentsService) Insert(_ context.Context, _ *computepb.InsertRegionCommitmentRequest) (computeengine.CommitmentsOperation, error) {
	f.insertCalls.Add(1)
	return &fakeComputeEngineCommitmentsOperation{}, nil
}

func (f *fakeComputeEngineCommitmentsService) Get(_ context.Context, _ *computepb.GetRegionCommitmentRequest) (*computepb.Commitment, error) {
	return nil, errors.New("fakeComputeEngineCommitmentsService: unexpected Get call")
}

func (f *fakeComputeEngineCommitmentsService) Close() error { return nil }

type fakeComputeEngineCommitmentsIterator struct{}

func (it *fakeComputeEngineCommitmentsIterator) Next() (*computepb.Commitment, error) {
	return nil, iterator.Done
}

type fakeComputeEngineCommitmentsOperation struct{}

func (o *fakeComputeEngineCommitmentsOperation) Wait(_ context.Context, _ ...gax.CallOption) error {
	return nil
}

// fakeBillingService serves real-shaped N2 catalog SKUs for us-central1
// (rates copied from the library's pricing_test.go n2AmericasSKUs, plus a
// decoy family) so the real pricing path runs without network.
type fakeBillingService struct{}

func (fakeBillingService) ListSKUs(_ string) (*cloudbilling.ListSkusResponse, error) {
	americas := []string{"us-central1", "us-east1", "us-west1"}
	sku := func(id, desc, group, usage, unit string, nanos int64) *cloudbilling.Sku {
		return &cloudbilling.Sku{
			SkuId: id, Description: desc,
			Category:       &cloudbilling.Category{ResourceFamily: "Compute", ResourceGroup: group, UsageType: usage},
			ServiceRegions: americas,
			PricingInfo: []*cloudbilling.PricingInfo{{PricingExpression: &cloudbilling.PricingExpression{
				UsageUnit:   unit,
				TieredRates: []*cloudbilling.TierRate{{UnitPrice: &cloudbilling.Money{CurrencyCode: "USD", Nanos: nanos}}},
			}}},
		}
	}
	return &cloudbilling.ListSkusResponse{Skus: []*cloudbilling.Sku{
		sku("n2-od-core", "N2 Instance Core running in Americas", "CPU", "OnDemand", "h", 31611000),
		sku("n2-od-ram", "N2 Instance Ram running in Americas", "RAM", "OnDemand", "GiBy.h", 4237000),
		sku("n2-c1-core", "Commitment v1: N2 Cpu in Americas for 1 Year", "CPU", "Commit1Yr", "h", 19915000),
		sku("n2-c1-ram", "Commitment v1: N2 Ram in Americas for 1 Year", "RAM", "Commit1Yr", "GiBy.h", 2669000),
		sku("n2-c3-core", "Commitment v1: N2 Cpu in Americas for 3 Year", "CPU", "Commit3Yr", "h", 14225000),
		sku("n2-c3-ram", "Commitment v1: N2 Ram in Americas for 3 Year", "RAM", "Commit3Yr", "GiBy.h", 1907000),
		sku("n2d-od-core", "N2D AMD Instance Core running in Americas", "CPU", "OnDemand", "h", 27502000),
	}}, nil
}

// fakeGCPProvider implements provider.Provider just far enough to hand the
// purchase flow the library's real computeengine client (with the fake
// commitments service injected); every other method fails loudly so a
// regression that reaches further into the provider surface is visible.
type fakeGCPProvider struct {
	cfg                *provider.ProviderConfig
	commitmentsService *fakeComputeEngineCommitmentsService
}

func (p *fakeGCPProvider) Name() string        { return "gcp" }
func (p *fakeGCPProvider) DisplayName() string { return "Google Cloud Platform" }
func (p *fakeGCPProvider) IsConfigured() bool  { return true }

func (p *fakeGCPProvider) GetCredentials() (provider.Credentials, error) {
	return nil, errors.New("fakeGCPProvider: GetCredentials not implemented")
}

func (p *fakeGCPProvider) ValidateCredentials(_ context.Context) error {
	return errors.New("fakeGCPProvider: ValidateCredentials not implemented")
}

func (p *fakeGCPProvider) GetAccounts(_ context.Context) ([]common.Account, error) {
	return nil, errors.New("fakeGCPProvider: GetAccounts not implemented")
}

func (p *fakeGCPProvider) GetRegions(_ context.Context) ([]common.Region, error) {
	return nil, errors.New("fakeGCPProvider: GetRegions not implemented")
}

func (p *fakeGCPProvider) GetDefaultRegion() string { return "us-central1" }

func (p *fakeGCPProvider) GetSupportedServices() []common.ServiceType {
	return []common.ServiceType{common.ServiceCompute}
}

func (p *fakeGCPProvider) GetServiceClient(ctx context.Context, service common.ServiceType, region string) (provider.ServiceClient, error) {
	if service != common.ServiceCompute {
		return nil, errors.New("fakeGCPProvider: only compute is implemented")
	}
	client, err := computeengine.NewClient(ctx, p.cfg.GCPProjectID, region)
	if err != nil {
		return nil, err
	}
	client.SetCommitmentsService(p.commitmentsService)
	client.SetBillingService(fakeBillingService{})
	return client, nil
}

func (p *fakeGCPProvider) GetRecommendationsClient(_ context.Context) (provider.RecommendationsClient, error) {
	return nil, errors.New("fakeGCPProvider: GetRecommendationsClient not implemented")
}

// callGCPCUDPurchase drives a real purchase over the MCP transport against the
// library's real computeengine client with fake commitments and billing
// services, under the given USD cap, and returns the call result and the
// number of Insert calls. Cloud-priced fixtures only; nothing leaves the process.
func callGCPCUDPurchase(t *testing.T, usdCap string, termYears int) (*gosdk.CallToolResult, int32) {
	t.Helper()
	t.Setenv(tools.EnvEnableRealPurchases, "1")
	t.Setenv(tools.EnvMaxCount, "1000")
	t.Setenv(tools.EnvMaxHourlyCommitment, "1000")
	t.Setenv(tools.EnvMaxMemoryGB, "1000")
	t.Setenv(tools.EnvMaxCommitmentUSD, usdCap)
	t.Setenv(tools.EnvBudgetUSD, "1000000000000")
	service := &fakeComputeEngineCommitmentsService{}

	provider.GetRegistry().Unregister("gcp")
	err := provider.RegisterProvider("gcp", func(cfg *provider.ProviderConfig) (provider.Provider, error) {
		return &fakeGCPProvider{cfg: cfg, commitmentsService: service}, nil
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		provider.GetRegistry().Unregister("gcp")
		if regErr := provider.RegisterProvider("gcp", func(cfg *provider.ProviderConfig) (provider.Provider, error) {
			return gcpprovider.NewProvider(cfg)
		}); regErr != nil {
			t.Errorf("restore real gcp provider factory: %v", regErr)
		}
	})

	server, err := cudlymcp.NewServer("test-gcp-purchase")
	require.NoError(t, err)
	clientTransport, serverTransport := gosdk.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	go func() {
		_ = server.Run(ctx, serverTransport)
	}()

	client := gosdk.NewClient(&gosdk.Implementation{Name: "test-client"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })

	// Assert registration through the protocol, not just the registry.
	toolsResult, err := session.ListTools(ctx, nil)
	require.NoError(t, err)
	found := false
	for _, tool := range toolsResult.Tools {
		if tool.Name == "cudly_gcp_computeengine_cud_purchase" {
			found = true
			break
		}
	}
	require.True(t, found, "cudly_gcp_computeengine_cud_purchase must be registered on the server")

	result, err := session.CallTool(ctx, &gosdk.CallToolParams{
		Name: "cudly_gcp_computeengine_cud_purchase",
		Arguments: map[string]any{
			"region":         "us-central1",
			"machine_type":   "n2-standard-4",
			"vcpu_count":     4,
			"memory_gb":      16,
			"term_years":     termYears,
			"gcp_project_id": "test-project",
			"dry_run":        false,
			"confirm":        true,
		},
	})
	require.NoError(t, err, "CallTool itself must not return a transport-level error")
	return result, service.insertCalls.Load()
}

// TestGCPComputeEngineCUDPurchaseStructuredCost is the protocol-level
// acceptance guard for issue #35: a REAL (dry_run=false, confirm=true,
// operator-authorized) cudly_gcp_computeengine_cud_purchase must succeed
// end-to-end and report an explicit zero cost in its structured output. The
// USD cap is priced by the library's real catalog path over fake SKUs.
func TestGCPComputeEngineCUDPurchaseStructuredCost(t *testing.T) {
	result, inserts := callGCPCUDPurchase(t, "1000000000", 1)
	require.False(t, result.IsError, "a successful fake-backed purchase must not surface as a tool error: %+v", result.Content)
	require.Equal(t, int32(1), inserts)

	require.NotNil(t, result.StructuredContent, "structured output must be present")
	encoded, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	var structured map[string]any
	require.NoError(t, json.Unmarshal(encoded, &structured))

	assert.Equal(t, true, structured["success"], "purchase must report success, got %s", encoded)
	assert.Equal(t, false, structured["dry_run"], "this was a real purchase, got %s", encoded)
	// PurchaseResponse.Cost is a *float64 with omitempty: a known zero upfront
	// charge must survive as an explicit "cost":0.
	cost, present := structured["cost"]
	require.True(t, present, "structured output must carry an explicit cost field, got %s", encoded)
	assert.Equal(t, 0.0, cost, "a Compute Engine CUD has no upfront charge, got %s", encoded)
}

// The GCP total is the whole commitment for 4 vCPU / 16 GiB, not multiplied by
// count again (x4 would be about 4287.6 USD and refuse the first case).
func TestGCPComputeEngineCUDPurchaseUSDCapBoundary(t *testing.T) {
	oneYear := (4*0.019915 + 16*0.002669) * 8760
	threeYear := (4*0.014225 + 16*0.001907) * 8760 * 3
	assert.InDelta(t, 1071.90864, oneYear, 1e-6)
	assert.InDelta(t, 2297.18736, threeYear, 1e-6)

	for _, tc := range []struct {
		name    string
		cap     string
		term    int
		allowed bool
	}{
		{"1yr cap above total", "1100", 1, true},
		{"1yr cap below total", "1000", 1, false},
		{"3yr cap above total", "2300", 3, true},
		{"3yr cap below total", "2297", 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, inserts := callGCPCUDPurchase(t, tc.cap, tc.term)
			if tc.allowed {
				require.False(t, result.IsError, "%+v", result.Content)
				require.Equal(t, int32(1), inserts)
				return
			}
			require.True(t, result.IsError, "a purchase over the cap must be refused")
			text, ok := result.Content[0].(*gosdk.TextContent)
			require.True(t, ok)
			assert.Contains(t, text.Text, tools.EnvMaxCommitmentUSD)
			require.Equal(t, int32(0), inserts)
		})
	}
}
