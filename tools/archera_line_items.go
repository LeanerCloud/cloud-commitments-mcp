package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/insurance"
)

const archeraLineItemsName = "cudly_archera_line_items"
const archeraLineItemsTitle = "Archera plan line items (read-only)"
const archeraListingNote = "Local pages repeat a complete vendor read; no atomic snapshot is guaranteed. Vendor UUIDs do not establish recommendation or cross-account association. Selection is not a quote or purchase; no currency is supplied."
const archeraLineItemsDescription = "Explicitly list Archera line-item UUIDs for a subsequent cudly_archera_comparison call. Read-only: no purchase, enrollment or audit write. Requires operator ARCHERA_API_KEY, ARCHERA_ORG_ID and ARCHERA_PLAN_ID, read at call time; no credentials, organization, plan or URL arguments. Fetches all vendor pages within 30 seconds before returning a sorted local window. offset defaults to 0; limit defaults to 100 (1..200). Quantities and commitments are exact text; missing offer dimensions are null, never inferred. Local pages are repeated complete reads without a guaranteed snapshot. IDs do not identify platform recommendations or cross-account association. Both partnership disclosures accompany the result. Errors include bounded Retry-After; no automatic retry."

type archeraLineItemsArgs struct {
	Limit  *int64 `json:"limit,omitempty" jsonschema:"local output count, default 100, integer 1 through 200; all vendor pages are fetched"`
	Offset int64  `json:"offset,omitempty" jsonschema:"nonnegative local output offset, default 0; all vendor pages are fetched"`
}

func (a archeraLineItemsArgs) window() (int64, error) {
	limit := int64(100)
	if a.Limit != nil {
		limit = *a.Limit
	}
	if a.Offset < 0 || limit < 1 || limit > 200 {
		return 0, errors.New("offset must be nonnegative and limit must be 1..200")
	}
	return limit, nil
}

type archeraLineItemsTool struct{ httpClient *http.Client }

func NewArcheraLineItemsTool() Registration { return &archeraLineItemsTool{} }

func (t *archeraLineItemsTool) Descriptor() Descriptor {
	return Descriptor{
		Name: archeraLineItemsName, Description: archeraLineItemsDescription,
		Annotations: readOnlyAnnotations(archeraLineItemsTitle, true),
		Product:     "insurance", Action: "line_items",
		ExamplePrompts: []string{"List Archera line-item UUIDs before explicitly comparing selected line items"},
	}
}

func (t *archeraLineItemsTool) Register(server *mcp.Server) error {
	schema, err := BuildInputSchema[archeraLineItemsArgs](nil)
	if err != nil {
		return err
	}
	zero, one, maxLimit := float64(0), float64(1), float64(200)
	schema.Properties["offset"].Minimum = &zero
	limit := schema.Properties["limit"]
	limit.Type, limit.Types, limit.AnyOf = "integer", nil, nil
	limit.Minimum, limit.Maximum = &one, &maxLimit
	descriptor := t.Descriptor()
	mcp.AddTool(server, &mcp.Tool{
		Name: descriptor.Name, Description: descriptor.Description,
		Annotations: descriptor.Annotations, InputSchema: schema,
	}, t.handle)
	return nil
}

type archeraListingRow struct {
	Region                *string `json:"region"`
	InstanceFamily        *string `json:"instance_family"`
	NativePlanType        *string `json:"native_plan_type"`
	NativeDurationSeconds *string `json:"native_duration_seconds"`
	LineItemID            string  `json:"line_item_id"`
	OfferID               string  `json:"offer_id"`
	SelectedQuantity      string  `json:"selected_quantity"`
	SelectedCommitment    string  `json:"selected_commitment"`
	ContractTerm          string  `json:"contract_term"`
	PaymentOption         string  `json:"payment_option"`
	Provider              string  `json:"provider"`
	CommitmentType        string  `json:"commitment_type"`
	IsSelected            bool    `json:"is_selected"`
}

type archeraListingPage struct {
	NextOffset            *int64              `json:"next_offset"`
	NonGatingDisclosure   string              `json:"non_gating_disclosure"`
	SponsorshipDisclosure string              `json:"sponsorship_disclosure"`
	Note                  string              `json:"note"`
	LineItems             []archeraListingRow `json:"line_items"`
	Total                 int64               `json:"total"`
	Offset                int64               `json:"offset"`
	Limit                 int64               `json:"limit"`
}

