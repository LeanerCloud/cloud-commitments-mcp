package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
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
	for _, service := range []common.ServiceType{common.ServiceRDS, common.ServiceSavingsPlansCompute, common.ServiceSavingsPlansEC2Instance, common.ServiceSavingsPlansSageMaker, common.ServiceSavingsPlansDatabase, common.ServiceSavingsPlansAll} {
		for _, selected := range []bool{false, true} {
			kinds := []string{"valid", "empty", "mixed", "all-invalid"}
			if common.IsSavingsPlan(service) {
				kinds = append(kinds, "partial-type-failure", "all-type-failure", "late-page-failure")
			}
			for _, kind := range kinds {
				t.Run(fmt.Sprintf("%s/selected=%t/%s", service, selected, kind), func(t *testing.T) {
					t.Parallel()
					fixture := &recommendationCompletenessHTTP{kind: kind}
					adapter := awsprovider.NewRecommendationsClient(aws.Config{
						Region: "us-east-1", HTTPClient: fixture, RetryMaxAttempts: 1,
						Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
							return aws.Credentials{AccessKeyID: "synthetic", SecretAccessKey: "synthetic"}, nil
						}),
					})
					tool := newTestSearchTool(&fakeProvider{name: "aws", services: []common.ServiceType{service}, recClient: adapter})
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
					args := map[string]any{"provider": "aws", "service": string(service)}
					wantRequests := []recommendationCompletenessRequest{
						{Term: "ONE_YEAR", Payment: "ALL_UPFRONT", Scope: "LINKED"}, {Term: "ONE_YEAR", Payment: "PARTIAL_UPFRONT", Scope: "LINKED"}, {Term: "ONE_YEAR", Payment: "NO_UPFRONT", Scope: "LINKED"},
						{Term: "THREE_YEARS", Payment: "ALL_UPFRONT", Scope: "LINKED"}, {Term: "THREE_YEARS", Payment: "PARTIAL_UPFRONT", Scope: "LINKED"}, {Term: "THREE_YEARS", Payment: "NO_UPFRONT", Scope: "LINKED"},
					}
					if selected {
						args["term_years"], args["payment_option"] = 3, "no-upfront"
						wantRequests = wantRequests[5:]
					}
					if common.IsSavingsPlan(service) {
						planTypes := map[common.ServiceType][]string{
							common.ServiceSavingsPlansCompute: {"COMPUTE_SP"}, common.ServiceSavingsPlansEC2Instance: {"EC2_INSTANCE_SP"},
							common.ServiceSavingsPlansSageMaker: {"SAGEMAKER_SP"}, common.ServiceSavingsPlansDatabase: {"DATABASE_SP"},
							common.ServiceSavingsPlansAll: {"COMPUTE_SP", "EC2_INSTANCE_SP", "SAGEMAKER_SP", "DATABASE_SP"},
						}[service]
						term, payment, lookback := "ONE_YEAR", "NO_UPFRONT", "THIRTY_DAYS"
						if selected {
							term, payment, lookback = "THREE_YEARS", "PARTIAL_UPFRONT", "SIXTY_DAYS"
							args["payment_option"], args["lookback_period"] = "partial-upfront", "60d"
						}
						wantRequests = make([]recommendationCompletenessRequest, 0, len(planTypes)+1)
						for _, planType := range planTypes {
							request := recommendationCompletenessRequest{Term: term, Payment: payment, PlanType: planType, Lookback: lookback, Scope: "LINKED"}
							wantRequests = append(wantRequests, request)
							if kind == "late-page-failure" && planType == planTypes[0] {
								request.Token = "page-two"
								wantRequests = append(wantRequests, request)
							}
						}
					}
					result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: searchRecommendationsName, Arguments: args})
					require.NoError(t, err)
					fixture.mu.Lock()
					defer fixture.mu.Unlock()
					require.Empty(t, fixture.unexpected)
					if kind == "all-invalid" && service == common.ServiceRDS {
						wantRequests = wantRequests[:1]
					}
					assert.Equal(t, wantRequests, fixture.requests)
					if common.IsSavingsPlan(service) && kind != "valid" && kind != "empty" {
						require.True(t, result.IsError, "incomplete SP collection produced a menu: %+v", result.StructuredContent)
						require.Nil(t, result.StructuredContent)
						var diagnostic strings.Builder
						for _, content := range result.Content {
							part, ok := content.(*mcp.TextContent)
							require.True(t, ok)
							diagnostic.WriteString(part.Text)
						}
						fatal := kind == "all-type-failure" || (kind == "partial-type-failure" && service != common.ServiceSavingsPlansAll)
						if fatal {
							assert.Contains(t, diagnostic.String(), "fixture API failure")
							assert.NotContains(t, diagnostic.String(), "failed details")
						} else {
							details, scopes := 0, 1
							if kind == "mixed" || kind == "all-invalid" {
								details, scopes = len(wantRequests), 0
								assert.Contains(t, diagnostic.String(), "page 0 detail")
								assert.Contains(t, diagnostic.String(), "not-a-number")
							}
							assert.Regexp(t, fmt.Sprintf(`\b%d failed details\b`, details), diagnostic.String())
							assert.Regexp(t, fmt.Sprintf(`\b%d failed scopes\b`, scopes), diagnostic.String())
							if kind == "late-page-failure" {
								assert.Contains(t, diagnostic.String(), "page 1")
							}
						}
						if !fatal {
							failedRequest := wantRequests[0]
							assert.Contains(t, diagnostic.String(), failedRequest.PlanType)
							term, payment := "1yr", "no-upfront"
							if selected {
								term, payment = "3yr", "partial-upfront"
							}
							assert.Contains(t, diagnostic.String(), "term "+term)
							assert.Contains(t, diagnostic.String(), "payment "+payment)
						}
						return
					}
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
}

