package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/insurance"
)

const (
	quoteRecID   = "9aadf00d-ffff-4aba-8ada-abcdefabcdef"
	quoteItemID  = "caadf00d-ffff-4aba-8ada-abcdefabcdef"
	quoteOfferID = "daadf00d-ffff-4aba-8ada-abcdefabcdef"
	quoteSecond  = "eaadf00d-ffff-4aba-8ada-abcdefabcdef"
	quoteHourly  = "0.50000000000000000001"
)

func quoteItemJSON(id string) string {
	return `{"id":"` + id + `","plan_id":"` + testArcheraPlan + `","offer_id":"` + quoteOfferID + `","account_id":"Source-account-a","selected_quantity":7,"selected_commitment":0.50000000000000000001,"is_selected":true,"contract_term":"one_year_gris","payment_option":"No Upfront","contract_spec":{"commitment_type":"opaque-contract-type"},"offer":{"id":"` + quoteOfferID + `","type":"savings_plan","provider":"aws","provider_offering_id":"provider-offer-a","offering_id":"distinct-catalog-offer","duration_seconds":31536000,"region":"us-east-1","az":null,"instance_type":null,"instance_family":"m5","product_description":null,"tenancy":null,"plan_type":"EC2Instance","is_multi_az":null,"is_flexible":null,"payment_option":"No Upfront"}}`
}

func quoteComparisonJSON() string {
	current := `{"is_current":true,"offer_id":"` + quoteOfferID + `","selected_amount":0.50000000000000000001,"commitment_type":"savings_plan","contract_term":"one_year_gris","payment_option":"no_upfront","offer":{"provider":"aws","region":"us-east-1"},"discount_rate":0,"commitment_upfront_cost":0,"commitment_financials_monthly_rate":{},"delta_vs_current":{"monthly_net_savings":0,"upfront_cost":0,"discount_rate":0}}`
	candidate := strings.ReplaceAll(strings.Replace(current, `"is_current":true`, `"is_current":false`, 1), quoteOfferID, "faadf00d-ffff-4aba-8ada-abcdefabcdef")
	candidate = strings.Replace(candidate, "no_upfront", "all_upfront", 1)
	return `{"current_totals":{"commitment_financials_monthly_rate":{},"commitment_upfront_cost":0},"hypothetical_totals":[],"data":[{"line_item_id":"` + quoteItemID + `","current":` + current + `,"candidates":[` + candidate + "," + candidate + `]}]}`
}

// quoteTransport serves the line items and the comparison and records requests.
func quoteTransport(t *testing.T, items ...string) *recordingTransport {
	t.Helper()
	return &recordingTransport{respond: func(r *http.Request) (*http.Response, error) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "api.archera.ai", r.URL.Host)
		assert.Equal(t, testArcheraKey, r.Header.Get("x-api-key"))
		if strings.HasSuffix(r.URL.Path, "/comparison") {
			return okTransport([]byte(quoteComparisonJSON())).RoundTrip(r)
		}
		rt := okTransport([]byte("[" + strings.Join(items, ",") + "]"))
		rt.header = http.Header{"X-Pagination": {`{"total":` + strconv.Itoa(len(items)) + `,"total_pages":1,"page":1,"first_page":1,"last_page":1}`}}
		return rt.RoundTrip(r)
	}}
}

func quoteArgs() map[string]any {
	return map[string]any{
		"recommendation_id": quoteRecID, "source_account_id": "Source-account-a", "offering_id": "provider-offer-a",
		"plan_type": "EC2Instance", "region": "us-east-1", "instance_family": "m5", "term": "1yr",
		"payment": "no_upfront", "hourly_commitment": quoteHourly,
	}
}

func callQuote(t *testing.T, rt http.RoundTripper, args map[string]any) (out map[string]any, errText, protocol string) {
	t.Helper()
	res, protocol, err := callArcheraTool(context.Background(), t, &archeraRecQuoteTool{httpClient: &http.Client{Transport: rt}}, archeraRecQuoteName, args)
	require.NoError(t, err)
	if res.IsError {
		return nil, resultText(res), protocol
	}
	b, err := json.Marshal(res.StructuredContent)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &out))
	return out, "", protocol
}