func archeraListingIdentityCheck(row insurance.LineItem, key string) error {
	if archeraSanitizeText(row.ID, key) != row.ID || archeraSanitizeText(row.OfferID, key) != row.OfferID {
		return errors.New("line-item identities cannot be safely displayed")
	}
	return nil
}

func archeraListingRowDTO(row insurance.LineItem, key string) (archeraListingRow, error) {
	text := func(s string) string { return archeraSanitizeText(s, key) }
	if err := archeraListingIdentityCheck(row, key); err != nil {
		return archeraListingRow{}, err
	}
	nullable := func(s *string) *string {
		if s == nil {
			return nil
		}
		value := text(*s)
		return &value
	}
	mapper := &archeraMapper{}
	commitment, duration := mapper.dec(row.SelectedCommitment), mapper.dec(row.Offer.DurationSeconds)
	if mapper.err != nil {
		return archeraListingRow{}, mapper.err
	}
	if commitment == nil {
		return archeraListingRow{}, errors.New("line-item selected commitment is unavailable")
	}
	return archeraListingRow{
		LineItemID: row.ID, OfferID: row.OfferID, IsSelected: row.IsSelected,
		SelectedQuantity: strconv.FormatInt(row.SelectedQuantity, 10), SelectedCommitment: *commitment,
		ContractTerm: text(row.ContractTerm), PaymentOption: text(string(row.PaymentOption)),
		Provider: text(string(row.Offer.Provider)), CommitmentType: text(row.ContractSpec.CommitmentType),
		Region: nullable(row.Offer.Region), InstanceFamily: nullable(row.Offer.InstanceFamily),
		NativePlanType: nullable(row.Offer.PlanType), NativeDurationSeconds: duration,
	}, nil
}

func archeraListingWindow(rows []insurance.LineItem, offset, limit int64, key string) (archeraListingPage, error) {
	total := int64(len(rows))
	if offset > total {
		return archeraListingPage{}, errors.New("offset exceeds the total line-item count")
	}
	for i := range rows {
		if err := archeraListingIdentityCheck(rows[i], key); err != nil {
			return archeraListingPage{}, err
		}
	}
	slices.SortFunc(rows, func(a, b insurance.LineItem) int {
		if c := strings.Compare(strings.ToLower(a.ID), strings.ToLower(b.ID)); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	end := offset + min(limit, total-offset)
	page := archeraListingPage{
		Total: total, Offset: offset, Limit: limit, LineItems: []archeraListingRow{},
		NonGatingDisclosure:   common.ArcheraNonGatingDisclosure,
		SponsorshipDisclosure: common.ArcheraSponsorshipDisclosure, Note: archeraListingNote,
	}
	if end < total {
		page.NextOffset = &end
	}
	for i := offset; i < end; i++ {
		dto, err := archeraListingRowDTO(rows[i], key)
		if err != nil {
			return archeraListingPage{}, err
		}
		page.LineItems = append(page.LineItems, dto)
	}
	return page, nil
}

func archeraListingOutputGuard(page archeraListingPage, key string) error {
	output, err := json.Marshal(page)
	if err != nil {
		return fmt.Errorf("marshal archera line items: %w", err)
	}
	if len(output) > archeraMaxResultBytes {
		return errors.New("line-item output exceeds 512 KiB; choose a smaller limit")
	}
	if strings.Contains(archeraSkeleton(string(output)), archeraSkeleton(key)) {
		return errors.New("line-item output cannot be safely displayed")
	}
	return nil
}

func (t *archeraLineItemsTool) handle(ctx context.Context, _ *mcp.CallToolRequest, args archeraLineItemsArgs) (*mcp.CallToolResult, archeraListingPage, error) {
	limit, err := args.window()
	if err != nil {
		return nil, archeraListingPage{}, err
	}
	cfg, plan, err := archeraConfig()
	if err != nil {
		return nil, archeraListingPage{}, err
	}
	key := cfg.APIKey.Reveal()
	client, err := insurance.NewClient(cfg, t.httpClient)
	if err != nil {
		return nil, archeraListingPage{}, archeraError(err, key)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rows, err := client.LineItems(ctx, plan)
	if err != nil {
		return nil, archeraListingPage{}, archeraError(err, key)
	}
	page, err := archeraListingWindow(rows, args.Offset, limit, key)
	if err != nil {
		return nil, archeraListingPage{}, err
	}
	if err := archeraListingOutputGuard(page, key); err != nil {
		return nil, archeraListingPage{}, err
	}
	if err := ctx.Err(); err != nil {
		return nil, archeraListingPage{}, err
	}
	return nil, page, nil
}
