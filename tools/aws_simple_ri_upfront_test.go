package tools

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
)

// simpleToolOver wires a simple RI tool from ctor to client and returns it
// with a counter of provider resolutions.
func simpleToolOver(ctor func() Registration, client provider.ServiceClient) (*simpleAWSRIPurchaseTool, *int) {
	tool := ctor().(*simpleAWSRIPurchaseTool)
	resolved := new(int)
	tool.createProvider = func(string, *provider.Config) (provider.Provider, error) {
		*resolved++
		return &recordingProvider{
			fakeProvider: &fakeProvider{name: "aws"}, client: client,
			gotService: new(common.ServiceType), gotRegion: new(string),
		}, nil
	}
	return tool, resolved
}

func upfrontSimpleArgs(payment PaymentOption, nonce string) simpleAWSRIPurchaseArgs {
	args := validSimpleArgs()
	args.PaymentOption = string(payment)
	args.IdempotencyNonce = nonce
	return args
}

func confirmedSimple(args simpleAWSRIPurchaseArgs, upfront *float64, currency string) simpleAWSRIPurchaseArgs {
	args.DryRun, args.Confirm = boolPtr(false), boolPtr(true)
	args.ApprovedUpfront, args.ApprovedCurrency = upfront, currency
	return args
}

func TestSimpleUpfrontDryRunReturnsApprovedUpfront(t *testing.T) {
	t.Parallel()
	for product, ctor := range simpleToolConstructors() {
		for _, payment := range []PaymentOption{PaymentOptionPartialUpfront, PaymentOptionAllUpfront} {
			t.Run(product+"/"+string(payment), func(t *testing.T) {
				t.Parallel()
				fake := &fakeServiceClient{offering: upfrontOffering(500.25, "USD")}
				tool, resolved := simpleToolOver(ctor, fake)
				_, resp, err := tool.handle(context.Background(), nil, upfrontSimpleArgs(payment, "dry"))
				require.NoError(t, err)
				assert.True(t, resp.DryRun)
				require.NotNil(t, resp.ApprovedUpfront)
				assert.InDelta(t, 1000.5, *resp.ApprovedUpfront, 1e-9, "2 x 500.25")
				assert.Equal(t, "USD", resp.ApprovedCurrency)
				assert.Equal(t, 1, *resolved)
				assert.Zero(t, fake.purchaseCalls, "a dry run must never buy")
			})
		}
	}
}

func TestSimpleUpfrontErrorsNameTheService(t *testing.T) {
	t.Parallel()
	for product, ctor := range simpleToolConstructors() {
		t.Run(product, func(t *testing.T) {
			t.Parallel()
			label := ctor().(*simpleAWSRIPurchaseTool).spec.displayName

			tool, _ := simpleToolOver(ctor, &fakeServiceClient{offering: upfrontOffering(10, "EUR")})
			_, _, err := tool.handle(context.Background(), nil, upfrontSimpleArgs(PaymentOptionAllUpfront, "eur"))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "the "+label+" offering is priced in")

			tool, _ = simpleToolOver(ctor, &fakeServiceClient{offeringErr: assert.AnError})
			_, _, err = tool.handle(context.Background(), nil, upfrontSimpleArgs(PaymentOptionAllUpfront, "err"))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "dry run: cannot fetch the "+label+" offering's upfront price")

			tool, resolved := simpleToolOver(ctor, &fakeServiceClient{})
			_, _, err = tool.handle(context.Background(), nil,
				confirmedSimple(upfrontSimpleArgs(PaymentOptionAllUpfront, "cur"), floatPtr(100), "EUR"))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "the only currency the "+label+" price check supports")
			assert.Zero(t, *resolved)
		})
	}
}

func TestSimpleUpfrontPurchaseWithoutFigureIsRefused(t *testing.T) {
	t.Parallel()
	for product, ctor := range simpleToolConstructors() {
		for name, tc := range map[string]struct {
			upfront  *float64
			currency string
		}{
			"both missing":     {nil, ""},
			"upfront missing":  {nil, "USD"},
			"currency missing": {floatPtr(1500), ""},
		} {
			t.Run(product+"/"+name, func(t *testing.T) {
				t.Parallel()
				fake := &fakeServiceClient{}
				tool, resolved := simpleToolOver(ctor, fake)
				_, _, err := tool.handle(context.Background(), nil,
					confirmedSimple(upfrontSimpleArgs(PaymentOptionAllUpfront, "missing"), tc.upfront, tc.currency))
				require.Error(t, err)
				assert.Contains(t, err.Error(), "dry_run=true first")
				assert.Zero(t, *resolved, "refused before resolving a provider")
				assert.Zero(t, fake.purchaseCalls+fake.offeringCalls)
			})
		}
	}
}

// Not parallel: Redshift's durable first-attempt store reads EnvBudgetState.
func TestSimpleUpfrontPurchaseCarriesApprovedFigure(t *testing.T) {
	for product, ctor := range simpleToolConstructors() {
		t.Run(product, func(t *testing.T) {
			t.Setenv(EnvBudgetState, filepath.Join(t.TempDir(), "budget.json"))
			t.Setenv(EnvAuditLog, filepath.Join(t.TempDir(), "audit.jsonl"))
			fake := &fakeServiceClient{purchaseResult: common.PurchaseResult{Success: true, CommitmentID: "res-1"}}
			tool, _ := simpleToolOver(ctor, fake)
			_, _, err := tool.handle(context.Background(), nil,
				confirmedSimple(upfrontSimpleArgs(PaymentOptionPartialUpfront, "carries-"+product), floatPtr(1234.56), "USD"))
			require.NoError(t, err)
			require.Equal(t, 1, fake.purchaseCalls)
			got := fake.purchaseResult.Recommendation
			assert.InDelta(t, 1234.56, got.CommitmentCost, 1e-9)
			assert.Equal(t, "USD", got.CommitmentCurrency)
		})
	}
}

func TestSimpleNoUpfrontNeedsNoFigure(t *testing.T) {
	t.Parallel()
	for product, ctor := range simpleToolConstructors() {
		t.Run(product, func(t *testing.T) {
			t.Parallel()
			fake := &fakeServiceClient{}
			tool, resolved := simpleToolOver(ctor, fake)
			args := upfrontSimpleArgs(PaymentOptionNoUpfront, "no-upfront")

			_, preview, err := tool.handle(context.Background(), nil, args)
			require.NoError(t, err)
			assert.Nil(t, preview.ApprovedUpfront)
			assert.Empty(t, preview.ApprovedCurrency)
			assert.Zero(t, *resolved, "a no-upfront dry run never contacts AWS")

			for name, bad := range map[string]simpleAWSRIPurchaseArgs{
				"upfront":  confirmedSimple(args, floatPtr(10), ""),
				"currency": confirmedSimple(args, nil, "USD"),
			} {
				_, _, err := tool.handle(context.Background(), nil, bad)
				require.Error(t, err, name)
				assert.Contains(t, err.Error(), "apply only to partial-upfront and all-upfront", name)
			}
		})
	}
}
