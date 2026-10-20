package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExecutePurchaseAuditLogging pins the MCP server's stderr diagnostic
// trail for a real purchase. The durable JSONL audit log is covered
// separately in audit_test.go.
//
// It also pins that a preview emits no purchase diagnostics on stderr and that
// the idempotency token is masked rather than written in full.
// recordPurchaseAudit still runs; TestUnwritablePathWarnsAndDoesNotChangeResult
// covers its "mcp audit log" warning when the audit path cannot be written.
func TestExecutePurchaseAuditLogging(t *testing.T) {
	// Not parallel: this test swaps the shared standard-logger output.
	rec := testRecommendation()

	t.Run("a preview logs nothing", func(t *testing.T) {
		out := captureStandardLogger(t, func() {
			_, err := ExecutePurchase(context.Background(), PurchaseRequest{
				Region: "us-east-1", Recommendation: rec, DryRun: true,
			})
			require.NoError(t, err)
		})
		assert.Empty(t, out, "a dry run spends nothing and must not pollute the stderr purchase diagnostic trail")
	})

	t.Run("a real purchase logs the attempt and the outcome", func(t *testing.T) {
		fake := &fakeServiceClient{purchaseResult: common.PurchaseResult{Success: true, CommitmentID: "ri-abc123"}}
		out := captureStandardLogger(t, func() {
			_, err := ExecutePurchase(context.Background(), PurchaseRequest{
				Region: "us-east-1", Recommendation: rec, DryRun: false, Confirm: true,
				CredentialScope: "subscription-a",
				ResolveClient:   func(_ context.Context) (provider.ServiceClient, error) { return fake, nil },
			})
			require.NoError(t, err)
		})

		assert.Contains(t, out, "mcp purchase ATTEMPT")
		assert.Contains(t, out, "mcp purchase OK")
		assert.Contains(t, out, "subscription-a", "the audit line must record which account was billed")
		assert.Contains(t, out, "ri-abc123", "the audit line must record the resulting commitment ID")

		token := fake.lastOpts.IdempotencyToken
		require.NotEmpty(t, token)
		assert.NotContains(t, out, token, "the full idempotency token must never be logged")
		assert.Contains(t, out, common.MaskToken(token), "the masked token must be logged to correlate attempt with outcome")
	})

	t.Run("a failed purchase logs the failure", func(t *testing.T) {
		fake := &fakeServiceClient{purchaseErr: errors.New("insufficient capacity")}
		out := captureStandardLogger(t, func() {
			_, err := ExecutePurchase(context.Background(), PurchaseRequest{
				Region: "us-east-1", Recommendation: rec, DryRun: false, Confirm: true, CredentialScope: "test-scope",
				ResolveClient: func(_ context.Context) (provider.ServiceClient, error) { return fake, nil },
			})
			require.Error(t, err)
		})
		assert.Contains(t, out, "mcp purchase FAILED")
		assert.Contains(t, out, "insufficient capacity")
	})

	// TestExecutePurchaseAuditLogging/a_provider-reported_failure_is_never_
	// logged_as_OK is the regression guard for the audit-line bug found in
	// review: PurchaseCommitment can return a nil Go error alongside
	// PurchaseResult{Success: false, Error: nil} (the provider ran the call
	// but reports it did not actually buy anything), and before this fix that
	// combination fell through to the "mcp purchase OK" line because
	// logPurchaseOutcome only inspected the (nil) error, never
	// result.Success.
	t.Run("a provider-reported failure is never logged as OK", func(t *testing.T) {
		fake := &fakeServiceClient{purchaseResult: common.PurchaseResult{Success: false, Error: nil}}
		out := captureStandardLogger(t, func() {
			_, err := ExecutePurchase(context.Background(), PurchaseRequest{
				Region: "us-east-1", Recommendation: rec, DryRun: false, Confirm: true, CredentialScope: "test-scope",
				ResolveClient: func(_ context.Context) (provider.ServiceClient, error) { return fake, nil },
			})
			require.NoError(t, err, "a provider-reported failure surfaces via the response, not a Go error")
		})
		assert.Contains(t, out, "mcp purchase FAILED",
			"Success=false must be logged as a failure even with a nil result.Error")
		assert.NotContains(t, out, "mcp purchase OK")
	})
}

