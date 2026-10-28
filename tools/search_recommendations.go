package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
)

const searchRecommendationsName = "cudly_search_recommendations"

const searchRecommendationsTitle = "Search commitment recommendations"

// searchRecommendationsAnnotations is read-only/open-world (it reaches a
// live Cost Explorer/Advisor/Recommender API), but deliberately does NOT
// claim IdempotentHint -- see readOnlyAnnotations' doc comment for why the
// hint doesn't gate anything for a read-only tool per spec. The reason this
// tool still calls it out: an AWS search fans out to up to 6 Cost Explorer
// requests (searchCombos below) and AWS bills those per request, so a
// client that inferred "safe to retry freely" from a stray idempotent-style
// hint would be invited into a free-retry billing loop. openWorld=true
// because every search call reaches a live cloud provider API.
var searchRecommendationsAnnotations = readOnlyAnnotations(searchRecommendationsTitle, true)

// searchRecommendationsDescription ships in the tool schema, so the model
// reads it as the contract for what calling this costs. It states the
// guarantee that matters (nothing is ever bought) without claiming the call is
// free: an AWS reservation search that omits term_years and payment_option
// fans out to six Cost Explorer requests (searchCombos), and Cost Explorer
// bills per request. Saying "spends no money" would invite a model to re-run
// searches in a loop believing that has no cost.
const searchRecommendationsDescription = "Search for reserved-capacity purchase recommendations (RI/SP/CUD) " +
	"across AWS, Azure, or GCP. Read-only: makes no purchase and buys no commitment -- there is no dry_run or " +
	"confirm parameter because nothing is ever bought. It is not free of charge, though: an AWS search issues " +
	"one or more Cost Explorer requests (up to 6 when term_years and payment_option are both omitted, more " +
	"with pagination), and AWS bills those per request, so avoid re-running the same search in a loop. Use " +
	"this first to find what to buy, then feed a result's region/resource_type/count into the matching " +
	"cudly_<provider>_<product>_<action>_purchase tool. Results are checked against commitments bought in " +
	"the last 24 hours in the credentials' own account: a fully covered RDS, OpenSearch, Redshift or " +
	"ElastiCache recommendation is removed and listed under dedupe.suppressed; EC2, Azure, GCP and MemoryDB " +
	"recommendations that may be covered stay in the results and are listed under dedupe.flagged " +
	"(possibly_covered). Check dedupe before acting. A failure to list existing commitments fails the search."

// searchRecommendationsArgs mirrors common.RecommendationParams, adding the
// provider selector and the optional per-call credential overrides from the
// design doc's §4 config-exposure model (aws_profile /
// azure_subscription_id / gcp_project_id).
type searchRecommendationsArgs struct {
	Provider            string   `json:"provider" jsonschema:"cloud provider to search"`
	Service             string   `json:"service" jsonschema:"service to search, e.g. ec2, rds, elasticache, compute, computeengine"`
	Region              string   `json:"region,omitempty" jsonschema:"region to search; omit for account/global-level services such as Savings Plans"`
	GCPProjectID        string   `json:"gcp_project_id,omitempty" jsonschema:"GCP project ID override; default uses ambient project"`
	AzureSubscriptionID string   `json:"azure_subscription_id,omitempty" jsonschema:"Azure subscription ID override; default uses AZURE_SUBSCRIPTION_ID"`
	LookbackPeriod      string   `json:"lookback_period,omitempty" jsonschema:"cost/usage lookback window backing the recommendation; AWS Savings Plans searches default to 30d when omitted (AWS requires a value); reservation searches (EC2/RDS/etc) fall back to AWS's own server-side default (7 days) when omitted"`
	AWSProfile          string   `json:"aws_profile,omitempty" jsonschema:"AWS named profile override (~/.aws/config); default uses ambient credentials"`
	PaymentOption       string   `json:"payment_option,omitempty" jsonschema:"payment schedule filter; AWS Savings Plans searches default to no-upfront when omitted (AWS requires a value); reservation searches (EC2/RDS/etc) leave it omitted to search ALL THREE payment options and return a result per option"`
	IncludeSPTypes      []string `json:"include_sp_types,omitempty" jsonschema:"AWS Savings Plans types to include; omit for all"`
	AccountFilter       []string `json:"account_filter,omitempty" jsonschema:"restrict the search to these account/subscription/project IDs"`
	ExcludeSPTypes      []string `json:"exclude_sp_types,omitempty" jsonschema:"AWS Savings Plans types to exclude"`
	ExcludeRegions      []string `json:"exclude_regions,omitempty" jsonschema:"exclude these regions from the search"`
	IncludeRegions      []string `json:"include_regions,omitempty" jsonschema:"restrict the search to these regions, in addition to (or instead of) region"`
	TermYears           int      `json:"term_years,omitempty" jsonschema:"commitment term filter; AWS Savings Plans searches default to 1 (1yr, no-upfront) when omitted (AWS requires a value); reservation searches (EC2/RDS/etc) leave it omitted to search BOTH 1yr and 3yr and return a result per term"`
}

