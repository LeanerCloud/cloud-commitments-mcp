package tools

import (
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSearchRecommendationsInvalidProvider(t *testing.T) {
	t.Parallel()
	tool := newTestSearchTool(&fakeProvider{})
	_, _, err := tool.handle(context.Background(), nil, searchRecommendationsArgs{
		Provider: "openstack",
		Service:  "ec2",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid provider")
}

func TestSearchRecommendationsUnsupportedService(t *testing.T) {
	t.Parallel()
	fp := &fakeProvider{name: "aws", services: []common.ServiceType{common.ServiceEC2, common.ServiceRDS}}
	tool := newTestSearchTool(fp)

	_, _, err := tool.handle(context.Background(), nil, searchRecommendationsArgs{
		Provider: "aws",
		Service:  "cosmosdb", // not an AWS service
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid service")
}

func TestSearchRecommendationsInvalidTermYears(t *testing.T) {
	t.Parallel()
	fp := &fakeProvider{name: "aws", services: []common.ServiceType{common.ServiceEC2}}
	tool := newTestSearchTool(fp)

	_, _, err := tool.handle(context.Background(), nil, searchRecommendationsArgs{
		Provider:  "aws",
		Service:   "ec2",
		TermYears: 2,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid term_years")
}

// TestSearchRecommendationsInvalidLookbackPeriod is the regression guard for
// the CodeRabbit finding: lookback_period was constrained only by the
// advertised MCP jsonschema enum (Register's BuildInputSchema override), not
// re-validated in the handler, so a direct MCP call bypassing schema
// enforcement could pass an unsupported value through to the provider.
func TestSearchRecommendationsInvalidLookbackPeriod(t *testing.T) {
	t.Parallel()
	fp := &fakeProvider{name: "aws", services: []common.ServiceType{common.ServiceEC2}}
	tool := newTestSearchTool(fp)

	_, _, err := tool.handle(context.Background(), nil, searchRecommendationsArgs{
		Provider:       "aws",
		Service:        "ec2",
		LookbackPeriod: "90d",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid lookback_period")
}

func TestSearchRecommendationsInvalidSPType(t *testing.T) {
	t.Parallel()
	fp := &fakeProvider{name: "aws", services: []common.ServiceType{common.ServiceSavingsPlansAll}}
	tool := newTestSearchTool(fp)

	_, _, err := tool.handle(context.Background(), nil, searchRecommendationsArgs{
		Provider:       "aws",
		Service:        string(common.ServiceSavingsPlansAll),
		IncludeSPTypes: []string{"NotARealType"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "include_sp_types")
}

// TestSearchRecommendationsMultipleInvalidFieldsJoinedInOneError is the
// validate-all-at-once safety net (issue #1506 change 2): a request with
// several invalid fields must name all of them in one returned error instead
// of surfacing only the first one found.
func TestSearchRecommendationsMultipleInvalidFieldsJoinedInOneError(t *testing.T) {
	t.Parallel()
	fp := &fakeProvider{name: "aws", services: []common.ServiceType{common.ServiceEC2}}
	tool := newTestSearchTool(fp)

	_, _, err := tool.handle(context.Background(), nil, searchRecommendationsArgs{
		Provider:       "aws",
		Service:        "ec2",
		PaymentOption:  "not-a-real-option",
		LookbackPeriod: "90d",
		IncludeSPTypes: []string{"NotARealType"},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid payment_option")
	assert.Contains(t, err.Error(), "invalid lookback_period")
	assert.Contains(t, err.Error(), "include_sp_types")
}

// TestSearchBlankFilterListEntriesRejected is the regression guard for the
// silent empty-result path found in review. A blank entry cannot match any
// real region code or account ID, but a non-empty list still switches the
// corresponding filter ON: include_regions=["  "] activated region
// filtering with a set matching nothing and returned zero recommendations,
// which reads as "your account has nothing worth buying" rather than "your
// filter was malformed".
func TestSearchBlankFilterListEntriesRejected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mutate  func(*searchRecommendationsArgs)
		wantSub string
	}{
		{"blank include_regions entry", func(a *searchRecommendationsArgs) {
			a.IncludeRegions = []string{"  "}
		}, "include_regions[0] is blank"},
		{"blank exclude_regions entry among valid ones", func(a *searchRecommendationsArgs) {
			a.ExcludeRegions = []string{"us-east-1", ""}
		}, "exclude_regions[1] is blank"},
		{"blank account_filter entry", func(a *searchRecommendationsArgs) {
			a.AccountFilter = []string{"111111111111", "\t"}
		}, "account_filter[1] is blank"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			args := searchRecommendationsArgs{Provider: string(common.ProviderAWS), Service: "ec2"}
			tc.mutate(&args)

			tool := &searchRecommendationsTool{
				createProvider: func(_ string, _ *provider.ProviderConfig) (provider.Provider, error) {
					t.Fatal("provider must not be created for malformed filter input")
					return nil, nil
				},
			}
			_, _, err := tool.handle(context.Background(), nil, args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantSub)
		})
	}
}
