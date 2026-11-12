package tools

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
)

func validGCPCUDArgs() gcpComputeEngineCUDPurchaseArgs {
	return gcpComputeEngineCUDPurchaseArgs{
		GCPProjectID: "test-project",
		Region:       "us-central1",
		MachineType:  "n2-standard-4",
		VCPUCount:    4,
		MemoryGB:     16,
		TermYears:    1,
	}
}

func TestGCPComputeEngineRecommendationFromArgs(t *testing.T) {
	t.Parallel()
	rec, region, dryRun, confirm, err := gcpComputeEngineRecommendationFromArgs(validGCPCUDArgs())
	require.NoError(t, err)
	assert.Equal(t, "us-central1", region)
	assert.True(t, dryRun)
	assert.False(t, confirm)
	assert.Equal(t, common.ProviderGCP, rec.Provider)
	assert.Equal(t, common.ServiceCompute, rec.Service)
	assert.Equal(t, common.CommitmentCUD, rec.CommitmentType)
	assert.Equal(t, "n2-standard-4", rec.ResourceType)
	assert.Equal(t, 4, rec.Count)
	assert.Equal(t, "1yr", rec.Term)

	// Details MUST be a value common.ComputeDetails, not a pointer:
	// providers/gcp/services/computeengine/client.go's memoryMBFromDetails
	// type-asserts rec.Details.(common.ComputeDetails), unlike every AWS
	// Details assertion which expects a pointer.
	details, ok := rec.Details.(common.ComputeDetails)
	require.True(t, ok, "Details must be a value common.ComputeDetails, not *common.ComputeDetails")
	assert.InDelta(t, 16.0, details.MemoryGB, 0.001)
}

func TestGCPComputeEngineRecommendationFromArgsInvalid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*gcpComputeEngineCUDPurchaseArgs)
		errSub string
	}{
		{"missing region", func(a *gcpComputeEngineCUDPurchaseArgs) { a.Region = "" }, "region is required"},
		{"whitespace-only region", func(a *gcpComputeEngineCUDPurchaseArgs) { a.Region = "   " }, "region is required"},
		{"missing machine_type", func(a *gcpComputeEngineCUDPurchaseArgs) { a.MachineType = "" }, "machine_type is required"},
		{"whitespace-only machine_type", func(a *gcpComputeEngineCUDPurchaseArgs) { a.MachineType = "\t " }, "machine_type is required"},
		{"zero vcpu_count", func(a *gcpComputeEngineCUDPurchaseArgs) { a.VCPUCount = 0 }, "vcpu_count must be"},
		{"zero memory_gb", func(a *gcpComputeEngineCUDPurchaseArgs) { a.MemoryGB = 0 }, "memory_gb must be"},
		{"negative memory_gb", func(a *gcpComputeEngineCUDPurchaseArgs) { a.MemoryGB = -1 }, "memory_gb must be"},
		{"invalid term", func(a *gcpComputeEngineCUDPurchaseArgs) { a.TermYears = 2 }, "invalid term_years"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := validGCPCUDArgs()
			tc.mutate(&args)
			_, _, _, _, err := gcpComputeEngineRecommendationFromArgs(args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.errSub)
		})
	}
}

// TestGCPComputeEngineRecommendationFromArgsTrimsSurroundingWhitespace is the
// regression guard for the CodeRabbit finding: requireNonBlank rejected an
// all-whitespace value but let a value with surrounding whitespace (e.g.
// " us-central1 ") pass through unchanged into rec.Region/rec.ResourceType and
// the returned region (which resolveClient uses for ProviderConfig.Region and
// GetServiceClient).
func TestGCPComputeEngineRecommendationFromArgsTrimsSurroundingWhitespace(t *testing.T) {
	t.Parallel()
	args := validGCPCUDArgs()
	args.Region = " us-central1 "
	args.MachineType = " n2-standard-4 "

	rec, region, _, _, err := gcpComputeEngineRecommendationFromArgs(args)
	require.NoError(t, err)
	assert.Equal(t, "us-central1", region, "returned region must be trimmed")
	assert.Equal(t, "us-central1", rec.Region, "rec.Region must be trimmed")
	assert.Equal(t, "n2-standard-4", rec.ResourceType, "rec.ResourceType must be trimmed")
	details, ok := rec.Details.(common.ComputeDetails)
	require.True(t, ok)
	assert.Equal(t, "n2-standard-4", details.InstanceType, "Details.InstanceType must be trimmed")
}

func TestGCPComputeEngineCUDPurchaseHandleConfirmFalseRefuses(t *testing.T) {
	t.Parallel()
	resolveCalled := false
	tool := &gcpComputeEngineCUDPurchaseTool{
		createProvider: func(_ string, _ *provider.Config) (provider.Provider, error) {
			resolveCalled = true
			return nil, nil
		},
	}
	args := validGCPCUDArgs()
	args.DryRun = boolPtr(false)
	args.Confirm = boolPtr(false)

	_, _, err := tool.handle(context.Background(), nil, args)
	require.Error(t, err)
	assert.False(t, resolveCalled)
	assert.Contains(t, err.Error(), "confirm=true")
}

