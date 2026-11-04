package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// allUpfrontRDSFake is a fresh fake serving one exact-match All Upfront USD
// offering whose FixedPrice (per instance) is price.
func allUpfrontRDSFake(price float64) *fakeRDSAPI {
	o := rdsOffering("off-all-upfront", "postgresql")
	o.OfferingType = aws.String("All Upfront")
	o.FixedPrice = aws.Float64(price)
	return &fakeRDSAPI{offerings: []rdstypes.ReservedDBInstancesOffering{o}}
}

// partialUpfrontRDSFake serves one Partial Upfront offering: FixedPrice plus
// a recurring hourly charge, which the guard must ignore.
func partialUpfrontRDSFake(price float64) *fakeRDSAPI {
	o := rdsOffering("off-partial-upfront", "postgresql")
	o.OfferingType = aws.String("Partial Upfront")
	o.FixedPrice = aws.Float64(price)
	o.RecurringCharges = []rdstypes.RecurringCharge{{RecurringChargeAmount: aws.Float64(0.05), RecurringChargeFrequency: aws.String("Hourly")}}
	return &fakeRDSAPI{offerings: []rdstypes.ReservedDBInstancesOffering{o}}
}

func guardRDSArgs(payment PaymentOption, nonce string, upfront *float64, currency string) rdsRIPurchaseArgs {
	args := upfrontRDSArgs(payment, nonce)
	args.Count = 1
	return confirmedRDS(args, upfront, currency)
}

// TestRDSUpfrontGuardBoundaryOnRealClient runs the real go RDS client's
// approved-upfront check (go#611): approved 1000 allows an offering up to
// 1050.00 (+5%); 1050.01 is refused before any purchase call.
func TestRDSUpfrontGuardBoundaryOnRealClient(t *testing.T) {
	fakes := map[PaymentOption]func(float64) *fakeRDSAPI{
		PaymentOptionAllUpfront:     allUpfrontRDSFake,
		PaymentOptionPartialUpfront: partialUpfrontRDSFake,
	}
	for payment, newFake := range fakes {
		for _, tc := range []struct {
			name    string
			wantErr string
			price   float64
			buys    bool
		}{
			{"at approved", "", 1000, true},
			{"at tolerance edge", "", 1050.00, true},
			{"just above tolerance", "exceeds the approved upfront", 1050.01, false},
		} {
			t.Run(string(payment)+"/"+tc.name, func(t *testing.T) {
				t.Setenv(EnvAuditLog, filepath.Join(t.TempDir(), "audit.jsonl"))
				fake := newFake(tc.price)
				_, resp, err := rdsToolWithFake(fake).handle(context.Background(), nil,
					guardRDSArgs(payment, "guard-"+string(payment)+tc.name, floatPtr(1000), "USD"))
				if tc.buys {
					require.NoError(t, err)
					assert.True(t, resp.Success)
					assert.Equal(t, 1, fake.purchaseCalls)
					return
				}
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.Zero(t, fake.purchaseCalls, "a refused guard must never reach Purchase")
			})
		}
	}
}

// TestRDSUpfrontDryRunQuotesRealClientOffering: the dry run prices the same
// offering through the real client and reports FixedPrice x count.
func TestRDSUpfrontDryRunQuotesRealClientOffering(t *testing.T) {
	t.Setenv(EnvAuditLog, filepath.Join(t.TempDir(), "audit.jsonl"))
	fake := allUpfrontRDSFake(525.5)
	args := upfrontRDSArgs(PaymentOptionAllUpfront, "dry-real")
	args.Count = 2
	_, resp, err := rdsToolWithFake(fake).handle(context.Background(), nil, args)
	require.NoError(t, err)
	require.NotNil(t, resp.ApprovedUpfront)
	assert.InDelta(t, 1051.0, *resp.ApprovedUpfront, 1e-9)
	assert.Equal(t, "USD", resp.ApprovedCurrency)
	assert.Zero(t, fake.purchaseCalls)
}

func TestRDSUpfrontUnusableFiguresNeverReachClient(t *testing.T) {
	t.Setenv(EnvAuditLog, filepath.Join(t.TempDir(), "audit.jsonl"))
	for name, tc := range map[string]struct {
		upfront  *float64
		currency string
	}{
		"zero":  {floatPtr(0), "USD"},
		"eur":   {floatPtr(1000), "EUR"},
		"unset": {nil, ""},
	} {
		t.Run(name, func(t *testing.T) {
			fake := allUpfrontRDSFake(1000)
			_, _, err := rdsToolWithFake(fake).handle(context.Background(), nil, guardRDSArgs(PaymentOptionAllUpfront, "unusable-"+name, tc.upfront, tc.currency))
			require.Error(t, err)
			assert.Zero(t, fake.describeCalls+fake.purchaseCalls)
		})
	}
}

// TestRDSUpfrontRoundTripOverMCP drives both calls through the MCP protocol:
// the schema accepts the new fields and its approved_currency enum is {USD}.
func TestRDSUpfrontRoundTripOverMCP(t *testing.T) {
	t.Setenv(EnvAuditLog, filepath.Join(t.TempDir(), "audit.jsonl"))
	fake := allUpfrontRDSFake(500)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server"}, nil)
	require.NoError(t, rdsToolWithFake(fake).Register(server))
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	require.NoError(t, err)
	defer serverSession.Close()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test-client"}, nil).Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	defer session.Close()

	tools, err := session.ListTools(ctx, nil)
	require.NoError(t, err)
	var schema []byte
	for _, tl := range tools.Tools {
		if tl.Name == awsRDSRIPurchaseName {
			schema, err = json.Marshal(tl.InputSchema)
			require.NoError(t, err)
		}
	}
	require.NotEmpty(t, schema)
	var parsed struct {
		Properties map[string]struct {
			Enum []any `json:"enum"`
		} `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(schema, &parsed))
	assert.Equal(t, []any{"USD"}, parsed.Properties["approved_currency"].Enum)

	call := func(args map[string]any) (map[string]any, error) {
		t.Helper()
		result, callErr := session.CallTool(ctx, &mcp.CallToolParams{Name: awsRDSRIPurchaseName, Arguments: args})
		require.NoError(t, callErr)
		if result.IsError {
			return nil, fmt.Errorf("%v", result.Content)
		}
		raw, marshalErr := json.Marshal(result.StructuredContent)
		require.NoError(t, marshalErr)
		var fields map[string]any
		require.NoError(t, json.Unmarshal(raw, &fields))
		return fields, nil
	}
	args := map[string]any{
		"region": "us-east-1", "instance_class": "db.r6g.large", "engine": "postgres", "az_config": "multi-az",
		"count": 2, "term_years": 3, "payment_option": "all-upfront", "aws_profile": "test-profile",
		"idempotency_nonce": "rds-mcp-round-trip",
	}
	preview, err := call(args)
	require.NoError(t, err)
	assert.Equal(t, 1000.0, preview["approved_upfront"])
	assert.Equal(t, "USD", preview["approved_currency"])

	args["dry_run"], args["confirm"] = false, true
	args["approved_upfront"], args["approved_currency"] = preview["approved_upfront"], "EUR"
	_, err = call(args)
	require.Error(t, err, "a non-USD currency is rejected")
	assert.Zero(t, fake.purchaseCalls)

	args["approved_currency"] = preview["approved_currency"]
	bought, err := call(args)
	require.NoError(t, err)
	assert.Equal(t, true, bought["success"])
	assert.NotContains(t, bought, "approved_upfront", "only the dry run reports the quote")
	assert.Equal(t, 1, fake.purchaseCalls)
}