func TestArcheraRecommendationQuoteExactMatchReturnsFigures(t *testing.T) {
	setArcheraEnv(t)
	rt := quoteTransport(t, quoteItemJSON(quoteItemID))
	out, errText, protocol := callQuote(t, rt, quoteArgs())
	require.Empty(t, errText)
	assert.Equal(t, "quote", out["status"])
	assert.NotEmpty(t, out["message"])
	assert.NotEmpty(t, out["non_gating_disclosure"])
	assert.NotEmpty(t, out["sponsorship_disclosure"])
	cmp := out["comparison"].(map[string]any)
	assert.Equal(t, quoteRecID, cmp["recommendation_id"])
	assert.Nil(t, cmp["currency"], "the API states no currency; unknown is null")
	assert.Equal(t, quoteHourly, cmp["current"].(map[string]any)["hourly_selected_commitment"])
	assert.Equal(t, 2, rt.count(), "one line-item read and one comparison")
	assert.NotContains(t, protocol, testArcheraKey)
}

func TestArcheraRecommendationQuoteInexactIdentityNeverYieldsFigure(t *testing.T) {
	setArcheraEnv(t)
	edits := map[string]func(map[string]any){
		"commitment": func(a map[string]any) { a["hourly_commitment"] = "0.5" },
		"account":    func(a map[string]any) { a["source_account_id"] = "source-account-a" },
		"offering":   func(a map[string]any) { a["offering_id"] = "provider-offer-b" },
		"region":     func(a map[string]any) { a["region"] = "us-east-2" },
		"family":     func(a map[string]any) { a["instance_family"] = "c5" },
		"term":       func(a map[string]any) { a["term"] = "3yr" },
		"payment":    func(a map[string]any) { a["payment"] = "all_upfront" },
	}
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			rt := quoteTransport(t, quoteItemJSON(quoteItemID))
			args := quoteArgs()
			edit(args)
			out, errText, _ := callQuote(t, rt, args)
			require.Empty(t, errText)
			assert.Equal(t, "unavailable", out["status"])
			assert.Equal(t, "identity_unproven", out["reason"])
			assert.Equal(t, "No exact Archera match could be proven for this recommendation; no figure is given.", out["message"])
			assert.Nil(t, out["comparison"])
			assert.Equal(t, 1, rt.count(), "no comparison read without a unique exact match")
		})
	}
}

func TestArcheraRecommendationQuoteAmbiguousHasNoFigure(t *testing.T) {
	setArcheraEnv(t)
	rt := quoteTransport(t, quoteItemJSON(quoteItemID), quoteItemJSON(quoteSecond))
	out, errText, _ := callQuote(t, rt, quoteArgs())
	require.Empty(t, errText)
	assert.Equal(t, "ambiguous", out["status"])
	assert.Equal(t, "ambiguous_line_items", out["reason"])
	assert.Equal(t, "More than one Archera line item matches this recommendation; no figure is given.", out["message"])
	assert.Nil(t, out["comparison"])
	assert.Equal(t, 1, rt.count())
}

// The inputs the tool accepts are always eligible, so the library's ineligible
// status is exercised on the result mapping directly.
func TestArcheraRecommendationQuoteIneligibleStatusFixedText(t *testing.T) {
	out, err := archeraRecQuoteResultFrom(&insurance.RecommendationQuote{
		Status: insurance.QuoteStatusIneligible, Reason: insurance.ReasonIdentityUnproven}, testArcheraKey)
	require.NoError(t, err)
	assert.Equal(t, "ineligible", out.Status)
	assert.Nil(t, out.Comparison)
	assert.Equal(t, "This recommendation is not a supported EC2 Instance Savings Plan; Archera was not called.", out.Message)
}

func TestArcheraRecommendationQuoteResultInconsistentRejected(t *testing.T) {
	_, err := archeraRecQuoteResultFrom(&insurance.RecommendationQuote{Status: insurance.QuoteStatusQuote}, testArcheraKey)
	require.Error(t, err, "a quote without figures is never shown")
	_, err = archeraRecQuoteResultFrom(&insurance.RecommendationQuote{Status: insurance.QuoteStatusUnavailable,
		Comparison: &insurance.RecommendationComparisonDTO{}}, testArcheraKey)
	require.Error(t, err, "figures on a non-quote status are never shown")
	_, err = archeraRecQuoteResultFrom(&insurance.RecommendationQuote{Status: "weird"}, testArcheraKey)
	require.Error(t, err)
}

func TestArcheraRecommendationQuoteOutputGuards(t *testing.T) {
	leak := &insurance.RecommendationQuote{Status: insurance.QuoteStatusQuote,
		Comparison: &insurance.RecommendationComparisonDTO{RecommendationID: "id " + testArcheraKey}}
	_, err := archeraRecQuoteResultFrom(leak, testArcheraKey)
	require.ErrorContains(t, err, "cannot be safely displayed")
	spaced := &insurance.RecommendationQuote{Status: insurance.QuoteStatusQuote,
		Comparison: &insurance.RecommendationComparisonDTO{RecommendationID: strings.Join(strings.Split(testArcheraKey, ""), "\u200b")}}
	_, err = archeraRecQuoteResultFrom(spaced, testArcheraKey)
	require.Error(t, err, "a key split by format characters is still withheld")

	huge := &insurance.RecommendationQuote{Status: insurance.QuoteStatusQuote,
		Comparison: &insurance.RecommendationComparisonDTO{RecommendationID: strings.Repeat("x", archeraMaxResultBytes)}}
	_, err = archeraRecQuoteResultFrom(huge, testArcheraKey)
	require.ErrorContains(t, err, "512 KiB")
}

