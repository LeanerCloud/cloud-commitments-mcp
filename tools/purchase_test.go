package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
)

// TestMain enables the operator-side real-purchase gate for this package's
// test binary by default. Every other test in this file that drives a
// confirmed real purchase (DryRun: false, Confirm: true) predates
// EnvEnableRealPurchases and asserts on what happens once a purchase is
// actually authorized to run (provider called, token derived, response
// shaped correctly, etc) -- none of that is what TestExecutePurchaseRealPurchaseGate
// exists to cover, so defaulting the gate on here keeps them exercising the
// behavior they were written for instead of universally failing at the gate.
// TestExecutePurchaseRealPurchaseGate is the one test that deliberately
// overrides this default, and it does so non-parallel (see its doc comment)
// so no parallel test in this package ever observes a transient override.
//
// It also pins EnvAuditLog to a path inside a run-scoped temp directory.
// Auditing is on by default (see EnvAuditLog), so without this every test in
// this file that calls ExecutePurchase would append to the developer's real
// ~/.local/state/cudly/mcp-audit.jsonl. Individual tests that need to assert
// on audit file contents override this with their own t.Setenv.
func TestMain(m *testing.M) {
	if err := os.Setenv(EnvEnableRealPurchases, "1"); err != nil {
		log.Printf("enable real purchases for MCP tests: %v", err)
		os.Exit(1)
	}

	// Generous default caps so suites that predate the spend ceiling keep
	// exercising what happens once a purchase is authorized. The cap tests
	// override these (non-parallel) per case.
	for env, v := range map[string]string{EnvMaxCount: "1000000", EnvMaxHourlyCommitment: "1000000", EnvMaxMemoryGB: "1000000", EnvMaxCommitmentUSD: "1000000000", EnvBudgetUSD: "1000000000000"} {
		if err := os.Setenv(env, v); err != nil {
			log.Printf("set %s for MCP tests: %v", env, err)
			os.Exit(1)
		}
	}

	auditDir, err := os.MkdirTemp("", "cudly-mcp-audit-testmain")
	if err != nil {
		log.Printf("create MCP tool audit test directory: %v", err)
		os.Exit(1)
	}
	if err := os.Setenv(EnvAuditLog, filepath.Join(auditDir, "mcp-audit.jsonl")); err != nil {
		log.Printf("set MCP tool audit log test path: %v", err)
		if cleanupErr := os.RemoveAll(auditDir); cleanupErr != nil {
			log.Printf("remove MCP tool audit test directory after setup failure: %v", cleanupErr)
		}
		os.Exit(1)
	}

	// os.Exit skips deferred calls, so the temp dir is removed explicitly
	// before exiting rather than via defer.
	code := m.Run()
	os.RemoveAll(auditDir)
	os.Exit(code)
}

// fakeServiceClient is a minimal provider.ServiceClient test double. Only
// PurchaseCommitment is exercised by these tests; the rest of the interface
// is implemented trivially to satisfy the type.
type fakeServiceClient struct {
	purchaseErr    error
	offeringErr    error
	offering       *common.OfferingDetails
	lastOpts       common.PurchaseOptions
	purchaseResult common.PurchaseResult
	purchaseCalls  int
	offeringCalls  int
}

func (f *fakeServiceClient) GetServiceType() common.ServiceType { return common.ServiceEC2 }
func (f *fakeServiceClient) GetRegion() string                  { return "us-east-1" }
func (f *fakeServiceClient) GetRecommendations(_ context.Context, _ *common.RecommendationParams) ([]common.Recommendation, error) {
	return nil, nil
}
func (f *fakeServiceClient) GetExistingCommitments(_ context.Context) ([]common.Commitment, error) {
	return nil, nil
}
func (f *fakeServiceClient) PurchaseCommitment(_ context.Context, rec common.Recommendation, opts common.PurchaseOptions) (common.PurchaseResult, error) {
	f.purchaseCalls++
	f.lastOpts = opts
	f.purchaseResult.Recommendation = rec
	return f.purchaseResult, f.purchaseErr
}
func (f *fakeServiceClient) ValidateOffering(_ context.Context, _ common.Recommendation) error {
	return nil
}
func (f *fakeServiceClient) GetOfferingDetails(_ context.Context, _ common.Recommendation) (*common.OfferingDetails, error) {
	f.offeringCalls++
	if f.offering == nil && f.offeringErr == nil {
		return &common.OfferingDetails{TotalCost: 1, Currency: "USD"}, nil
	}
	return f.offering, f.offeringErr
}
func (f *fakeServiceClient) GetValidResourceTypes(_ context.Context) ([]string, error) {
	return nil, nil
}