// searchRecommendationsResult is the tool's structured output.
type searchRecommendationsResult struct {
	Dedupe          searchDedupe            `json:"dedupe"`
	Recommendations []common.Recommendation `json:"recommendations"`
	Count           int                     `json:"count"`
}

type searchRecommendationsTool struct {
	// createProvider is a seam over provider.CreateProvider so tests can
	// inject a fake Provider without resolving real cloud credentials.
	createProvider func(name string, cfg *provider.Config) (provider.Provider, error)
}

// NewSearchRecommendationsTool builds the cudly_search_recommendations tool.
func NewSearchRecommendationsTool() Registration {
	return &searchRecommendationsTool{createProvider: provider.CreateProvider}
}

func (t *searchRecommendationsTool) Descriptor() Descriptor {
	return Descriptor{
		Name:        searchRecommendationsName,
		Description: searchRecommendationsDescription,
		Annotations: searchRecommendationsAnnotations,
		Action:      "search",
		ExamplePrompts: []string{
			"Search for AWS EC2 RI recommendations in us-east-1",
			"What RDS Reserved Instance recommendations exist for account 123456789012?",
			"Find GCP Compute Engine committed-use discount recommendations",
		},
	}
}

func (t *searchRecommendationsTool) Register(s *mcp.Server) error {
	// Every constrained field advertises its value set in the schema, so an
	// MCP client can discover the valid inputs without a round trip that
	// fails validation. These mirror ValidatePaymentOption / ValidateTermYears,
	// which stay the enforcing check: the schema is discoverability, not the
	// guard, since a client may send anything regardless of what it declares.
	schema, err := BuildInputSchema[searchRecommendationsArgs](map[string]FieldOverride{
		"provider":        {Enum: []any{string(common.ProviderAWS), string(common.ProviderAzure), string(common.ProviderGCP)}},
		"lookback_period": {Enum: []any{"7d", "30d", "60d"}},
		"payment_option": {Enum: []any{
			string(PaymentOptionAllUpfront),
			string(PaymentOptionPartialUpfront),
			string(PaymentOptionNoUpfront),
		}},
		"term_years": {Enum: []any{int(TermOneYear), int(TermThreeYear)}},
	})
	if err != nil {
		return err
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        searchRecommendationsName,
		Description: searchRecommendationsDescription,
		Annotations: searchRecommendationsAnnotations,
		InputSchema: schema,
	}, t.handle)
	return nil
}