func TestArcheraRecommendationQuoteInvalidInputsSendNothing(t *testing.T) {
	setArcheraEnv(t)
	bad := map[string]map[string]any{
		"recommendation_id": {"recommendation_id": "not-a-uuid"},
		"plan_type":         {"plan_type": "Compute"},
		"term":              {"term": "2yr"},
		"payment":           {"payment": "monthly"},
		"empty account":     {"source_account_id": ""},
		"space offering":    {"offering_id": "a b"},
		"control region":    {"region": "us\u0000east"},
		"long family":       {"instance_family": strings.Repeat("m", 129)},
		"empty commitment":  {"hourly_commitment": ""},
		"exponent":          {"hourly_commitment": "1e3"},
		"negative":          {"hourly_commitment": "-1"},
		"zero":              {"hourly_commitment": "0"},
		"no leading digit":  {"hourly_commitment": ".5"},
		"too long decimal":  {"hourly_commitment": "0." + strings.Repeat("1", 21)},
		"json number":       {"hourly_commitment": 0.5},
	}
	for name, edit := range bad {
		t.Run(name, func(t *testing.T) {
			rt := quoteTransport(t, quoteItemJSON(quoteItemID))
			args := quoteArgs()
			for k, v := range edit {
				args[k] = v
			}
			res, _, err := callArcheraTool(context.Background(), t, &archeraRecQuoteTool{httpClient: &http.Client{Transport: rt}}, archeraRecQuoteName, args)
			if err == nil {
				assert.True(t, res.IsError)
			}
			assert.Zero(t, rt.count(), "validation precedes any vendor call")
		})
	}
}

func TestArcheraRecommendationQuoteUnconfiguredSendsNothing(t *testing.T) {
	t.Setenv(EnvArcheraAPIKey, "")
	t.Setenv(EnvArcheraOrgID, "")
	t.Setenv(EnvArcheraPlanID, "")
	rt := quoteTransport(t)
	_, errText, _ := callQuote(t, rt, quoteArgs())
	assert.Contains(t, errText, "archera is not configured")
	assert.Zero(t, rt.count())
}

func TestArcheraRecommendationQuoteVendorErrorsAreFixedText(t *testing.T) {
	setArcheraEnv(t)
	echo := []byte(`{"message":"VENDOR-INTERNAL ` + testArcheraKey + `"}`)
	cases := []struct {
		header     http.Header
		name, want string
		status     int
	}{
		{http.Header{"Retry-After": {"120"}}, "429", "archera recommendation quote failed: vendor_rejected at line_items; HTTP 429; retry after 120s", 429},
		{nil, "401 echo", "archera recommendation quote failed: vendor_rejected at line_items; HTTP 401; retry after: not given", 401},
		{nil, "503", "archera recommendation quote failed: vendor_unavailable at line_items; HTTP 503; retry after: not given", 503},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := &recordingTransport{status: tc.status, header: tc.header, body: echo}
			_, errText, protocol := callQuote(t, rt, quoteArgs())
			assert.Equal(t, tc.want, errText)
			assert.NotContains(t, protocol, testArcheraKey)
			assert.NotContains(t, protocol, "VENDOR-INTERNAL")
			assert.Equal(t, 1, rt.count(), "no retry")
		})
	}
}

func TestArcheraRecommendationQuoteSchemaAndAnnotations(t *testing.T) {
	d := NewArcheraRecommendationQuoteTool().Descriptor()
	assert.True(t, d.Annotations.ReadOnlyHint)
	assert.False(t, *d.Annotations.DestructiveHint)
	assert.True(t, *d.Annotations.OpenWorldHint)
	assert.False(t, d.RealPurchaseEnabled)

	schema, err := BuildInputSchema[archeraRecQuoteArgs](nil)
	require.NoError(t, err)
	for _, f := range []string{"recommendation_id", "source_account_id", "offering_id", "plan_type", "region", "instance_family", "term", "payment", "hourly_commitment"} {
		require.Contains(t, schema.Properties, f)
		assert.Contains(t, schema.Required, f)
		assert.Equal(t, "string", schema.Properties[f].Type, f)
	}
	assert.NotContains(t, schema.Properties, "api_key")
	assert.NotContains(t, schema.Properties, "plan_id")
}

