package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
)

const gcpComputeEngineCUDPurchaseName = "cudly_gcp_computeengine_cud_purchase"

const gcpComputeEngineCUDPurchaseTitle = "Purchase a GCP Compute Engine Committed Use Discount"

var gcpComputeEngineCUDPurchaseAnnotations = purchaseAnnotations(gcpComputeEngineCUDPurchaseTitle)

const gcpComputeEngineCUDPurchaseDescription = "Purchase a GCP Compute Engine Committed Use Discount (CUD). THIS " +
	"SPENDS REAL MONEY when dry_run=false and confirm=true. Always call with dry_run=true first (the default) " +
	"to validate your parameters before committing; a dry_run response never contacts GCP and never spends " +
	"money. A CUD commits vCPUs and memory directly (not an instance count): vcpu_count is the number of vCPUs " +
	"and memory_gb is the amount of memory to commit. There is no payment_option parameter: GCP Compute Engine " +
	"CUDs are billed monthly over the term, with no upfront option."

// gcpPaymentOption is the only payment schedule GCP Compute Engine CUDs
// offer, and the only value config.ValidPaymentOptionsByProvider["gcp"]
// accepts. Named here rather than written as a bare literal at the one use
// site so the constraint is stated once, next to its rationale.
const gcpPaymentOption = "monthly"

// gcpProjectEnvVar names the project a real CUD purchase buys for when
// gcp_project_id is omitted. It is CUDly-namespaced on purpose: GOOGLE_CLOUD_PROJECT
// names the project a process runs in, and a hosting runtime may set it
// implicitly. Only this tool reads it; providers/gcp still sees the value as
// an explicit config.GCPProjectID, so the idempotency scope and the purchase
// target always name the same project.
const gcpProjectEnvVar = "CUDLY_GCP_PROJECT_ID"

// gcpComputeEngineCUDPurchaseArgs is the input schema for
// cudly_gcp_computeengine_cud_purchase. memory_gb is required: unlike AWS/
// Azure, providers/gcp/services/computeengine/client.go's buildInsertRequest
// reads Recommendation.Details as a *value* common.ComputeDetails (not a
// pointer, unlike every AWS Details assertion) and hard-errors when
// MemoryGB is absent or <= 0 rather than guessing a vCPU:memory ratio.
type gcpComputeEngineCUDPurchaseArgs struct {
	DryRun           *bool   `json:"dry_run,omitempty" jsonschema:"preview only, no purchase; defaults to true"`
	Confirm          *bool   `json:"confirm,omitempty" jsonschema:"required (with dry_run=false) to execute a real purchase; defaults to false"`
	Region           string  `json:"region" jsonschema:"GCP region, e.g. us-central1"`
	MachineType      string  `json:"machine_type" jsonschema:"GCP machine type family for the commitment, e.g. n2-standard-4"`
	GCPProjectID     string  `json:"gcp_project_id,omitempty" jsonschema:"GCP project ID to buy for. Optional for a dry_run preview; a real purchase (dry_run=false, confirm=true) needs it here or CUDLY_GCP_PROJECT_ID set in the server environment, and is refused otherwise"`
	IdempotencyNonce string  `json:"idempotency_nonce,omitempty" jsonschema:"optional; set to a fresh value to authorize a purchase that is otherwise identical to a previous one (e.g. buy 3 more RIs with the same parameters); leave empty (the default) so retries with identical parameters dedupe and never double-buy"`
	VCPUCount        int     `json:"vcpu_count" jsonschema:"number of vCPUs to commit, must be > 0; real purchases are capped by the operator (CUDLY_MCP_MAX_COUNT)"`
	MemoryGB         float64 `json:"memory_gb" jsonschema:"amount of memory (GB) to commit, must be > 0; real purchases are capped by the operator (CUDLY_MCP_MAX_MEMORY_GB)"`
	TermYears        int     `json:"term_years" jsonschema:"commitment length in years"`
}

type gcpComputeEngineCUDPurchaseTool struct {
	createProvider func(name string, cfg *provider.Config) (provider.Provider, error)
}

// NewGCPComputeEngineCUDPurchaseTool builds the cudly_gcp_computeengine_cud_purchase tool.
func NewGCPComputeEngineCUDPurchaseTool() Registration {
	return &gcpComputeEngineCUDPurchaseTool{createProvider: provider.CreateProvider}
}

func (t *gcpComputeEngineCUDPurchaseTool) Descriptor() Descriptor {
	return Descriptor{
		Name:                gcpComputeEngineCUDPurchaseName,
		Provider:            "gcp",
		Product:             "computeengine",
		Action:              "cud_purchase",
		Description:         gcpComputeEngineCUDPurchaseDescription,
		Annotations:         gcpComputeEngineCUDPurchaseAnnotations,
		RealPurchaseEnabled: true,
		ExamplePrompts: []string{
			"Preview a 3-year CUD for 8 vCPUs and 32 GB memory in us-central1",
			"Buy a 1-year Compute Engine CUD for real: 4 vCPUs, 16 GB memory",
		},
	}
}

