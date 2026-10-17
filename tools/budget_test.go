package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
)

// resetBudget gives the test a fresh process ledger and a budget limit.
func resetBudget(t *testing.T, limit string) {
	t.Helper()
	t.Setenv(EnvBudgetUSD, limit)
	t.Setenv(EnvMaxCommitmentUSD, "1000000")
	processBudget = &budgetLedger{}
	t.Cleanup(func() { processBudget = &budgetLedger{} })
}

// lockedClient serializes the non-thread-safe fake for concurrent tests.
type lockedClient struct {
	*fakeServiceClient
	mu *sync.Mutex
}

func (c lockedClient) GetOfferingDetails(ctx context.Context, rec common.Recommendation) (*common.OfferingDetails, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fakeServiceClient.GetOfferingDetails(ctx, rec)
}

func (c lockedClient) PurchaseCommitment(ctx context.Context, rec common.Recommendation, opts common.PurchaseOptions) (common.PurchaseResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fakeServiceClient.PurchaseCommitment(ctx, rec, opts)
}

func pricedFake(usd float64, result common.PurchaseResult, err error) *fakeServiceClient {
	return &fakeServiceClient{
		purchaseResult: result,
		purchaseErr:    err,
		offering:       &common.OfferingDetails{TotalCost: usd, Currency: "USD"},
	}
}

func runWithNonce(fake provider.ServiceClient, nonce string) error {
	rec := testRecommendation()
	rec.Count = 1
	_, err := ExecutePurchase(context.Background(), PurchaseRequest{
		Region: rec.Region, Recommendation: rec, Confirm: true, CredentialScope: "scope", Nonce: nonce,
		ResolveClient: func(context.Context) (provider.ServiceClient, error) { return fake, nil },
	})
	return err
}

func TestBudgetRepeatCallWithFreshNonceRefused(t *testing.T) {
	resetBudget(t, "100")
	fake := pricedFake(60, common.PurchaseResult{Success: true, CommitmentID: "id"}, nil)
	require.NoError(t, runWithNonce(fake, "a"))
	err := runWithNonce(fake, "b")
	require.ErrorContains(t, err, EnvBudgetUSD)
	require.ErrorContains(t, err, "resets on restart")
	assert.Equal(t, 1, fake.purchaseCalls)
}

func TestBudgetConcurrentReservations(t *testing.T) {
	resetBudget(t, "100")
	fake := pricedFake(30, common.PurchaseResult{Success: true, CommitmentID: "id"}, nil)
	client := lockedClient{fake, &sync.Mutex{}}
	var ok atomic.Int32
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if runWithNonce(client, "") == nil {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	assert.EqualValues(t, 3, ok.Load())
	assert.Equal(t, 3, fake.purchaseCalls)
}

func TestBudgetFailedPurchaseKeepsCounted(t *testing.T) {
	resetBudget(t, "100")
	fake := pricedFake(60, common.PurchaseResult{}, assert.AnError)
	require.Error(t, runWithNonce(fake, "a"))
	assert.InDelta(t, 60, processBudget.committedUSD(), 1e-9)
	require.ErrorContains(t, runWithNonce(fake, "b"), EnvBudgetUSD)
	assert.Equal(t, 1, fake.purchaseCalls)
}

func TestBudgetCleanNoopReleases(t *testing.T) {
	resetBudget(t, "100")
	fake := pricedFake(60, common.PurchaseResult{}, nil)
	_ = runWithNonce(fake, "a")
	assert.Zero(t, processBudget.committedUSD())
	_ = runWithNonce(fake, "b")
	assert.Equal(t, 2, fake.purchaseCalls)
}

func TestBudgetExistingCommitmentReleases(t *testing.T) {
	resetBudget(t, "100")
	fake := pricedFake(60, common.PurchaseResult{Success: true, CommitmentID: "id", ExistingCommitment: true}, nil)
	require.NoError(t, runWithNonce(fake, "a"))
	assert.Zero(t, processBudget.committedUSD())
	require.NoError(t, runWithNonce(fake, "a"))
}

// TestBudgetExistingCommitmentWithErrorKeepsCounted pins the ordering inside
// purchaseSpent: an error outranks the adopted-commitment result field, so a
// provider that reports ExistingCommitment AND fails still keeps its
// reservation counted. No provider is known to return that combination, but
// the error case is the one that may have spent money.
func TestBudgetExistingCommitmentWithErrorKeepsCounted(t *testing.T) {
	resetBudget(t, "100")
	fake := pricedFake(60, common.PurchaseResult{ExistingCommitment: true}, assert.AnError)
	require.Error(t, runWithNonce(fake, "a"))
	assert.InDelta(t, 60, processBudget.committedUSD(), 1e-9)
	require.ErrorContains(t, runWithNonce(fake, "b"), EnvBudgetUSD)
	assert.Equal(t, 1, fake.purchaseCalls)
}

// reservedUSD reads the in-flight reservation total, so a test can prove a
// settle released it rather than leaving it held.
func reservedUSD() float64 {
	processBudget.mu.Lock()
	defer processBudget.mu.Unlock()
	return processBudget.reserved
}

// panickingClient panics from PurchaseCommitment, standing in for an SDK bug
// or a nil dereference inside a provider client. The panic value is a string
// so a test can also prove it never reaches the caller.
type panickingClient struct{ *fakeServiceClient }

func (c panickingClient) PurchaseCommitment(context.Context, common.Recommendation, common.PurchaseOptions) (common.PurchaseResult, error) {
	panic("provider sdk exploded")
}

// cancellingClient cancels the request context from inside
// PurchaseCommitment and returns its error, standing in for a provider call
// that observes a cancellation after the reservation was taken.
type cancellingClient struct {
	*fakeServiceClient
	cancel context.CancelFunc
}

func (c cancellingClient) PurchaseCommitment(ctx context.Context, _ common.Recommendation, _ common.PurchaseOptions) (common.PurchaseResult, error) {
	c.cancel()
	return common.PurchaseResult{}, ctx.Err()
}

// TestBudgetPanicInPurchaseKeepsCountedAndServerAlive pins that a panic
// inside the provider call is recovered at the call site: the process
// survives, the reservation settles as spent (the money may already have left
// the account), a second purchase is refused, and the audit trail records the
// unknown outcome. The panic value itself must not reach the caller, since it
// can carry SDK internals.
func TestBudgetPanicInPurchaseKeepsCountedAndServerAlive(t *testing.T) {
	resetBudget(t, "100")
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	t.Setenv(EnvAuditLog, path)
	client := panickingClient{&fakeServiceClient{offering: &common.OfferingDetails{TotalCost: 60, Currency: "USD"}}}

	err := runWithNonce(client, "a")
	require.Error(t, err)
	assert.ErrorContains(t, err, "outcome unknown")
	assert.NotContains(t, err.Error(), "provider sdk exploded")
	assert.InDelta(t, 60, processBudget.committedUSD(), 1e-9)
	assert.Zero(t, reservedUSD())
	require.ErrorContains(t, runWithNonce(client, "b"), EnvBudgetUSD)

	lines := readAuditLines(t, path)
	require.Len(t, lines, 1)
	var rec struct {
		Status string `json:"status"`
	}
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &rec))
	assert.Equal(t, auditStatusUnknown, rec.Status)
}