func (t *searchRecommendationsTool) handle(ctx context.Context, _ *mcp.CallToolRequest, args searchRecommendationsArgs) (*mcp.CallToolResult, searchRecommendationsResult, error) {
	// Trim BEFORE validating: validateSearchArgs matches service against the
	// Savings Plans family predicate and rejects blank filter-list entries,
	// both of which must see the normalized values the rest of the call will
	// actually use.
	args = trimSearchArgsIdentifiers(args)
	providerType, term, args, err := validateSearchArgs(args)
	if err != nil {
		return nil, searchRecommendationsResult{}, err
	}

	prov, err := t.createProvider(string(providerType), providerConfigFromArgs(providerType, args))
	if err != nil {
		return nil, searchRecommendationsResult{}, fmt.Errorf("create %s provider: %w", providerType, err)
	}

	service, err := validateSupportedService(prov, args.Service)
	if err != nil {
		return nil, searchRecommendationsResult{}, err
	}

	recClient, err := prov.GetRecommendationsClient(ctx)
	if err != nil {
		return nil, searchRecommendationsResult{}, fmt.Errorf("get %s recommendations client: %w", providerType, err)
	}

	recs, err := fetchSearchCombos(ctx, recClient, providerType, service, term, args)
	if err != nil {
		return nil, searchRecommendationsResult{}, err
	}

	recs, dedupe, err := dedupeSearchResults(ctx, prov, recs)
	if err != nil {
		return nil, searchRecommendationsResult{}, err
	}

	return nil, searchRecommendationsResult{Count: len(recs), Recommendations: recs, Dedupe: dedupe}, nil
}

// searchCombo is one (term, payment option) pair to query. Cost Explorer's
// GetReservationPurchaseRecommendation accepts exactly one term and one
// payment option per request and returns recommendations only for that cell
// -- there is no "give me every variant" mode -- so covering the full menu
// takes one call per combo.
type searchCombo struct {
	term    string
	payment string
}

// allSearchTerms and allSearchPaymentOptions are the full reservation menus
// fanned out over when the caller omits term_years / payment_option. They
// mirror defaultDiscoveryTerms and defaultDiscoveryPaymentOptions in
// providers/aws/recommendations/client.go, which the scheduler's discovery
// sweep already fans out over for the same reason.
var (
	allSearchTerms          = []string{TermOneYear.RecommendationTerm(), TermThreeYear.RecommendationTerm()}
	allSearchPaymentOptions = []string{
		string(PaymentOptionAllUpfront),
		string(PaymentOptionPartialUpfront),
		string(PaymentOptionNoUpfront),
	}
)

// searchCombos returns every (term, payment) pair the search must query.
//
// For an AWS reservation search (EC2/RDS/ElastiCache/...), an omitted
// term_years or payment_option means "search all", so the omitted dimension
// expands to its full menu and the result is the Cartesian product: 6 combos
// when both are omitted, 2 or 3 when one is, 1 when both are supplied.
//
// Without this expansion, omitting the fields sent Cost Explorer an EMPTY
// TermInYears/PaymentOption (convertTermInYears and convertPaymentOption both
// silently map unrecognized input to ""), AWS quietly applied its own default
// of 1yr/all-upfront, and the caller saw exactly ONE of the six purchasable
// options while the tool claimed to have searched them all. On a real account
// that hid the best offer outright: 1yr/all-upfront saved 40% where
// 3yr/all-upfront saved 63% on the identical instance.
//
// Every other search -- Savings Plans (own required-field defaulting, see
// applySavingsPlansSearchDefaults), Azure (term/payment come back off the
// Advisor response, they are not request filters), GCP (no term/payment
// concept in its recommendations path) -- queries once with whatever the
// caller supplied, since fanning out there would re-issue the same query and
// duplicate results.
func searchCombos(providerType common.ProviderType, service common.ServiceType, term, payment string) []searchCombo {
	if providerType != common.ProviderAWS || common.IsSavingsPlan(service) {
		return []searchCombo{{term: term, payment: payment}}
	}

	terms := []string{term}
	if term == "" {
		terms = allSearchTerms
	}
	payments := []string{payment}
	if payment == "" {
		payments = allSearchPaymentOptions
	}

	combos := make([]searchCombo, 0, len(terms)*len(payments))
	for _, tm := range terms {
		for _, pay := range payments {
			combos = append(combos, searchCombo{term: tm, payment: pay})
		}
	}
	return combos
}