func (t *gcpComputeEngineCUDPurchaseTool) Register(s *mcp.Server) error {
	schema, err := BuildInputSchema[gcpComputeEngineCUDPurchaseArgs](map[string]FieldOverride{
		"term_years": {Enum: []any{int(TermOneYear), int(TermThreeYear)}},
		"dry_run":    {Default: true},
		"confirm":    {Default: false},
	})
	if err != nil {
		return err
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        gcpComputeEngineCUDPurchaseName,
		Description: gcpComputeEngineCUDPurchaseDescription,
		Annotations: gcpComputeEngineCUDPurchaseAnnotations,
		InputSchema: schema,
	}, t.handle)
	return nil
}

func (t *gcpComputeEngineCUDPurchaseTool) handle(ctx context.Context, _ *mcp.CallToolRequest, args gcpComputeEngineCUDPurchaseArgs) (*mcp.CallToolResult, PurchaseResponse, error) {
	rec, region, dryRun, confirm, err := gcpComputeEngineRecommendationFromArgs(args)
	if err != nil {
		return nil, PurchaseResponse{}, err
	}

	resp, err := ExecutePurchase(ctx, PurchaseRequest{
		Region:          region,
		Recommendation:  rec,
		DryRun:          dryRun,
		Confirm:         confirm,
		ResolveClient:   t.resolveClient(args, region),
		Nonce:           args.IdempotencyNonce,
		CredentialScope: requestCredentialScope(args.GCPProjectID, dryRun, gcpProjectEnvVar),
	})
	if err != nil {
		return nil, PurchaseResponse{}, err
	}
	return nil, *resp, nil
}

// gcpComputeEngineRecommendationFromArgs validates args and builds the
// common.Recommendation to purchase, the effective region (trimmed of any
// surrounding whitespace), and the effective dry_run/confirm booleans.
// Details is set as a value (common.ComputeDetails{}), not a pointer, to
// match the value type assertion in
// providers/gcp/services/computeengine/client.go's memoryMBFromDetails.
func gcpComputeEngineRecommendationFromArgs(args gcpComputeEngineCUDPurchaseArgs) (rec common.Recommendation, region string, dryRun, confirm bool, err error) {
	region, err = requireNonBlank("region", args.Region)
	if err != nil {
		return common.Recommendation{}, "", false, false, err
	}
	machineType, err := requireNonBlank("machine_type", args.MachineType)
	if err != nil {
		return common.Recommendation{}, "", false, false, err
	}
	if args.VCPUCount <= 0 {
		return common.Recommendation{}, "", false, false, fmt.Errorf("vcpu_count must be > 0, got %d", args.VCPUCount)
	}
	if args.MemoryGB <= 0 {
		return common.Recommendation{}, "", false, false, fmt.Errorf("memory_gb must be > 0, got %v", args.MemoryGB)
	}
	term, err := ValidateTermYears(args.TermYears)
	if err != nil {
		return common.Recommendation{}, "", false, false, err
	}

	rec = common.Recommendation{
		Provider:       common.ProviderGCP,
		Service:        common.ServiceCompute,
		Region:         region,
		ResourceType:   machineType,
		Count:          args.VCPUCount,
		CommitmentType: common.CommitmentCUD,
		Term:           term.RecommendationTerm(),
		// GCP Compute Engine CUDs have exactly one billing schedule: monthly
		// over the term, with no upfront option. There is therefore no
		// payment_option argument on this tool, but PaymentOption is still
		// set explicitly rather than left "", because "" is not neutral
		// downstream: providers/gcp/services/computeengine/client.go's
		// offering-details switch falls through to `default: upfrontCost =
		// totalCost`, reporting the whole commitment as an upfront charge
		// for an empty payment option. gcpPaymentOption is the single value
		// config.ValidPaymentOptionsByProvider["gcp"] recognizes.
		PaymentOption: gcpPaymentOption,
		Details: common.ComputeDetails{
			InstanceType: machineType,
			MemoryGB:     args.MemoryGB,
		},
	}

	dryRun, confirm = ResolveDryRunConfirm(args.DryRun, args.Confirm)
	return rec, region, dryRun, confirm, nil
}

// resolveClient returns the ResolveClientFunc that ExecutePurchase invokes
// only for a real purchase. region is the effective, already-validated-and-
// trimmed region returned by gcpComputeEngineRecommendationFromArgs -- not
// args.Region -- so a real purchase never resolves the provider/service
// client against a raw, un-trimmed value.
func (t *gcpComputeEngineCUDPurchaseTool) resolveClient(args gcpComputeEngineCUDPurchaseArgs, region string) ResolveClientFunc {
	return func(ctx context.Context) (provider.ServiceClient, error) {
		cfg := &provider.Config{Name: string(common.ProviderGCP), GCPProjectID: CredentialScope(args.GCPProjectID, gcpProjectEnvVar), Region: region}
		prov, err := t.createProvider(string(common.ProviderGCP), cfg)
		if err != nil {
			return nil, err
		}
		return prov.GetServiceClient(ctx, common.ServiceCompute, region)
	}
}
