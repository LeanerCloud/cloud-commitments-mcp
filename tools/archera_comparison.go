package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/insurance"
)

const (
	archeraComparisonName  = "cudly_archera_comparison"
	archeraComparisonTitle = "Archera commitment plan comparison (read-only)"

	// EnvArcheraAPIKey, EnvArcheraOrgID and EnvArcheraPlanID configure the
	// Archera comparison tool. They are read from the server's environment on
	// every call and never from tool arguments, which the model controls.
	EnvArcheraAPIKey = "ARCHERA_API_KEY" // #nosec G101 -- environment variable name, not a credential
	EnvArcheraOrgID  = "ARCHERA_ORG_ID"
	EnvArcheraPlanID = "ARCHERA_PLAN_ID"

	// archeraMaxResultBytes bounds the marshaled result; beyond it the call
	// fails and the caller narrows with line_item_ids.
	archeraMaxResultBytes = 512 << 10
	archeraMaxLineItemIDs = 200
	archeraMaxPayments    = 3
)

var archeraComparisonAnnotations = readOnlyAnnotations(archeraComparisonTitle, true)

const archeraComparisonDescription = "Read-only comparison of an Archera commitment plan: per line item, the " +
	"current offer and the vendor's alternatives, plus plan-wide hypothetical rollups for contract term and " +
	"payment option combinations. Makes no purchase and enrolls nothing. Disabled until the operator sets " +
	"ARCHERA_API_KEY, ARCHERA_ORG_ID and ARCHERA_PLAN_ID in the server's environment; unconfigured, a call " +
	"returns an error and sends no request. Reads https://api.archera.ai only. Money is exact decimal text " +
	"or null (null means unknown, never zero); monthly figures are 730-hour rates with the Archera premium " +
	"already included; upfront figures are one-time amounts. The API states no currency. Not a bindable " +
	"quote. The response carries both partnership disclosures. No retries: on HTTP 429 the error states " +
	"the vendor's Retry-After, wait that long before calling again."

type archeraComparisonArgs struct {
	LineItemIDs    []string `json:"line_item_ids,omitempty" jsonschema:"restrict the comparison to these Archera line item UUIDs; omit for all selected line items"`
	ContractTerms  []string `json:"contract_terms,omitempty" jsonschema:"target contract terms for the hypothetical rollups; omit for every distinct candidate term"`
	PaymentOptions []string `json:"payment_options,omitempty" jsonschema:"payment options for the hypothetical rollups; omit for no_upfront only"`
}

type archeraComparisonTool struct {
	// httpClient is nil in production, which selects the library's
	// SSRF-hardened client. Tests inject a RoundTripper. The Archera key and
	// client are deliberately never fields: both live in call locals only.
	httpClient *http.Client
}

// NewArcheraComparisonTool builds the cudly_archera_comparison tool.
func NewArcheraComparisonTool() Registration { return &archeraComparisonTool{} }

func (t *archeraComparisonTool) Descriptor() Descriptor {
	return Descriptor{
		Name:        archeraComparisonName,
		Description: archeraComparisonDescription,
		Annotations: archeraComparisonAnnotations,
		Product:     "insurance",
		Action:      "comparison",
		ExamplePrompts: []string{
			"Compare my Archera commitment plan against one-year and three-year alternatives",
			"Show the Archera plan comparison for these line items with all_upfront payment",
		},
	}
}

func (t *archeraComparisonTool) Register(s *mcp.Server) error {
	schema, err := BuildInputSchema[archeraComparisonArgs](nil)
	if err != nil {
		return err
	}
	terms := insurance.ContractTerms()
	termEnum := make([]any, len(terms))
	for i, v := range terms {
		termEnum[i] = v
	}
	payEnum := []any{
		string(insurance.PaymentNoUpfront), string(insurance.PaymentPartialUpfront), string(insurance.PaymentAllUpfront),
	}
	limit := func(field string, max int, enum []any) {
		p := schema.Properties[field]
		n := max
		p.MaxItems = &n
		if enum != nil {
			p.Items.Enum = enum
		}
	}
	limit("line_item_ids", archeraMaxLineItemIDs, nil)
	limit("contract_terms", len(terms), termEnum)
	limit("payment_options", archeraMaxPayments, payEnum)
	mcp.AddTool(s, &mcp.Tool{
		Name:        archeraComparisonName,
		Description: archeraComparisonDescription,
		Annotations: archeraComparisonAnnotations,
		InputSchema: schema,
	}, t.handle)
	return nil
}

// archeraConfig reads the operator's configuration from the environment at
// call time. Missing names are reported, never values.
func archeraConfig() (cfg insurance.Config, planID string, err error) {
	var missing []string
	get := func(name string) string {
		v := strings.TrimSpace(os.Getenv(name))
		if v == "" {
			missing = append(missing, name)
		}
		return v
	}
	cfg.APIKey = get(EnvArcheraAPIKey)
	cfg.OrgID = get(EnvArcheraOrgID)
	planID = get(EnvArcheraPlanID)
	if len(missing) > 0 {
		return insurance.Config{}, "", fmt.Errorf(
			"archera comparison is not configured: set %s, %s and %s in the server environment (missing: %s)",
			EnvArcheraAPIKey, EnvArcheraOrgID, EnvArcheraPlanID, strings.Join(missing, ", "))
	}
	return cfg, planID, nil
}

func (t *archeraComparisonTool) handle(ctx context.Context, _ *mcp.CallToolRequest, args archeraComparisonArgs) (*mcp.CallToolResult, archeraComparisonDTO, error) {
	cfg, planID, err := archeraConfig()
	if err != nil {
		return nil, archeraComparisonDTO{}, err
	}
	key := cfg.APIKey
	client, err := insurance.NewClient(cfg, t.httpClient)
	if err != nil {
		return nil, archeraComparisonDTO{}, archeraError(err, key)
	}
	req := insurance.ComparisonRequest{PlanID: planID, LineItemIDs: args.LineItemIDs, ContractTerms: args.ContractTerms}
	for _, p := range args.PaymentOptions {
		req.PaymentOptions = append(req.PaymentOptions, insurance.PaymentOption(p))
	}
	cmp, err := client.Comparison(ctx, req)
	if err != nil {
		return nil, archeraComparisonDTO{}, archeraError(err, key)
	}
	dto, err := buildArcheraComparison(cmp)
	if err != nil {
		return nil, archeraComparisonDTO{}, err
	}
	b, err := json.Marshal(dto)
	if err != nil {
		return nil, archeraComparisonDTO{}, fmt.Errorf("marshal archera comparison: %w", err)
	}
	if len(b) > archeraMaxResultBytes {
		return nil, archeraComparisonDTO{}, fmt.Errorf(
			"archera comparison result is %d bytes, over the %d byte limit: narrow it with line_item_ids", len(b), archeraMaxResultBytes)
	}
	return nil, *dto, nil
}

// archeraError reports a vendor failure by status and Retry-After only: the
// vendor message is dropped. Any other error is passed through with the key
// masked as a second line of defense behind the library's own redaction.
func archeraError(err error, key string) error {
	var he *insurance.HTTPError
	if errors.As(err, &he) {
		retry := "retry after: not given"
		if he.RetryAfter > 0 {
			retry = fmt.Sprintf("retry after %ds", int64(he.RetryAfter.Seconds()))
		}
		return fmt.Errorf("archera request failed: HTTP %d; %s", he.StatusCode, retry)
	}
	if key != "" && strings.Contains(err.Error(), key) {
		return errors.New(strings.ReplaceAll(err.Error(), key, "[redacted]"))
	}
	return err
}
