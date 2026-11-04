package tools

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
)

// upfrontAuditRun is one handler invocation for a given scenario: the error
// it returned and how many provider purchase calls it made.
type upfrontAuditRun func(t *testing.T, s upfrontAuditScenario) (purchases int, err error)

type upfrontAuditScenario int

const (
	scenarioPreviewPricingFails upfrontAuditScenario = iota
	scenarioPreviewOK
	scenarioRealPurchase
	scenarioConfirmedMissingFigures
	scenarioQuoteRejected
)

var errPricingDown = errors.New("pricing backend down")

func upfrontAuditTools() map[string]upfrontAuditRun {
	tools := map[string]upfrontAuditRun{
		"ec2": func(t *testing.T, s upfrontAuditScenario) (int, error) {
			api := newPricedEC2API(PaymentOptionAllUpfront, 500)
			if s == scenarioQuoteRejected {
				api.fixedPrice, api.standardPrice = 0, 0
			}
			if s == scenarioPreviewPricingFails {
				api.describeErr = errPricingDown
			}
			tool, _ := ec2ToolOverFakeAPI(api)
			args := upfrontEC2Args(PaymentOptionAllUpfront, "audit-"+t.Name())
			switch s {
			case scenarioRealPurchase:
				args = confirmed(args, floatPtr(1500), "USD")
			case scenarioConfirmedMissingFigures:
				args = confirmed(args, nil, "")
			}
			_, _, err := tool.handle(context.Background(), nil, args)
			return api.purchaseCalls, err
		},
		"rds": func(t *testing.T, s upfrontAuditScenario) (int, error) {
			fake := &fakeServiceClient{offering: upfrontOffering(500, "USD"),
				purchaseResult: common.PurchaseResult{Success: true, CommitmentID: "rds-ri"}}
			if s == scenarioPreviewPricingFails {
				fake.offeringErr = errPricingDown
			}
			if s == scenarioQuoteRejected {
				fake.offering = upfrontOffering(0, "USD")
			}
			tool, _ := rdsToolOver(fake)
			args := upfrontRDSArgs(PaymentOptionAllUpfront, "audit-"+t.Name())
			switch s {
			case scenarioRealPurchase:
				args = confirmedRDS(args, floatPtr(1000), "USD")
			case scenarioConfirmedMissingFigures:
				args = confirmedRDS(args, nil, "")
			}
			_, _, err := tool.handle(context.Background(), nil, args)
			return fake.purchaseCalls, err
		},
	}
	for product, ctor := range simpleToolConstructors() {
		tools[product] = func(t *testing.T, s upfrontAuditScenario) (int, error) {
			fake := &fakeServiceClient{offering: upfrontOffering(500, "USD"),
				purchaseResult: common.PurchaseResult{Success: true, CommitmentID: "res-1"}}
			if s == scenarioPreviewPricingFails {
				fake.offeringErr = errPricingDown
			}
			if s == scenarioQuoteRejected {
				fake.offering = upfrontOffering(0, "USD")
			}
			tool, _ := simpleToolOver(ctor, fake)
			args := upfrontSimpleArgs(PaymentOptionAllUpfront, "audit-"+product+t.Name())
			switch s {
			case scenarioRealPurchase:
				args = confirmedSimple(args, floatPtr(1000), "USD")
			case scenarioConfirmedMissingFigures:
				args = confirmedSimple(args, nil, "")
			}
			_, _, err := tool.handle(context.Background(), nil, args)
			return fake.purchaseCalls, err
		}
	}
	return tools
}

func auditRecords(t *testing.T, path string) []map[string]any {
	t.Helper()
	lines := readAuditLines(t, path)
	out := make([]map[string]any, 0, len(lines))
	for _, l := range lines {
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(l), &m))
		out = append(out, m)
	}
	return out
}

// Not parallel: t.Setenv pins the audit log and Redshift's budget state.
func TestUpfrontAuditRecords(t *testing.T) {
	for name, run := range upfrontAuditTools() {
		// previewRecord is the successful-preview record, the baseline the
		// failed-pricing record must match except for status and error.
		var previewRecord map[string]any
		pin := func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "audit.jsonl")
			t.Setenv(EnvAuditLog, path)
			t.Setenv(EnvBudgetState, filepath.Join(t.TempDir(), "budget.json"))
			return path
		}

		t.Run(name+"/successful preview writes one record", func(t *testing.T) {
			path := pin(t)
			purchases, err := run(t, scenarioPreviewOK)
			require.NoError(t, err)
			assert.Zero(t, purchases)
			recs := auditRecords(t, path)
			require.Len(t, recs, 1)
			previewRecord = recs[0]
		})

		t.Run(name+"/failed preview pricing writes one error record", func(t *testing.T) {
			path := pin(t)
			purchases, err := run(t, scenarioPreviewPricingFails)
			require.Error(t, err)
			assert.Zero(t, purchases, "a dry run must never buy")
			recs := auditRecords(t, path)
			require.Len(t, recs, 1, "README: every purchase attempt, previews included, writes an audit record")
			got := recs[0]
			assert.Equal(t, auditStatusError, got["status"])
			assert.Equal(t, true, got["dry_run"])
			assert.Contains(t, got["error_message"], errPricingDown.Error())
			if previewRecord != nil {
				for _, k := range []string{"timestamp", "status", "error_message"} {
					delete(got, k)
					delete(previewRecord, k)
				}
				assert.Equal(t, previewRecord, got, "only status and error_message differ from a successful preview")
			}
		})

		t.Run(name+"/quote rejected writes one error record", func(t *testing.T) {
			path := pin(t)
			purchases, err := run(t, scenarioQuoteRejected)
			require.Error(t, err)
			assert.Zero(t, purchases)
			recs := auditRecords(t, path)
			require.Len(t, recs, 1)
			assert.Equal(t, auditStatusError, recs[0]["status"])
			assert.Equal(t, true, recs[0]["dry_run"])
			assert.NotEmpty(t, recs[0]["error_message"])
		})

		t.Run(name+"/real purchase writes one record", func(t *testing.T) {
			path := pin(t)
			purchases, err := run(t, scenarioRealPurchase)
			require.NoError(t, err)
			assert.Equal(t, 1, purchases)
			require.Len(t, auditRecords(t, path), 1)
		})

		t.Run(name+"/confirmed purchase without figures writes nothing", func(t *testing.T) {
			path := pin(t)
			purchases, err := run(t, scenarioConfirmedMissingFigures)
			require.Error(t, err)
			assert.Zero(t, purchases)
			assert.NoFileExists(t, path, "README: a missing approved figure is an argument error and writes nothing")
		})
	}
}
