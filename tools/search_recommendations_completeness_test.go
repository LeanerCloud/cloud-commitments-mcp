package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	awsprovider "github.com/LeanerCloud/cloud-commitments-go/providers/aws"
)

func TestSearchRecommendationsAWSCompletenessProtocol(t *testing.T) {
	t.Parallel()
	for _, selected := range []bool{false, true} {
		for _, kind := range []string{"valid", "empty", "mixed", "all-invalid"} {
			t.Run(fmt.Sprintf("selected=%t/%s", selected, kind), func(t *testing.T) {
				t.Parallel()
				fixture := &recommendationCompletenessHTTP{kind: kind}
				adapter := awsprovider.NewRecommendationsClient(aws.Config{
					Region: "us-east-1", HTTPClient: fixture,
					Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
						return aws.Credentials{AccessKeyID: "synthetic", SecretAccessKey: "synthetic"}, nil
					}),
				})
				tool := newTestSearchTool(&fakeProvider{name: "aws", services: []common.ServiceType{common.ServiceRDS}, recClient: adapter})
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				server := mcp.NewServer(&mcp.Implementation{Name: "completeness-server"}, nil)
				require.NoError(t, tool.Register(server))
				clientTransport, serverTransport := mcp.NewInMemoryTransports()
				serverSession, err := server.Connect(ctx, serverTransport, nil)
				require.NoError(t, err)
				defer serverSession.Close()
				client := mcp.NewClient(&mcp.Implementation{Name: "completeness-client"}, nil)
				session, err := client.Connect(ctx, clientTransport, nil)
				require.NoError(t, err)
				defer session.Close()
				args := map[string]any{"provider": "aws", "service": "rds"}
				wantRequests := []recommendationCompletenessRequest{
					{"ONE_YEAR", "ALL_UPFRONT"}, {"ONE_YEAR", "PARTIAL_UPFRONT"}, {"ONE_YEAR", "NO_UPFRONT"},
					{"THREE_YEARS", "ALL_UPFRONT"}, {"THREE_YEARS", "PARTIAL_UPFRONT"}, {"THREE_YEARS", "NO_UPFRONT"},
				}
				if selected {
					args["term_years"], args["payment_option"] = 3, "no-upfront"
					wantRequests = wantRequests[5:]
				}
				result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: searchRecommendationsName, Arguments: args})
				require.NoError(t, err)
				require.Empty(t, fixture.unexpected)
				if kind == "all-invalid" {
					wantRequests = wantRequests[:1]
				}
				assert.Equal(t, wantRequests, fixture.requests)
				if kind == "mixed" || kind == "all-invalid" {
					require.True(t, result.IsError, "incomplete response produced a successful short menu: %+v", result.StructuredContent)
					require.Nil(t, result.StructuredContent)
					var diagnostic strings.Builder
					for _, content := range result.Content {
						part, ok := content.(*mcp.TextContent)
						require.True(t, ok)
						diagnostic.WriteString(part.Text)
					}
					index, term, payment := 1, "3yr", "no-upfront"
					if kind == "all-invalid" {
						index = 0
						if !selected {
							term, payment = "1yr", "all-upfront"
						}
					}
					assert.Regexp(t, `\b1 failed details\b`, diagnostic.String())
					assert.Regexp(t, `\b0 failed scopes\b`, diagnostic.String())
					for _, want := range []string{fmt.Sprintf("block 0 detail %d", index), term, payment, "not-a-number"} {
						assert.Contains(t, diagnostic.String(), want)
					}
					if !selected {
						assert.Contains(t, diagnostic.String(), "term="+term+", payment_option="+payment)
					}
					return
				}
				require.False(t, result.IsError)
				encoded, err := json.Marshal(result.StructuredContent)
				require.NoError(t, err)
				var menu struct {
					Count           int               `json:"count"`
					Recommendations []json.RawMessage `json:"recommendations"`
				}
				require.NoError(t, json.Unmarshal(encoded, &menu))
				wantCount := len(wantRequests)
				if kind == "empty" {
					wantCount = 0
				}
				assert.Equal(t, wantCount, menu.Count)
				assert.Len(t, menu.Recommendations, wantCount)
				assert.NotNil(t, menu.Recommendations)
			})
		}
	}
}

type recommendationCompletenessRequest struct {
	Term    string `json:"TermInYears"`
	Payment string `json:"PaymentOption"`
}

type recommendationCompletenessHTTP struct {
	kind       string
	requests   []recommendationCompletenessRequest
	unexpected []string
}

func (f *recommendationCompletenessHTTP) Do(req *http.Request) (*http.Response, error) {
	operation := req.Header.Get("X-Amz-Target")
	if operation != "AWSInsightsIndexService.GetReservationPurchaseRecommendation" {
		f.unexpected = append(f.unexpected, operation)
		return nil, fmt.Errorf("unexpected SDK operation: %s", operation)
	}
	var params recommendationCompletenessRequest
	if err := json.NewDecoder(req.Body).Decode(&params); err != nil {
		return nil, err
	}
	f.requests = append(f.requests, params)
	valid := map[string]any{
		"RecommendedNumberOfInstancesToPurchase": "2",
		"EstimatedMonthlySavingsAmount":          "10", "EstimatedMonthlyOnDemandCost": "30",
		"InstanceDetails": map[string]any{"RDSInstanceDetails": map[string]any{
			"InstanceType": "db.t3.medium", "Region": "us-east-1", "DeploymentOption": "Single-AZ",
		}},
	}
	invalid := map[string]any{"RecommendedNumberOfInstancesToPurchase": "not-a-number"}
	details := []map[string]any{valid}
	switch f.kind {
	case "empty":
		details = nil
	case "all-invalid":
		details = []map[string]any{invalid}
	case "mixed":
		if params.Term == "THREE_YEARS" && params.Payment == "NO_UPFRONT" {
			details = append(details, invalid)
		}
	}
	body, err := json.Marshal(map[string]any{"Recommendations": []any{map[string]any{"RecommendationDetails": details}}})
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/x-amz-json-1.1"}}, Body: io.NopCloser(strings.NewReader(string(body))), Request: req}, nil
}
