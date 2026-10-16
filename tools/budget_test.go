package tools

import (
	"context"
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