// fetchSearchCombos queries every combo searchCombos selected and returns the
// concatenated recommendations, always as a non-nil slice so a no-results
// search serializes as [] rather than null.
//
// Each combo is sent with a CONCRETE term and payment option, which is also
// what makes the returned recommendations self-describing: the AWS parser
// tags each one with the term/payment of the request that produced it
// (providers/aws/recommendations/parser_ri.go), so a fanned-out result set
// says which offer each set of money figures belongs to instead of leaving
// them blank and unattributable.
//
// A failing combo fails the whole search rather than being skipped. The
// scheduler's equivalent sweep (GetRecommendationsForService) tolerates
// per-combo errors because partial progress beats none in a batch job, but
// here the caller is choosing what to BUY: silently returning 5 of 6 offers
// is indistinguishable from "these are all your options" and recreates the
// exact defect this fan-out exists to fix. The error names the combo so a
// retry is targeted.
func fetchSearchCombos(
	ctx context.Context,
	recClient provider.RecommendationsClient,
	providerType common.ProviderType,
	service common.ServiceType,
	term string,
	args searchRecommendationsArgs,
) ([]common.Recommendation, error) {
	combos := searchCombos(providerType, service, term, args.PaymentOption)
	recs := make([]common.Recommendation, 0)
	for _, combo := range combos {
		got, err := recClient.GetRecommendations(ctx, recommendationParamsFromArgs(service, combo, args))
		if err != nil {
			if len(combos) == 1 {
				return nil, fmt.Errorf("get recommendations: %w", err)
			}
			return nil, fmt.Errorf("get recommendations (term=%s, payment_option=%s): %w", combo.term, combo.payment, err)
		}
		recs = append(recs, got...)
	}
	return recs, nil
}

// trimSearchArgsIdentifiers returns args with surrounding whitespace
// stripped from every free-text identifier field that flows into
// providerConfigFromArgs/recommendationParamsFromArgs (service, region,
// include_regions, exclude_regions, account_filter). Unlike the purchase
// tools' requireNonBlank, region is optional here, so trimming rather than
// rejecting a blank/whitespace value is correct: " us-east-1 " must resolve
// and search the same region as "us-east-1" instead of silently searching
// the wrong (or account-default) region.
//
// This runs before validateSearchArgs so service is already trimmed when
// isAWSSavingsPlansSearch and validateSupportedService match on it: without
// that, " savingsplans-compute" would both miss the Savings Plans defaulting
// branch and fail the supported-service check for a reason (a stray space)
// the error message would not explain.
func trimSearchArgsIdentifiers(args searchRecommendationsArgs) searchRecommendationsArgs {
	args.Service = strings.TrimSpace(args.Service)
	args.Region = strings.TrimSpace(args.Region)
	args.IncludeRegions = trimAll(args.IncludeRegions)
	args.ExcludeRegions = trimAll(args.ExcludeRegions)
	args.AccountFilter = trimAll(args.AccountFilter)
	// The credential overrides flow straight into ProviderConfig via
	// providerConfigFromArgs, where they select the account/subscription/
	// project the search runs against. A padded " my-profile " fails profile
	// resolution for a reason the error never mentions (the whitespace),
	// which reads as "these credentials are broken" rather than "this name
	// has a stray space". The purchase tools normalize the same three fields
	// through CredentialScope for the equivalent reason.
	args.AWSProfile = strings.TrimSpace(args.AWSProfile)
	args.AzureSubscriptionID = strings.TrimSpace(args.AzureSubscriptionID)
	args.GCPProjectID = strings.TrimSpace(args.GCPProjectID)
	return args
}

// trimAll returns a copy of ss with strings.TrimSpace applied to every
// entry, preserving a nil input as nil.
func trimAll(ss []string) []string {
	if ss == nil {
		return nil
	}
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = strings.TrimSpace(s)
	}
	return out
}