var _ provider.ServiceClient = (*fakeServiceClient)(nil)

func captureStandardLogger(t *testing.T, fn func()) string {
	t.Helper()

	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	}()

	fn()
	return buf.String()
}

// testRecommendation mirrors what a real purchase tool's *FromArgs
// constructor actually builds: none of them populate
// OnDemandCost/CommitmentCost/EstimatedSavings/SavingsPercentage (they build
// a fresh Recommendation from the caller's typed args, not from a priced
// search result), so this fixture leaves those fields at their zero value
// too. An earlier version of this fixture hand-set those fields, which
// masked the all-responses-report-0 finding from review -- see
// TestExecutePurchasePreviewOmitsUnknownCostFields.
func testRecommendation() common.Recommendation {
	return common.Recommendation{
		Provider:      common.ProviderAWS,
		Account:       "123456789012",
		Service:       common.ServiceEC2,
		Region:        "us-east-1",
		ResourceType:  "m5.large",
		Count:         3,
		Term:          "3yr",
		PaymentOption: "no-upfront",
	}
}

// testRecommendationWithCost extends testRecommendation with real cost
// figures, used only to prove ExecutePurchase passes a genuinely-known cost
// through to the response when one is present.
func testRecommendationWithCost() common.Recommendation {
	rec := testRecommendation()
	rec.OnDemandCost = 1000
	rec.CommitmentCost = 600
	rec.EstimatedSavings = 400
	rec.SavingsPercentage = 40
	return rec
}

func TestDecidePurchaseMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		want    purchaseMode
		dryRun  bool
		confirm bool
		wantErr bool
	}{
		{name: "dry run wins regardless of confirm", dryRun: true, confirm: false, want: modePreview, wantErr: false},
		{name: "dry run with confirm still previews", dryRun: true, confirm: true, want: modePreview, wantErr: false},
		{name: "confirmed real purchase executes", dryRun: false, confirm: true, want: modeExecute, wantErr: false},
		{name: "unconfirmed real purchase refused", dryRun: false, confirm: false, want: 0, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decidePurchaseMode(tc.dryRun, tc.confirm)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestExecutePurchaseDryRunNeverCallsProvider proves the safety rail from
// the design doc: dry_run=true must never invoke ResolveClient (and
// therefore never PurchaseCommitment), even when confirm=true. ResolveClient
// here returns an error if called at all, so any invocation fails the test.
func TestExecutePurchaseDryRunNeverCallsProvider(t *testing.T) {
	t.Parallel()
	resolveCalled := false
	resolve := func(_ context.Context) (provider.ServiceClient, error) {
		resolveCalled = true
		return nil, errors.New("ResolveClient must not be called in dry_run mode")
	}

	resp, err := ExecutePurchase(context.Background(), PurchaseRequest{
		Region:         "us-east-1",
		Recommendation: testRecommendationWithCost(),
		DryRun:         true,
		Confirm:        true, CredentialScope: "test-scope",
		ResolveClient: resolve,
	})

	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.False(t, resolveCalled, "dry_run=true must never resolve a service client")
	assert.True(t, resp.DryRun)
	assert.True(t, resp.Success)
	require.NotNil(t, resp.Cost, "a genuinely-known cost must be passed through, not dropped")
	assert.Equal(t, 600.0, *resp.Cost)
	require.NotNil(t, resp.OnDemandCost)
	assert.Equal(t, 1000.0, *resp.OnDemandCost)
	require.NotNil(t, resp.EstimatedSavings)
	assert.Equal(t, 400.0, *resp.EstimatedSavings)
	require.NotNil(t, resp.SavingsPercentage)
	assert.Equal(t, 40.0, *resp.SavingsPercentage)
}

// TestExecutePurchasePreviewPopulatesTermYears is the regression guard for
// the CodeRabbit finding that PurchaseResponse.TermYears was declared in the
// JSON contract but never set in either ExecutePurchase branch, so it was
// always zero/omitted even though the term is known from the recommendation.
// testRecommendation() carries Term: "3yr", the same "<N>yr" format every
// *FromArgs constructor in this package writes.
func TestExecutePurchasePreviewPopulatesTermYears(t *testing.T) {
	t.Parallel()
	resp, err := ExecutePurchase(context.Background(), PurchaseRequest{
		Region:         "us-east-1",
		Recommendation: testRecommendation(),
		DryRun:         true,
		Confirm:        false,
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, 3, resp.TermYears, "a preview response must carry the term the caller specified")
}

// TestExecutePurchaseRealPurchasePopulatesTermYears is the real-purchase
// counterpart of TestExecutePurchasePreviewPopulatesTermYears: the term must
// be populated on the modeExecute branch too, not only the preview branch.
func TestExecutePurchaseRealPurchasePopulatesTermYears(t *testing.T) {
	t.Parallel()
	fake := &fakeServiceClient{
		purchaseResult: common.PurchaseResult{Success: true, CommitmentID: "ri-term-test"},
	}

	resp, err := ExecutePurchase(context.Background(), PurchaseRequest{
		Region:         "us-east-1",
		Recommendation: testRecommendation(),
		DryRun:         false,
		Confirm:        true, CredentialScope: "test-scope",
		ResolveClient: func(_ context.Context) (provider.ServiceClient, error) { return fake, nil },
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, 3, resp.TermYears, "a real-purchase response must carry the term the caller specified")
}

// TestExecutePurchaseUnconfirmedRealPurchaseRefused proves confirm=false
// refuses a real purchase (dry_run=false) with a structured error rather
// than a silent no-op, and that ResolveClient is never invoked either.
func TestExecutePurchaseUnconfirmedRealPurchaseRefused(t *testing.T) {
	t.Parallel()
	resolveCalled := false
	resolve := func(_ context.Context) (provider.ServiceClient, error) {
		resolveCalled = true
		return nil, nil
	}

	resp, err := ExecutePurchase(context.Background(), PurchaseRequest{
		Region:         "us-east-1",
		Recommendation: testRecommendation(),
		DryRun:         false,
		Confirm:        false,
		ResolveClient:  resolve,
	})

	require.Error(t, err)
	assert.Nil(t, resp)
	assert.False(t, resolveCalled)
	assert.Contains(t, err.Error(), "confirm=true")
}

// TestExecutePurchaseRealPurchaseCallsProviderWithMCPSource proves a
// confirmed real purchase resolves the client, calls PurchaseCommitment
// exactly once, and stamps PurchaseSourceMCP + a non-empty idempotency
// token -- never a caller-suppliable source string.
func TestExecutePurchaseRealPurchaseCallsProviderWithMCPSource(t *testing.T) {
	t.Parallel()
	cost := 600.0
	fake := &fakeServiceClient{
		purchaseResult: common.PurchaseResult{
			Success:      true,
			CommitmentID: "ri-12345",
			Cost:         &cost,
		},
	}
	resolve := func(_ context.Context) (provider.ServiceClient, error) {
		return fake, nil
	}

	resp, err := ExecutePurchase(context.Background(), PurchaseRequest{
		Region:         "us-east-1",
		Recommendation: testRecommendation(),
		DryRun:         false,
		Confirm:        true, CredentialScope: "test-scope",
		ResolveClient: resolve,
	})

	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, 1, fake.purchaseCalls)
	assert.Equal(t, common.PurchaseSourceMCP, fake.lastOpts.Source)
	assert.NotEmpty(t, fake.lastOpts.IdempotencyToken)
	assert.True(t, resp.Success)
	assert.Equal(t, "ri-12345", resp.CommitmentID)
	assert.False(t, resp.DryRun)
}

// TestExecutePurchaseUnsetTimestampOmitsEffectiveDate is the regression guard
// for the fabricated start date found in review. common.PurchaseResult's
// Timestamp is a plain time.Time that not every provider client populates,
// and EffectiveDate used to be a plain string set from
// result.Timestamp.Format(time.RFC3339). Formatting the zero time.Time
// yields the literal "0001-01-01T00:00:00Z" rather than "", so `omitempty`
// could never drop it and every such response advertised a real-looking
// commitment start date in the year 1 -- a value the provider never
// reported, on a field a caller may key billing or renewal reminders off.
//
// The assertion is made against the marshaled JSON, not just the Go field,
// because the JSON payload is what actually crosses the MCP boundary to the
// caller.
func TestExecutePurchaseUnsetTimestampOmitsEffectiveDate(t *testing.T) {
	t.Parallel()
	// Timestamp deliberately left unset, exactly as a provider client that
	// never populates it leaves it.
	fake := &fakeServiceClient{
		purchaseResult: common.PurchaseResult{Success: true, CommitmentID: "ri-no-timestamp"},
	}

	resp, err := ExecutePurchase(context.Background(), PurchaseRequest{
		Region:         "us-east-1",
		Recommendation: testRecommendation(),
		DryRun:         false,
		Confirm:        true, CredentialScope: "test-scope",
		ResolveClient: func(_ context.Context) (provider.ServiceClient, error) { return fake, nil },
	})

	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Nil(t, resp.EffectiveDate,
		"an unset provider timestamp must stay unknown, not become a formatted zero time")

	payload, err := json.Marshal(resp)
	require.NoError(t, err)
	assert.NotContains(t, string(payload), "0001-01-01",
		"the zero time.Time must never reach the caller as a start date: %s", payload)
	assert.NotContains(t, string(payload), "effective_date",
		"effective_date must be omitted entirely when the provider reported none: %s", payload)
}

// TestExecutePurchaseRealTimestampIsReported is the other half of the guard
// above: suppressing the zero value must not suppress a genuine one.
func TestExecutePurchaseRealTimestampIsReported(t *testing.T) {
	t.Parallel()
	stamp := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	fake := &fakeServiceClient{
		purchaseResult: common.PurchaseResult{Success: true, CommitmentID: "ri-stamped", Timestamp: stamp},
	}

	resp, err := ExecutePurchase(context.Background(), PurchaseRequest{
		Region:         "us-east-1",
		Recommendation: testRecommendation(),
		DryRun:         false,
		Confirm:        true, CredentialScope: "test-scope",
		ResolveClient: func(_ context.Context) (provider.ServiceClient, error) { return fake, nil },
	})

	require.NoError(t, err)
	require.NotNil(t, resp)
	require.NotNil(t, resp.EffectiveDate, "a populated provider timestamp must still be reported")
	assert.Equal(t, stamp.Format(time.RFC3339), *resp.EffectiveDate)
}

// TestExecutePurchaseProviderErrorSurfaced proves a provider-side purchase
// failure surfaces the full underlying error text rather than being
// swallowed.
func TestExecutePurchaseProviderErrorSurfaced(t *testing.T) {
	t.Parallel()
	fake := &fakeServiceClient{purchaseErr: errors.New("AWS API: InsufficientInstanceCapacity")}
	resolve := func(_ context.Context) (provider.ServiceClient, error) { return fake, nil }

	resp, err := ExecutePurchase(context.Background(), PurchaseRequest{
		Region:         "us-east-1",
		Recommendation: testRecommendation(),
		DryRun:         false,
		Confirm:        true, CredentialScope: "test-scope",
		ResolveClient: resolve,
	})

	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "InsufficientInstanceCapacity")
}

// TestExecutePurchaseResolveClientErrorSurfaced proves a client-resolution
// failure (e.g. bad credentials) surfaces its error text too.
func TestExecutePurchaseResolveClientErrorSurfaced(t *testing.T) {
	t.Parallel()
	resolve := func(_ context.Context) (provider.ServiceClient, error) {
		return nil, errors.New("no AWS credentials found")
	}

	resp, err := ExecutePurchase(context.Background(), PurchaseRequest{
		Region:         "us-east-1",
		Recommendation: testRecommendation(),
		DryRun:         false,
		Confirm:        true, CredentialScope: "test-scope",
		ResolveClient: resolve,
	})

	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "no AWS credentials found")
}

// TestResolveDryRunConfirm pins the shared default resolution now used by
// every purchase tool. This is the gate that decides whether real money
// moves, and it was previously hand-copied into seven files; the single
// most important property is that an OMITTED dry_run means preview, never
// execute.
func TestResolveDryRunConfirm(t *testing.T) {
	t.Parallel()
	ptr := func(b bool) *bool { return &b }

	cases := []struct {
		dryRun      *bool
		confirm     *bool
		name        string
		wantDryRun  bool
		wantConfirm bool
	}{
		{name: "both omitted defaults to preview", dryRun: nil, confirm: nil, wantDryRun: true, wantConfirm: false},
		// The safety-critical row: confirm=true alone must NOT execute.
		// decidePurchaseMode then sees dryRun=true and previews.
		{name: "omitted dry_run stays preview even when confirmed", dryRun: nil, confirm: ptr(true), wantDryRun: true, wantConfirm: true},
		{name: "explicit false dry_run is honored", dryRun: ptr(false), confirm: ptr(true), wantDryRun: false, wantConfirm: true},
		{name: "explicit true dry_run is honored", dryRun: ptr(true), confirm: ptr(false), wantDryRun: true, wantConfirm: false},
		{name: "explicit false confirm is honored", dryRun: ptr(false), confirm: ptr(false), wantDryRun: false, wantConfirm: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotDryRun, gotConfirm := ResolveDryRunConfirm(tc.dryRun, tc.confirm)
			assert.Equal(t, tc.wantDryRun, gotDryRun)
			assert.Equal(t, tc.wantConfirm, gotConfirm)
		})
	}
}
