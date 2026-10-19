package tools

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
)

// refusalCase sets up one pre-purchase refusal. setup runs after the audit
// log is pinned and may change env vars (t.Setenv) and the request.
type refusalCase struct {
	name   string
	setup  func(t *testing.T, req *PurchaseRequest)
	reason string
}

func refusalCases() []refusalCase {
	return []refusalCase{
		{name: "real purchases disabled", reason: EnvEnableRealPurchases, setup: func(t *testing.T, _ *PurchaseRequest) {
			t.Setenv(EnvEnableRealPurchases, "")
		}},
		{name: "count cap", reason: EnvMaxCount, setup: func(t *testing.T, _ *PurchaseRequest) {
			t.Setenv(EnvMaxCount, "1")
		}},
		{name: "hourly cap", reason: EnvMaxHourlyCommitment, setup: func(t *testing.T, req *PurchaseRequest) {
			t.Setenv(EnvMaxHourlyCommitment, "1")
			req.Recommendation = spRec(t, 100)
		}},
		{name: "memory cap", reason: EnvMaxMemoryGB, setup: func(t *testing.T, req *PurchaseRequest) {
			t.Setenv(EnvMaxMemoryGB, "1")
			req.Recommendation.CommitmentType = common.CommitmentCUD
			req.Recommendation.Details = common.ComputeDetails{MemoryGB: 100}
		}},
		{name: "usd cap exceeded", reason: EnvMaxCommitmentUSD, setup: func(t *testing.T, req *PurchaseRequest) {
			t.Setenv(EnvMaxCommitmentUSD, "10")
			req.ResolveClient = resolveTo(pricedFake(1000, common.PurchaseResult{}, nil))
		}},
		{name: "unpriceable offering", reason: "cannot determine USD total", setup: func(t *testing.T, req *PurchaseRequest) {
			req.ResolveClient = resolveTo(&fakeServiceClient{offeringErr: assert.AnError})
		}},
		{name: "missing credential scope", reason: "could not be determined", setup: func(t *testing.T, req *PurchaseRequest) {
			t.Setenv("AWS_PROFILE", "")
			req.CredentialScope = ""
		}},
		{name: "resolve client error", reason: "resolve aws service client", setup: func(_ *testing.T, req *PurchaseRequest) {
			req.ResolveClient = func(context.Context) (provider.ServiceClient, error) {
				return nil, errors.New("no credentials")
			}
		}},
		{name: "cumulative budget", reason: EnvBudgetUSD, setup: func(t *testing.T, req *PurchaseRequest) {
			resetBudget(t, "10")
			req.ResolveClient = resolveTo(pricedFake(1000, common.PurchaseResult{}, nil))
		}},
	}
}

func resolveTo(c provider.ServiceClient) ResolveClientFunc {
	return func(context.Context) (provider.ServiceClient, error) { return c, nil }
}

func TestRefusalWritesOneRefusedRecord(t *testing.T) {
	for _, tc := range refusalCases() {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "audit.jsonl")
			t.Setenv(EnvAuditLog, path)
			rec := testRecommendation()
			req := PurchaseRequest{
				Region: rec.Region, Recommendation: rec, Confirm: true, CredentialScope: "scope",
				ResolveClient: resolveTo(&fakeServiceClient{}),
			}
			tc.setup(t, &req)

			var err error
			stderr := captureStandardLogger(t, func() {
				_, err = ExecutePurchase(context.Background(), req)
			})
			require.ErrorContains(t, err, tc.reason)

			lines := readAuditLines(t, path)
			require.Len(t, lines, 1)
			var got common.AuditRecord
			require.NoError(t, json.Unmarshal([]byte(lines[0]), &got))
			assert.Equal(t, auditStatusRefused, got.Status)
			assert.Contains(t, got.ErrorMessage, tc.reason)
			assert.Equal(t, req.CredentialScope, got.CredentialScope)
			assert.Equal(t, string(req.Recommendation.Service), got.Service)
			assert.Equal(t, req.Recommendation.Region, got.Region)
			assert.Equal(t, req.Recommendation.Count, got.Count)
			assert.False(t, got.DryRun)
			assert.Empty(t, got.CommitmentID)
			assert.Equal(t, 1, strings.Count(stderr, "mcp purchase REFUSED"), stderr)
		})
	}
}

func TestRefusalWithAuditDisabledKeepsBehavior(t *testing.T) {
	t.Setenv(EnvAuditLog, "")
	t.Setenv(EnvMaxCount, "1")
	rec := testRecommendation()
	var err error
	stderr := captureStandardLogger(t, func() {
		_, err = ExecutePurchase(context.Background(), PurchaseRequest{
			Region: rec.Region, Recommendation: rec, Confirm: true, CredentialScope: "scope",
			ResolveClient: resolveTo(&fakeServiceClient{}),
		})
	})
	require.ErrorContains(t, err, EnvMaxCount)
	assert.Equal(t, 1, strings.Count(stderr, "mcp purchase REFUSED"))
	assert.NotContains(t, stderr, "mcp audit log")
}

func TestRefusalWithNewlineStaysOnOneStderrLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	t.Setenv(EnvAuditLog, path)
	rec := testRecommendation()
	const reason = "boom\nsecond line"
	var err error
	stderr := captureStandardLogger(t, func() {
		_, err = ExecutePurchase(context.Background(), PurchaseRequest{
			Region: rec.Region, Recommendation: rec, Confirm: true, CredentialScope: "scope",
			ResolveClient: func(context.Context) (provider.ServiceClient, error) {
				return nil, errors.New(reason)
			},
		})
	})
	require.ErrorContains(t, err, reason)

	var refused []string
	for _, l := range strings.Split(stderr, "\n") {
		if strings.Contains(l, "mcp purchase REFUSED") {
			refused = append(refused, l)
		}
	}
	require.Len(t, refused, 1)
	assert.Contains(t, refused[0], "second line")

	lines := readAuditLines(t, path)
	require.Len(t, lines, 1)
	var got common.AuditRecord
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &got))
	assert.Contains(t, got.ErrorMessage, reason)
}