// TestBudgetCancelledContextBeforePurchaseCountsNothing pins error
// propagation and accounting when the call never reaches the provider: the
// already-cancelled context plus an unpriceable offering means nothing was
// reserved, so nothing is counted. The fake ignores the context entirely, so
// this does NOT show that a provider honours cancellation; only the
// unpriceable-offering error stops this call.
func TestBudgetCancelledContextBeforePurchaseCountsNothing(t *testing.T) {
	resetBudget(t, "100")
	fake := &fakeServiceClient{
		offeringErr:    context.Canceled,
		purchaseResult: common.PurchaseResult{Success: true, CommitmentID: "id"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rec := testRecommendation()
	rec.Count = 1
	_, err := ExecutePurchase(ctx, PurchaseRequest{
		Region: rec.Region, Recommendation: rec, Confirm: true, CredentialScope: "scope",
		ResolveClient: func(context.Context) (provider.ServiceClient, error) { return fake, nil },
	})
	require.Error(t, err)
	assert.Zero(t, processBudget.committedUSD())
	assert.Zero(t, reservedUSD())
	assert.Zero(t, fake.purchaseCalls)
}

// TestBudgetCancellationDuringPurchaseKeepsCounted pins that a cancellation
// observed after the reservation was taken still counts as spent: the call
// reached the provider and its outcome is unknown from here.
func TestBudgetCancellationDuringPurchaseKeepsCounted(t *testing.T) {
	resetBudget(t, "100")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := cancellingClient{&fakeServiceClient{offering: &common.OfferingDetails{TotalCost: 60, Currency: "USD"}}, cancel}

	rec := testRecommendation()
	rec.Count = 1
	_, err := ExecutePurchase(ctx, PurchaseRequest{
		Region: rec.Region, Recommendation: rec, Confirm: true, CredentialScope: "scope",
		ResolveClient: func(context.Context) (provider.ServiceClient, error) { return client, nil },
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.InDelta(t, 60, processBudget.committedUSD(), 1e-9)
	assert.Zero(t, reservedUSD())
}

func TestBudgetUnsetRefusesBeforeResolve(t *testing.T) {
	resetBudget(t, "")
	fake := &fakeServiceClient{}
	resolved, err := runRealWith(fake, testRecommendation())
	require.ErrorContains(t, err, EnvBudgetUSD)
	assert.False(t, resolved)
}

func TestBudgetEnvParsing(t *testing.T) {
	for _, v := range []string{"", " ", "abc", "NaN", "Inf", "-1", "0"} {
		t.Setenv(EnvBudgetUSD, v)
		_, err := readBudget()
		require.ErrorContains(t, err, EnvBudgetUSD, "value %q", v)
	}
	t.Setenv(EnvBudgetUSD, "1e3")
	got, err := readBudget()
	require.NoError(t, err)
	assert.InDelta(t, 1000, got, 1e-9)
}

func TestBudgetEqualToLimitAllowed(t *testing.T) {
	resetBudget(t, "100")
	fake := pricedFake(60, common.PurchaseResult{Success: true, CommitmentID: "id"}, nil)
	require.NoError(t, runWithNonce(fake, "a"))
	fake.offering.TotalCost = 40
	require.NoError(t, runWithNonce(fake, "b"))
	fake.offering.TotalCost = 0.01
	require.ErrorContains(t, runWithNonce(fake, "c"), EnvBudgetUSD)
	assert.Equal(t, 2, fake.purchaseCalls)
}