func TestArcheraRecommendationQuoteKeyWithJSONEscapedRunesIsWithheld(t *testing.T) {
	const key = "abc<def&ghi>jkl\u2028mno"
	for name, dto := range map[string]*insurance.RecommendationComparisonDTO{
		"value": {RecommendationID: "id " + key},
		"split": {RecommendationID: strings.Join(strings.Split(key, ""), "\u200b")},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := archeraRecQuoteResultFrom(&insurance.RecommendationQuote{Status: insurance.QuoteStatusQuote, Comparison: dto}, key)
			require.ErrorContains(t, err, "cannot be safely displayed")
		})
	}
}

func TestArcheraQuoteErrorKeepsContextSentinels(t *testing.T) {
	canceled := archeraQuoteError(&insurance.QuoteError{Kind: insurance.QuoteErrCanceled, Stage: "line_items"}, testArcheraKey)
	require.ErrorIs(t, canceled, context.Canceled)
	assert.Equal(t, "archera recommendation quote failed: canceled at line_items: context canceled", canceled.Error())
	timeout := archeraQuoteError(&insurance.QuoteError{Kind: insurance.QuoteErrTimeout, Stage: "comparison"}, testArcheraKey)
	require.ErrorIs(t, timeout, context.DeadlineExceeded)
	assert.NotErrorIs(t, timeout, context.Canceled)
	other := archeraQuoteError(assert.AnError, testArcheraKey)
	assert.NotErrorAs(t, other, new(*insurance.QuoteError))
}

func TestArcheraRecommendationQuoteHandleCancellationAndDeadline(t *testing.T) {
	setArcheraEnv(t)
	tool := &archeraRecQuoteTool{httpClient: &http.Client{Transport: quoteTransport(t, quoteItemJSON(quoteItemID))}}
	args := archeraRecQuoteArgs{RecommendationID: quoteRecID, SourceAccountID: "Source-account-a", OfferingID: "provider-offer-a",
		PlanType: "EC2Instance", Region: "us-east-1", InstanceFamily: "m5", Term: "1yr", Payment: "no_upfront", HourlyCommitment: quoteHourly}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := tool.handle(ctx, nil, args)
	require.ErrorIs(t, err, context.Canceled)

	var deadline time.Duration
	rt := &recordingTransport{respond: func(r *http.Request) (*http.Response, error) {
		d, ok := r.Context().Deadline()
		require.True(t, ok, "the vendor call carries a deadline")
		deadline = time.Until(d)
		return nil, context.DeadlineExceeded
	}}
	tool.httpClient = &http.Client{Transport: rt}
	_, _, err = tool.handle(context.Background(), nil, args)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Greater(t, deadline, 25*time.Second)
	assert.LessOrEqual(t, deadline, archeraRecQuoteTimeout)
}

func TestArcheraRecQuoteIdentityChecks(t *testing.T) {
	good := archeraRecQuoteArgs{RecommendationID: quoteRecID, SourceAccountID: "a", OfferingID: "o", PlanType: "EC2Instance",
		Region: "r", InstanceFamily: "m5", Term: "3yr", Payment: "all_upfront", HourlyCommitment: "1.5"}
	rec, err := good.identity()
	require.NoError(t, err)
	assert.Equal(t, int64(94608000), rec.NativeDurationSeconds)
	assert.Equal(t, "1.5", rec.HourlyCommitment.FloatString(1))
	for name, edit := range map[string]func(*archeraRecQuoteArgs){
		"plan_type": func(a *archeraRecQuoteArgs) { a.PlanType = "Compute" },
		"term":      func(a *archeraRecQuoteArgs) { a.Term = "2yr" },
		"term case": func(a *archeraRecQuoteArgs) { a.Term = "1YR" },
		"uuid len":  func(a *archeraRecQuoteArgs) { a.RecommendationID = "{" + quoteRecID + "}" },
	} {
		t.Run(name, func(t *testing.T) {
			a := good
			edit(&a)
			_, ierr := a.identity()
			require.Error(t, ierr)
		})
	}
	both := good
	both.OfferingID, both.SourceAccountID = "", ""
	_, err = both.identity()
	require.ErrorContains(t, err, "source_account_id", "field checks run in a fixed order")
	maxText := good
	maxText.HourlyCommitment = strings.Repeat("9", 20) + "." + strings.Repeat("9", 20)
	_, err = maxText.identity()
	require.NoError(t, err, "the longest accepted decimal passes the length cap")
}