type recommendationCompletenessRequest struct {
	Term     string `json:"TermInYears"`
	Payment  string `json:"PaymentOption"`
	PlanType string `json:"SavingsPlansType,omitempty"`
	Lookback string `json:"LookbackPeriodInDays,omitempty"`
	Scope    string `json:"AccountScope,omitempty"`
	Token    string `json:"NextPageToken,omitempty"`
}

type recommendationCompletenessHTTP struct {
	mu         sync.Mutex
	kind       string
	requests   []recommendationCompletenessRequest
	unexpected []string
}

func (f *recommendationCompletenessHTTP) Do(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	operation := req.Header.Get("X-Amz-Target")
	if req.URL.Host != "ce.us-east-1.amazonaws.com" || (operation != "AWSInsightsIndexService.GetReservationPurchaseRecommendation" && operation != "AWSInsightsIndexService.GetSavingsPlansPurchaseRecommendation") {
		f.unexpected = append(f.unexpected, operation)
		return nil, fmt.Errorf("unexpected SDK operation: %s", operation)
	}
	var params recommendationCompletenessRequest
	if err := json.NewDecoder(req.Body).Decode(&params); err != nil {
		return nil, err
	}
	f.requests = append(f.requests, params)
	if operation == "AWSInsightsIndexService.GetSavingsPlansPurchaseRecommendation" {
		status := http.StatusOK
		detail := map[string]any{"AccountId": "survivor", "HourlyCommitmentToPurchase": "2", "EstimatedMonthlySavingsAmount": "10", "UpfrontCost": "3", "CurrentAverageHourlyOnDemandSpend": "4"}
		invalid := map[string]any{"HourlyCommitmentToPurchase": "not-a-number"}
		details := make([]map[string]any, 1, 2)
		details[0] = detail
		token := ""
		switch f.kind {
		case "empty":
			details = details[:0]
		case "mixed":
			details = append(details, invalid)
		case "all-invalid":
			details = []map[string]any{invalid}
		case "late-page-failure":
			if params.PlanType == f.requests[0].PlanType {
				token = "page-two"
				if params.Token != "" {
					status = http.StatusBadRequest
				}
			}
		case "partial-type-failure":
			if params.PlanType == f.requests[0].PlanType {
				status = http.StatusBadRequest
			}
		case "all-type-failure":
			status = http.StatusBadRequest
		}
		body := map[string]any{"NextPageToken": token, "SavingsPlansPurchaseRecommendation": map[string]any{"SavingsPlansPurchaseRecommendationDetails": details}}
		if status != http.StatusOK {
			body = map[string]any{"__type": "InvalidParameterValueException", "Message": "fixture API failure"}
		}
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/x-amz-json-1.1"}}, Body: io.NopCloser(strings.NewReader(string(encoded))), Request: req}, nil
	}
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