// rejectBlankListEntries fails loud when any entry of a supplied filter list
// is empty or whitespace-only. A blank entry cannot match a real region code
// or account ID, but a non-empty list still switches the corresponding
// filter ON: include_regions=["  "] would activate region filtering with a
// set that matches nothing and silently return zero recommendations, which
// reads as "your account has nothing to buy" rather than "your filter was
// malformed". Naming the offending index makes the malformed entry
// actionable instead of leaving the caller to guess which one it was.
func rejectBlankListEntries(field string, values []string) error {
	for i, v := range values {
		if v == "" {
			return fmt.Errorf("%s[%d] is blank: every entry must be a non-empty identifier", field, i)
		}
	}
	return nil
}

// validateFilterListEntries runs rejectBlankListEntries over every
// caller-supplied filter list. Split out of validateSearchArgs to keep that
// function under the repo's gocyclo gate (the pre-commit hook is -over 10,
// stricter than golangci's min-complexity 15).
func validateFilterListEntries(args searchRecommendationsArgs) error {
	lists := []struct {
		field  string
		values []string
	}{
		{"include_regions", args.IncludeRegions},
		{"exclude_regions", args.ExcludeRegions},
		{"account_filter", args.AccountFilter},
	}
	for _, list := range lists {
		if err := rejectBlankListEntries(list.field, list.values); err != nil {
			return err
		}
	}
	return nil
}

// providerConfigFromArgs builds the provider.Config for the given
// provider from the tool's per-call credential override fields (design §4).
func providerConfigFromArgs(providerType common.ProviderType, args searchRecommendationsArgs) *provider.Config {
	return &provider.Config{
		Name:                string(providerType),
		AWSProfile:          args.AWSProfile,
		AzureSubscriptionID: args.AzureSubscriptionID,
		GCPProjectID:        args.GCPProjectID,
		Region:              args.Region,
	}
}

// recommendationParamsFromArgs builds the common.RecommendationParams for
// the already-validated service and one combo from searchCombos. Term and
// payment option come from the combo, NOT from args, so a fanned-out search
// sends each (term, payment) cell concretely; every other field is shared
// across the fan-out and comes from args.
func recommendationParamsFromArgs(service common.ServiceType, combo searchCombo, args searchRecommendationsArgs) *common.RecommendationParams {
	return &common.RecommendationParams{
		Service:        service,
		Region:         args.Region,
		LookbackPeriod: args.LookbackPeriod,
		Term:           combo.term,
		PaymentOption:  combo.payment,
		AccountFilter:  args.AccountFilter,
		IncludeRegions: args.IncludeRegions,
		ExcludeRegions: args.ExcludeRegions,
		IncludeSPTypes: args.IncludeSPTypes,
		ExcludeSPTypes: args.ExcludeSPTypes,
	}
}

// validateProviderName returns the typed common.ProviderType for s, or an
// explicit error when s is not aws, azure, or gcp.
func validateProviderName(s string) (common.ProviderType, error) {
	switch common.ProviderType(s) {
	case common.ProviderAWS, common.ProviderAzure, common.ProviderGCP:
		return common.ProviderType(s), nil
	default:
		return "", fmt.Errorf("invalid provider %q: must be one of %s, %s, %s",
			s, common.ProviderAWS, common.ProviderAzure, common.ProviderGCP)
	}
}

// validateSupportedService checks service against prov's own
// GetSupportedServices() -- the provider's live list, not a hardcoded
// mirror of it -- so this tool never drifts from what each provider
// actually supports.
func validateSupportedService(prov provider.Provider, service string) (common.ServiceType, error) {
	if service == "" {
		return "", fmt.Errorf("service is required")
	}
	want := common.ServiceType(service)
	supported := prov.GetSupportedServices()
	for _, s := range supported {
		if s == want {
			return want, nil
		}
	}
	names := make([]string, len(supported))
	for i, s := range supported {
		names[i] = s.String()
	}
	return "", fmt.Errorf("invalid service %q for provider %s: must be one of %s", service, prov.Name(), strings.Join(names, ", "))
}