func TestGCPComputeEngineCUDPurchaseHandleDryRunNeverCallsProvider(t *testing.T) {
	t.Parallel()
	resolveCalled := false
	tool := &gcpComputeEngineCUDPurchaseTool{
		createProvider: func(_ string, _ *provider.Config) (provider.Provider, error) {
			resolveCalled = true
			return nil, nil
		},
	}
	args := validGCPCUDArgs()
	args.Confirm = boolPtr(true)

	_, resp, err := tool.handle(context.Background(), nil, args)
	require.NoError(t, err)
	assert.False(t, resolveCalled)
	assert.True(t, resp.DryRun)
}

func TestGCPComputeEngineCUDPurchaseHandleRealPurchase(t *testing.T) {
	t.Parallel()
	fake := &fakeServiceClient{purchaseResult: common.PurchaseResult{Success: true, CommitmentID: "cud-1"}}
	var gotService common.ServiceType
	tool := &gcpComputeEngineCUDPurchaseTool{
		createProvider: func(_ string, _ *provider.Config) (provider.Provider, error) {
			return &recordingProvider{
				fakeProvider: &fakeProvider{name: "gcp"},
				client:       fake,
				gotService:   &gotService,
				gotRegion:    new(string),
			}, nil
		},
	}
	args := validGCPCUDArgs()
	args.DryRun = boolPtr(false)
	args.Confirm = boolPtr(true)

	_, resp, err := tool.handle(context.Background(), nil, args)
	require.NoError(t, err)
	assert.True(t, resp.Success)
	assert.Equal(t, common.ServiceCompute, gotService)
	assert.Equal(t, common.PurchaseSourceMCP, fake.lastOpts.Source)
	assert.Equal(t, 1, fake.purchaseCalls)
}

// TestGCPComputeEngineRecommendationSetsMonthlyPaymentOption pins that the
// CUD recommendation carries an explicit payment option. GCP Compute Engine
// CUDs are monthly-only, so the tool exposes no payment_option argument, but
// leaving Recommendation.PaymentOption empty is not neutral: the offering-
// details switch in providers/gcp/services/computeengine/client.go falls
// through to `default: upfrontCost = totalCost`, reporting the entire
// commitment as an upfront charge for an empty payment option. "monthly" is
// also the sole value config.ValidPaymentOptionsByProvider["gcp"] accepts.
func TestGCPComputeEngineRecommendationSetsMonthlyPaymentOption(t *testing.T) {
	t.Parallel()
	rec, _, _, _, err := gcpComputeEngineRecommendationFromArgs(validGCPCUDArgs())
	require.NoError(t, err)
	assert.Equal(t, "monthly", rec.PaymentOption,
		"an empty payment option is read downstream as an upfront charge")
}

// TestGCPComputeEngineCUDResolveClientUsesProjectEnv pins that an omitted
// gcp_project_id falls back to CUDLY_GCP_PROJECT_ID, handed to the provider as
// the explicit project so providers/gcp never reaches getDefaultProject.
// Not parallel: t.Setenv.
func TestGCPComputeEngineCUDResolveClientUsesProjectEnv(t *testing.T) {
	t.Setenv(gcpProjectEnvVar, "proj-env")
	var gotProject string
	tool := &gcpComputeEngineCUDPurchaseTool{
		createProvider: func(_ string, cfg *provider.Config) (provider.Provider, error) {
			gotProject = cfg.GCPProjectID
			return nil, errors.New("stop after capturing config")
		},
	}
	args := validGCPCUDArgs()
	args.GCPProjectID = ""

	_, err := tool.resolveClient(args, "us-central1")(context.Background())
	require.Error(t, err)
	assert.Equal(t, "proj-env", gotProject)
}

// TestGCPComputeEngineCUDAmbientAndExplicitProjectDedupe is the invariant that
// makes the environment fallback safe: naming the project explicitly and
// inheriting it from CUDLY_GCP_PROJECT_ID must yield one idempotency token and
// one provider project, so a retry in either form dedupes. Not parallel: t.Setenv.
func TestGCPComputeEngineCUDAmbientAndExplicitProjectDedupe(t *testing.T) {
	t.Setenv(gcpProjectEnvVar, "proj-a")
	purchase := func(explicit string) (*fakeServiceClient, string) {
		fake := &fakeServiceClient{purchaseResult: common.PurchaseResult{Success: true}}
		var gotProject string
		tool := &gcpComputeEngineCUDPurchaseTool{
			createProvider: func(_ string, cfg *provider.Config) (provider.Provider, error) {
				gotProject = cfg.GCPProjectID
				return &recordingProvider{
					fakeProvider: &fakeProvider{name: "gcp"},
					client:       fake,
					gotService:   new(common.ServiceType),
					gotRegion:    new(string),
				}, nil
			},
		}
		args := validGCPCUDArgs()
		args.GCPProjectID = explicit
		args.DryRun = boolPtr(false)
		args.Confirm = boolPtr(true)
		_, _, err := tool.handle(context.Background(), nil, args)
		require.NoError(t, err)
		return fake, gotProject
	}

	explicit, explicitProject := purchase("proj-a")
	ambient, ambientProject := purchase("")

	assert.Equal(t, "proj-a", explicitProject)
	assert.Equal(t, "proj-a", ambientProject)
	require.NotEmpty(t, explicit.lastOpts.IdempotencyToken)
	assert.Equal(t, explicit.lastOpts.IdempotencyToken, ambient.lastOpts.IdempotencyToken,
		"explicit and ambient resolution of the same project must derive one token")
}