// TestExecutePurchaseContradictoryProviderResultIsFailure pins the
// contradictory provider result found in review: a provider can return a
// nil Go error while embedding a non-nil result.Error alongside
// Success=true. Every consumer-facing signal must still classify that as a
// failed purchase.
func TestExecutePurchaseContradictoryProviderResultIsFailure(t *testing.T) {
	// Not parallel: this test swaps the shared standard-logger output and
	// sets a process environment override for the audit path.
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	t.Setenv(EnvAuditLog, path)

	injectedErr := errors.New("provider returned contradictory success with error detail")
	fake := &fakeServiceClient{purchaseResult: common.PurchaseResult{
		Success:      true,
		CommitmentID: "ri-contradictory",
		Error:        injectedErr,
	}}

	var resp *PurchaseResponse
	out := captureStandardLogger(t, func() {
		var err error
		resp, err = ExecutePurchase(context.Background(), PurchaseRequest{
			Region: "us-east-1", Recommendation: testRecommendation(), DryRun: false, Confirm: true,
			CredentialScope: "test-scope",
			ResolveClient:   func(_ context.Context) (provider.ServiceClient, error) { return fake, nil },
		})
		require.NoError(t, err, "provider-level contradictory result must surface via the structured response")
	})

	require.NotNil(t, resp)
	assert.False(t, resp.Success)
	assert.Equal(t, injectedErr.Error(), resp.Error)
	assert.Nil(t, resp.Archera)
	assert.Contains(t, out, "mcp purchase FAILED")
	assert.Contains(t, out, injectedErr.Error())
	assert.NotContains(t, out, "mcp purchase OK")

	lines := readAuditLines(t, path)
	require.Len(t, lines, 1)
	var record common.AuditRecord
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &record))
	assert.Equal(t, "error", record.Status)
	assert.Equal(t, injectedErr.Error(), record.ErrorMessage)
}

// TestExecutePurchaseAuditStatusForOutcomeUnknown pins the audit status of a
// failed purchase: only an error wrapping common.ErrOutcomeUnknown is
// "unknown", matched by errors.Is and never by message text. Not parallel: it
// swaps the standard logger and the audit path.
func TestExecutePurchaseAuditStatusForOutcomeUnknown(t *testing.T) {
	run := func(t *testing.T, fake *fakeServiceClient) (status, stderr string) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "audit.jsonl")
		t.Setenv(EnvAuditLog, path)
		stderr = captureStandardLogger(t, func() {
			_, _ = ExecutePurchase(context.Background(), PurchaseRequest{
				Region: "us-east-1", Recommendation: testRecommendation(), Confirm: true, CredentialScope: "scope",
				ResolveClient: func(_ context.Context) (provider.ServiceClient, error) { return fake, nil },
			})
		})
		lines := readAuditLines(t, path)
		require.Len(t, lines, 1)
		var rec struct {
			Status string `json:"status"`
		}
		require.NoError(t, json.Unmarshal([]byte(lines[0]), &rec))
		return rec.Status, stderr
	}

	t.Run("a deep-wrapped sentinel returned as the error is unknown", func(t *testing.T) {
		wrapped := fmt.Errorf("purchase failed: %w", fmt.Errorf("send: %w", common.ErrOutcomeUnknown))
		status, stderr := run(t, &fakeServiceClient{purchaseErr: wrapped})
		assert.Equal(t, auditStatusUnknown, status)
		assert.Contains(t, stderr, "mcp purchase OUTCOME UNKNOWN")
	})

	t.Run("a sentinel in result.Error is unknown", func(t *testing.T) {
		fake := &fakeServiceClient{purchaseResult: common.PurchaseResult{
			Error: fmt.Errorf("purchase response was empty: %w", common.ErrOutcomeUnknown),
		}}
		status, stderr := run(t, fake)
		assert.Equal(t, auditStatusUnknown, status)
		assert.Contains(t, stderr, "mcp purchase OUTCOME UNKNOWN")
	})

	t.Run("a plain error is error", func(t *testing.T) {
		status, stderr := run(t, &fakeServiceClient{purchaseErr: errors.New("insufficient capacity")})
		assert.Equal(t, auditStatusError, status)
		assert.Contains(t, stderr, "mcp purchase FAILED")
	})

	t.Run("the sentinel's text without the sentinel is error", func(t *testing.T) {
		status, _ := run(t, &fakeServiceClient{purchaseErr: errors.New("purchase outcome unknown")})
		assert.Equal(t, auditStatusError, status, "the status must not depend on message text")
	})

	t.Run("a success is success", func(t *testing.T) {
		status, _ := run(t, &fakeServiceClient{purchaseResult: common.PurchaseResult{Success: true, CommitmentID: "ri-1"}})
		assert.Equal(t, auditStatusSuccess, status)
	})
}
