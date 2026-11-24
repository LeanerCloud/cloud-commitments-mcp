package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/insurance"
)

const (
	archeraRecQuoteName  = "cudly_archera_recommendation_quote"
	archeraRecQuoteTitle = "Archera recommendation quote (read-only)"

	archeraRecQuoteProvider = "aws"
	archeraRecQuoteService  = "savings-plans-ec2instance"
	archeraRecQuotePlanType = "EC2Instance"
	archeraRecQuoteTimeout  = 30 * time.Second
	archeraMaxIdentityText  = 128
	// 20 integer digits, a point and 20 fraction digits.
	archeraMaxDecimalText = 41

	archeraRecQuoteDescription = "Read-only Archera quote for one stored AWS EC2 Instance Savings Plan recommendation. " +
		"Reads the operator's Archera line items and one scoped comparison, then returns status quote (figures as exact " +
		"decimal text, null means unknown, never zero), unavailable (no exact match could be proven; no figure), " +
		"ambiguous (several line items match; no figure) or ineligible (not an EC2 Instance Savings Plan with a 1 or 3 " +
		"year term; no vendor call). Every identity field is an explicit input and must match the vendor line item " +
		"exactly; a near match never yields a figure. Unlike the search and purchase tools, payment uses Archera spellings (no_upfront, partial_upfront, all_upfront) and term is 1yr or 3yr. Makes no purchase and enrolls nothing. Requires operator " +
		"ARCHERA_API_KEY, ARCHERA_ORG_ID and ARCHERA_PLAN_ID, read at call time; no credential, organization, plan or URL " +
		"arguments. Not a bindable quote; the API states no currency. Both partnership disclosures accompany the result. " +
		"Errors are fixed text, with status and bounded Retry-After for vendor HTTP failures; no automatic retry."
)

var archeraDecimalInput = regexp.MustCompile(`^\d{1,20}(\.\d{1,20})?$`)

// Fixed user text per status; vendor text never reaches the result.
var archeraRecQuoteMessages = map[insurance.RecommendationQuoteStatus]string{
	insurance.QuoteStatusQuote:       "Exactly one Archera line item matched this recommendation; figures are the vendor's, not a bindable quote.",
	insurance.QuoteStatusUnavailable: "No exact Archera match could be proven for this recommendation; no figure is given.",
	insurance.QuoteStatusAmbiguous:   "More than one Archera line item matches this recommendation; no figure is given.",
	insurance.QuoteStatusIneligible:  "This recommendation is not a supported EC2 Instance Savings Plan; Archera was not called.",
}

type archeraRecQuoteArgs struct {
	RecommendationID string `json:"recommendation_id" jsonschema:"platform recommendation UUID"`
	SourceAccountID  string `json:"source_account_id" jsonschema:"AWS account ID the recommendation was generated for"`
	OfferingID       string `json:"offering_id" jsonschema:"provider offering ID of the recommended plan"`
	PlanType         string `json:"plan_type" jsonschema:"must be EC2Instance"`
	Region           string `json:"region" jsonschema:"AWS region of the recommendation"`
	InstanceFamily   string `json:"instance_family" jsonschema:"EC2 instance family, for example m5"`
	Term             string `json:"term" jsonschema:"1yr or 3yr"`
	Payment          string `json:"payment" jsonschema:"no_upfront, partial_upfront or all_upfront"`
	HourlyCommitment string `json:"hourly_commitment" jsonschema:"positive decimal string in USD per hour, no exponent or sign, for example 0.123"`
}

