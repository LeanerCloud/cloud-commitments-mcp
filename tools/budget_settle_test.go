package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
)

// hookClient lets a test run code inside PurchaseCommitment.
type hookClient struct {
	*fakeServiceClient
	onPurchase func(context.Context) (common.PurchaseResult, error)
}

func (c hookClient) PurchaseCommitment(ctx context.Context, _ common.Recommendation, _ common.PurchaseOptions) (common.PurchaseResult, error) {
	return c.onPurchase(ctx)
}

func runWithContext(ctx context.Context, fake provider.ServiceClient) error {
	rec := testRecommendation()
	rec.Count = 1
	_, err := ExecutePurchase(ctx, PurchaseRequest{
		Region: rec.Region, Recommendation: rec, Confirm: true, CredentialScope: "scope", Nonce: "n",
		ResolveClient: func(context.Context) (provider.ServiceClient, error) { return fake, nil },
	})
	return err
}

// A panic inside the provider call must leave the reservation held: the buy
// may have gone through, so the budget fails closed.
func TestBudgetPanicInPurchaseStaysReserved(t *testing.T) {
	resetBudget(t, "100")
	fake := hookClient{pricedFake(60, common.PurchaseResult{}, nil), func(context.Context) (common.PurchaseResult, error) {
		panic("provider blew up")
	}}
	require.Panics(t, func() { _ = runWithContext(context.Background(), fake) })

	processBudget.mu.Lock()
	reserved := processBudget.reserved
	processBudget.mu.Unlock()
	assert.InDelta(t, 60, reserved, 1e-9)

	other := pricedFake(60, common.PurchaseResult{Success: true, CommitmentID: "id"}, nil)
	require.ErrorContains(t, runWithNonce(other, "b"), EnvBudgetUSD)
	assert.Zero(t, other.purchaseCalls)
}

// Canceling the context while the provider call is in flight is an error
// outcome: the buy may have happened, so it stays counted.
func TestBudgetCancelDuringPurchaseStaysCounted(t *testing.T) {
	resetBudget(t, "100")
	ctx, cancel := context.WithCancel(context.Background())
	fake := hookClient{pricedFake(60, common.PurchaseResult{}, nil), func(ctx context.Context) (common.PurchaseResult, error) {
		cancel()
		return common.PurchaseResult{}, ctx.Err()
	}}
	require.ErrorIs(t, runWithContext(ctx, fake), context.Canceled)
	assert.InDelta(t, 60, processBudget.committedUSD(), 1e-9)

	other := pricedFake(60, common.PurchaseResult{Success: true, CommitmentID: "id"}, nil)
	require.ErrorContains(t, runWithNonce(other, "b"), EnvBudgetUSD)
}

// A context canceled before the provider call: pricing ignores it, the budget
// is reserved, and the provider call returns the cancellation. Whether it
// bought anything is unknown, so the reservation stays counted.
func TestBudgetCanceledBeforeProviderCallStaysCounted(t *testing.T) {
	resetBudget(t, "100")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fake := hookClient{pricedFake(60, common.PurchaseResult{}, nil), func(ctx context.Context) (common.PurchaseResult, error) {
		return common.PurchaseResult{}, ctx.Err()
	}}
	require.ErrorIs(t, runWithContext(ctx, fake), context.Canceled)
	assert.InDelta(t, 60, processBudget.committedUSD(), 1e-9)
	processBudget.mu.Lock()
	defer processBudget.mu.Unlock()
	assert.Zero(t, processBudget.reserved)
}
