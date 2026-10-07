package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/googleapis/gax-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
type fakeComputeEngineCommitmentsService struct{}

func (f *fakeComputeEngineCommitmentsService) List(_ context.Context, _ *computepb.ListRegionCommitmentsRequest) computeengine.CommitmentsIterator {
	return &fakeComputeEngineCommitmentsIterator{}
}

func (f *fakeComputeEngineCommitmentsService) Insert(_ context.Context, _ *computepb.InsertRegionCommitmentRequest) (computeengine.CommitmentsOperation, error) {
	return &fakeComputeEngineCommitmentsOperation{}, nil
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

// fakeGCPProvider implements provider.Provider just far enough to hand the
// purchase flow the library's real computeengine client (with the fake
// commitments service injected); every other method fails loudly so a
// regression that reaches further into the provider surface is visible.
type fakeGCPProvider struct {
	cfg *provider.ProviderConfig
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
	client.SetCommitmentsService(&fakeComputeEngineCommitmentsService{})
	return client, nil
}

func (p *fakeGCPProvider) GetRecommendationsClient(_ context.Context) (provider.RecommendationsClient, error) {
	return nil, errors.New("fakeGCPProvider: GetRecommendationsClient not implemented")
}

// TestGCPComputeEngineCUDPurchaseStructuredCost is the protocol-level
// acceptance guard for issue #35: a REAL (dry_run=false, confirm=true,
// operator-authorized) cudly_gcp_computeengine_cud_purchase driven over the
// MCP transport must succeed end-to-end and report an explicit zero cost in
// its structured output. It follows the in-process pattern of
// TestRealPurchasePastProviderRegistration (NewServer + in-memory transports
// + CallTool), but injects a fake gcp provider factory into the global
// registry so the call goes through the library's real computeengine client
// against a fake CommitmentsService instead of GCP.
//
// Deliberately NOT claimed pin-sensitive: on the MCP path
// gcpComputeEngineRecommendationFromArgs never sets CommitmentCost, so old
// and new library pins both yield "cost":0 here. The pin-sensitive cost proof
// lives at the library boundary (tools/gcp_computeengine_cud_safeguards_test.go).
func TestGCPComputeEngineCUDPurchaseStructuredCost(t *testing.T) {
	t.Setenv(tools.EnvEnableRealPurchases, "1")

	provider.GetRegistry().Unregister("gcp")
	err := provider.RegisterProvider("gcp", func(cfg *provider.ProviderConfig) (provider.Provider, error) {
		return &fakeGCPProvider{cfg: cfg}, nil
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
	defer cancel()

	go func() {
		_ = server.Run(ctx, serverTransport)
	}()

	client := gosdk.NewClient(&gosdk.Implementation{Name: "test-client"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	defer session.Close()

	// Assert registration through the protocol, not just the registry: the
	// tool must be listed by the server this test drives.
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
			"term_years":     1,
			"gcp_project_id": "test-project",
			"dry_run":        false,
			"confirm":        true,
		},
	})
	require.NoError(t, err, "CallTool itself must not return a transport-level error")
	require.False(t, result.IsError, "a successful fake-backed purchase must not surface as a tool error: %+v", result.Content)

	require.NotNil(t, result.StructuredContent, "structured output must be present")
	encoded, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	var structured map[string]any
	require.NoError(t, json.Unmarshal(encoded, &structured))

	assert.Equal(t, true, structured["success"], "purchase must report success, got %s", encoded)
	assert.Equal(t, false, structured["dry_run"], "this was a real purchase, got %s", encoded)
	// The cost contract is an EXPLICIT zero: PurchaseResponse.Cost is a
	// *float64 with omitempty, so a nil cost would drop the key entirely; a
	// known zero upfront charge must survive as "cost":0.
	cost, present := structured["cost"]
	require.True(t, present, "structured output must carry an explicit cost field, got %s", encoded)
	assert.Equal(t, 0.0, cost, "a Compute Engine CUD has no upfront charge, got %s", encoded)
}