// identity validates every input before any vendor call and maps it to the
// library's identity; a string never becomes a float.
func (a archeraRecQuoteArgs) identity() (*insurance.RecommendationIdentity, error) {
	if _, err := uuid.Parse(a.RecommendationID); err != nil || len(a.RecommendationID) != 36 {
		return nil, errors.New("recommendation_id must be a UUID")
	}
	if a.PlanType != archeraRecQuotePlanType {
		return nil, fmt.Errorf("plan_type must be %s", archeraRecQuotePlanType)
	}
	seconds := map[string]int64{string(common.Term1Year): 31536000, string(common.Term3Year): 94608000}[a.Term]
	if seconds == 0 {
		return nil, errors.New("term must be 1yr or 3yr")
	}
	switch insurance.PaymentOption(a.Payment) {
	case insurance.PaymentNoUpfront, insurance.PaymentPartialUpfront, insurance.PaymentAllUpfront:
	default:
		return nil, errors.New("payment must be no_upfront, partial_upfront or all_upfront")
	}
	if err := a.checkIdentityText(); err != nil {
		return nil, err
	}
	rate, err := parseArcheraHourly(a.HourlyCommitment)
	if err != nil {
		return nil, err
	}
	return &insurance.RecommendationIdentity{
		Provider: archeraRecQuoteProvider, Service: archeraRecQuoteService, PlanType: a.PlanType,
		SourceAccountID: a.SourceAccountID, OfferingID: a.OfferingID, Region: a.Region,
		InstanceFamily: a.InstanceFamily, Payment: a.Payment, NativeDurationSeconds: seconds,
		HourlyCommitment: rate,
	}, nil
}

func (a archeraRecQuoteArgs) checkIdentityText() error {
	for _, f := range []struct{ name, v string }{{"source_account_id", a.SourceAccountID}, {"offering_id", a.OfferingID},
		{"region", a.Region}, {"instance_family", a.InstanceFamily}} {
		name, v := f.name, f.v
		if v == "" || len(v) > archeraMaxIdentityText || v != archeraStripUnsafe(strings.TrimSpace(v)) || strings.ContainsAny(v, " \t") {
			return fmt.Errorf("%s must be non-empty text without whitespace or control characters, at most %d bytes", name, archeraMaxIdentityText)
		}
	}
	return nil
}

func parseArcheraHourly(text string) (*big.Rat, error) {
	if len(text) > archeraMaxDecimalText || !archeraDecimalInput.MatchString(text) {
		return nil, errors.New("hourly_commitment must be a plain decimal string such as 0.123")
	}
	rate, ok := new(big.Rat).SetString(text)
	if !ok || rate.Sign() <= 0 {
		return nil, errors.New("hourly_commitment must be greater than zero")
	}
	return rate, nil
}

type archeraRecQuoteResult struct {
	Comparison            *insurance.RecommendationComparisonDTO `json:"comparison"`
	Status                string                                 `json:"status"`
	Reason                string                                 `json:"reason"`
	Message               string                                 `json:"message"`
	NonGatingDisclosure   string                                 `json:"non_gating_disclosure"`
	SponsorshipDisclosure string                                 `json:"sponsorship_disclosure"`
}

type archeraRecQuoteTool struct{ httpClient *http.Client }

// NewArcheraRecommendationQuoteTool builds the cudly_archera_recommendation_quote tool.
func NewArcheraRecommendationQuoteTool() Registration { return &archeraRecQuoteTool{} }

func (t *archeraRecQuoteTool) Descriptor() Descriptor {
	return Descriptor{
		Name: archeraRecQuoteName, Description: archeraRecQuoteDescription,
		Annotations: readOnlyAnnotations(archeraRecQuoteTitle, true),
		Product:     "insurance", Action: "recommendation_quote",
		ExamplePrompts: []string{"Quote this EC2 Instance Savings Plan recommendation with Archera"},
	}
}

func (t *archeraRecQuoteTool) Register(server *mcp.Server) error {
	schema, err := BuildInputSchema[archeraRecQuoteArgs](nil)
	if err != nil {
		return err
	}
	schema.Properties["plan_type"].Enum = []any{archeraRecQuotePlanType}
	schema.Properties["term"].Enum = []any{string(common.Term1Year), string(common.Term3Year)}
	schema.Properties["payment"].Enum = []any{string(insurance.PaymentNoUpfront), string(insurance.PaymentPartialUpfront), string(insurance.PaymentAllUpfront)}
	d := t.Descriptor()
	mcp.AddTool(server, &mcp.Tool{Name: d.Name, Description: d.Description, Annotations: d.Annotations, InputSchema: schema}, t.handle)
	return nil
}

