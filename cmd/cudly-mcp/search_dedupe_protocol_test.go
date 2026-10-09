package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
	awsprovider "github.com/LeanerCloud/cloud-commitments-go/providers/aws"

	cudlymcp "github.com/LeanerCloud/cloud-commitments-mcp"

	gosdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const searchCallerAccount = "111111111111"

func searchProtocolRec() common.Recommendation {
	return common.Recommendation{
		Provider: common.ProviderAWS, Service: common.ServiceRDS, Account: searchCallerAccount, Region: "us-east-1",
		ResourceType: "db.r6g.large", Count: 2, Term: "1yr", PaymentOption: "all-upfront",
		Details: &common.DatabaseDetails{Engine: "MySQL", AZConfig: "multi-az"},
	}
}

// searchRecsClient serves a fixed recommendation list.
type searchRecsClient struct{ provider.RecommendationsClient }

func (searchRecsClient) GetRecommendations(_ context.Context, _ *common.RecommendationParams) ([]common.Recommendation, error) {
	return []common.Recommendation{searchProtocolRec()}, nil
}

// recentRDSClient lists one recent reservation that covers the recommendation.
type recentRDSClient struct{ provider.ServiceClient }

func (recentRDSClient) GetExistingCommitments(_ context.Context) ([]common.Commitment, error) {
	return []common.Commitment{{
		Provider: common.ProviderAWS, Service: common.ServiceRelationalDB, Region: "us-east-1", ResourceType: "db.r6g.large",
		Engine: "mysql", Deployment: "multi-az", Count: 2, State: common.CommitmentStateActive,
		StartDate: time.Now().Add(-time.Hour),
	}}, nil
}

type fakeSearchAWSProvider struct{}

func (fakeSearchAWSProvider) Name() string        { return "aws" }
func (fakeSearchAWSProvider) DisplayName() string { return "Amazon Web Services" }
func (fakeSearchAWSProvider) IsConfigured() bool  { return true }
func (fakeSearchAWSProvider) GetCredentials() (provider.Credentials, error) {
	return nil, errors.New("not implemented")
}
func (fakeSearchAWSProvider) ValidateCredentials(_ context.Context) error {
	return errors.New("not implemented")
}
func (fakeSearchAWSProvider) GetAccounts(_ context.Context) ([]common.Account, error) {
	return []common.Account{{ID: searchCallerAccount, IsDefault: true}}, nil
}
func (fakeSearchAWSProvider) GetRegions(_ context.Context) ([]common.Region, error) {
	return nil, errors.New("not implemented")
}
func (fakeSearchAWSProvider) GetDefaultRegion() string { return "us-east-1" }
func (fakeSearchAWSProvider) GetSupportedServices() []common.ServiceType {
	return []common.ServiceType{common.ServiceRDS}
}
func (fakeSearchAWSProvider) GetServiceClient(_ context.Context, service common.ServiceType, _ string) (provider.ServiceClient, error) {
	if service != common.ServiceRDS {
		return nil, errors.New("only rds is implemented")
	}
	return recentRDSClient{}, nil
}
func (fakeSearchAWSProvider) GetRecommendationsClient(_ context.Context) (provider.RecommendationsClient, error) {
	return searchRecsClient{}, nil
}

// TestSearchSuppressesCoveredRecommendationOverTheProtocol drives the search
// through the real server and an in-memory MCP client, with a fake aws
// provider: a recommendation covered by a recent reservation must leave the
// result and appear in dedupe.suppressed, and the structured output must pass
// the SDK's output-schema validation.
func TestSearchSuppressesCoveredRecommendationOverTheProtocol(t *testing.T) {
	provider.GetRegistry().Unregister("aws")
	require.NoError(t, provider.RegisterProvider("aws", func(_ *provider.ProviderConfig) (provider.Provider, error) {
		return fakeSearchAWSProvider{}, nil
	}))
	t.Cleanup(func() {
		provider.GetRegistry().Unregister("aws")
		if err := provider.RegisterProvider("aws", func(cfg *provider.ProviderConfig) (provider.Provider, error) {
			return awsprovider.NewAWSProvider(cfg)
		}); err != nil {
			t.Errorf("restore real aws provider factory: %v", err)
		}
	})

	server, err := cudlymcp.NewServer("test-search-dedupe")
	require.NoError(t, err)
	clientTransport, serverTransport := gosdk.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { _ = server.Run(ctx, serverTransport) }()

	client := gosdk.NewClient(&gosdk.Implementation{Name: "test-client"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	defer session.Close()

	result, err := session.CallTool(ctx, &gosdk.CallToolParams{
		Name: "cudly_search_recommendations",
		Arguments: map[string]any{
			"provider": "aws", "service": "rds", "region": "us-east-1", "term_years": 1, "payment_option": "all-upfront",
		},
	})
	require.NoError(t, err)
	require.False(t, result.IsError, "%+v", result.Content)

	encoded, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	var out struct {
		Dedupe struct {
			Suppressed []map[string]any `json:"suppressed"`
		} `json:"dedupe"`
		Recommendations []any `json:"recommendations"`
		Count           int   `json:"count"`
	}
	require.NoError(t, json.Unmarshal(encoded, &out), "%s", encoded)
	assert.Equal(t, 0, out.Count, "%s", encoded)
	assert.Empty(t, out.Recommendations, "%s", encoded)
	require.Len(t, out.Dedupe.Suppressed, 1, "%s", encoded)
	assert.Equal(t, searchCallerAccount, out.Dedupe.Suppressed[0]["account"])
}