// archeraQuoteError maps the library's typed failure to fixed text; the vendor
// cause is already dropped by the library.
func archeraQuoteError(err error, key string) error {
	var qe *insurance.QuoteError
	if !errors.As(err, &qe) {
		return archeraError(err, key)
	}
	text := fmt.Sprintf("archera recommendation quote failed: %s at %s", qe.Kind, qe.Stage)
	if qe.Status != 0 {
		retry := "retry after: not given"
		if qe.RetryAfter > 0 {
			retry = fmt.Sprintf("retry after %ds", int64(qe.RetryAfter.Seconds()))
		}
		text += fmt.Sprintf("; HTTP %d; %s", qe.Status, retry)
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("%s: %w", text, context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", text, context.DeadlineExceeded)
	}
	return errors.New(text)
}

func archeraRecQuoteResultFrom(q *insurance.RecommendationQuote, key string) (archeraRecQuoteResult, error) {
	msg, ok := archeraRecQuoteMessages[q.Status]
	if !ok || (q.Status == insurance.QuoteStatusQuote) != (q.Comparison != nil) {
		return archeraRecQuoteResult{}, errors.New("archera quote result is inconsistent")
	}
	out := archeraRecQuoteResult{
		Status: string(q.Status), Reason: string(q.Reason), Message: msg, Comparison: q.Comparison,
		NonGatingDisclosure: common.ArcheraNonGatingDisclosure, SponsorshipDisclosure: common.ArcheraSponsorshipDisclosure,
	}
	b, err := json.Marshal(out)
	if err != nil {
		return archeraRecQuoteResult{}, fmt.Errorf("marshal archera quote: %w", err)
	}
	if len(b) > archeraMaxResultBytes {
		return archeraRecQuoteResult{}, errors.New("archera quote output exceeds 512 KiB")
	}
	if archeraJSONHasKey(b, key) {
		return archeraRecQuoteResult{}, errors.New("archera quote output cannot be safely displayed")
	}
	return out, nil
}

// archeraJSONHasKey checks the encoded bytes and every decoded string: JSON
// escapes < > & and U+2028/2029, so a key containing them is invisible in the
// encoded form alone.
func archeraJSONHasKey(encoded []byte, key string) bool {
	want := archeraSkeleton(key)
	if strings.Contains(archeraSkeleton(string(encoded)), want) {
		return true
	}
	var decoded any
	if json.Unmarshal(encoded, &decoded) != nil {
		return true
	}
	var texts []string
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			texts = append(texts, x)
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for k, e := range x {
				texts = append(texts, k)
				walk(e)
			}
		}
	}
	walk(decoded)
	for _, t := range texts {
		if strings.Contains(archeraSkeleton(t), want) {
			return true
		}
	}
	return false
}

func (t *archeraRecQuoteTool) handle(ctx context.Context, _ *mcp.CallToolRequest, args archeraRecQuoteArgs) (*mcp.CallToolResult, archeraRecQuoteResult, error) {
	rec, err := args.identity()
	if err != nil {
		return nil, archeraRecQuoteResult{}, err
	}
	cfg, plan, err := archeraConfig()
	if err != nil {
		return nil, archeraRecQuoteResult{}, err
	}
	key := cfg.APIKey.Reveal()
	client, err := insurance.NewClient(cfg, t.httpClient)
	if err != nil {
		return nil, archeraRecQuoteResult{}, archeraError(err, key)
	}
	ctx, cancel := context.WithTimeout(ctx, archeraRecQuoteTimeout)
	defer cancel()
	q, err := insurance.QuoteRecommendation(ctx, client, plan, args.RecommendationID, rec)
	if err != nil {
		return nil, archeraRecQuoteResult{}, archeraQuoteError(err, key)
	}
	out, err := archeraRecQuoteResultFrom(q, key)
	return nil, out, err
}
