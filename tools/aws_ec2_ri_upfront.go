package tools

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
)

// approvedCurrency is the only currency the approved-upfront guard
// (cloud-commitments-go#611) can compare an offering against.
const approvedCurrency = "USD"

// upfrontQuote is what a partial- or all-upfront RI dry run reports: the
// offering's total upfront (FixedPrice x count) and its currency. The caller
// copies both into approved_upfront/approved_currency on the confirming call,
// and the purchase is refused if the offering it would buy then costs more
// than that figure plus go#611's tolerance.
type upfrontQuote struct {
	currency string
	total    float64
}

// requiresApprovedUpfront reports whether a confirmed purchase with this
// payment option must carry an approved upfront. Everything except
// no-upfront does, so a payment option added later fails closed (it needs a
// figure) instead of silently skipping the guard.
func requiresApprovedUpfront(paymentOption string) bool {
	return paymentOption != string(PaymentOptionNoUpfront)
}

// validateApprovedFigures checks approved_upfront and approved_currency in
// isolation: when present, the upfront must be a finite amount above zero
// (go#611 skips its check for zero or less, so accepting one would buy
// unguarded) and the currency must be exactly USD. service only words the error.
func validateApprovedFigures(upfront *float64, currency, service string) error {
	if currency != "" && currency != approvedCurrency {
		return fmt.Errorf("invalid approved_currency %q: must be %s, the only currency the %s price check supports",
			currency, approvedCurrency, service)
	}
	if upfront == nil {
		return nil
	}
	if v := *upfront; math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return fmt.Errorf("invalid approved_upfront %v: must be the positive amount from the dry run's approved_upfront", v)
	}
	return nil
}

// applyApprovedUpfront validates the approved-upfront arguments and, for a
// confirmed real purchase of a partial- or all-upfront RI, copies them into
// rec.CommitmentCost/CommitmentCurrency so the provider's go#611 guard
// compares the offering it is about to buy against them. A confirmed
// upfront purchase without both figures is refused here, before any
// credential, provider or AWS call. No-upfront purchases take neither field.
func applyApprovedUpfront(rec *common.Recommendation, args ec2RIPurchaseArgs, confirmedPurchase bool) error {
	return applyApprovedFigures(rec, args.ApprovedUpfront, args.ApprovedCurrency, "EC2", confirmedPurchase)
}

// applyApprovedFigures is applyApprovedUpfront for any service.
func applyApprovedFigures(rec *common.Recommendation, upfront *float64, currency, service string, confirmedPurchase bool) error {
	if err := validateApprovedFigures(upfront, currency, service); err != nil {
		return err
	}
	supplied := upfront != nil || currency != ""
	if !requiresApprovedUpfront(rec.PaymentOption) {
		if supplied {
			return fmt.Errorf("approved_upfront and approved_currency apply only to %s and %s purchases; omit them for payment_option=%s",
				PaymentOptionPartialUpfront, PaymentOptionAllUpfront, rec.PaymentOption)
		}
		return nil
	}
	if !confirmedPurchase {
		return nil
	}
	if upfront == nil || currency == "" {
		return fmt.Errorf("refusing real purchase: a payment_option=%s purchase needs approved_upfront and approved_currency; "+
			"run this call with dry_run=true first and copy approved_upfront and approved_currency from its response into this call",
			rec.PaymentOption)
	}
	rec.CommitmentCost = *upfront
	rec.CommitmentCurrency = currency
	return nil
}

// previewUpfront prices the offering for a partial- or all-upfront dry run.
// It returns nil (and contacts nothing) for a real purchase or a no-upfront
// dry run. The lookup is read-only (DescribeReservedInstancesOfferings) and
// needs credentials, but not the real-purchase opt-in or the spend caps.
func (t *awsEC2RIPurchaseTool) previewUpfront(ctx context.Context, args ec2RIPurchaseArgs, region string, rec common.Recommendation, offeringClass string, dryRun bool) (*upfrontQuote, error) {
	return previewUpfrontQuote(ctx, rec, dryRun, "EC2", func(ctx context.Context) (*common.OfferingDetails, error) {
		client, err := t.resolveClient(args, region)(ctx)
		if err != nil {
			return nil, fmt.Errorf("dry run: resolve the AWS EC2 client to price the offering: %w", err)
		}
		// The offering class is passed explicitly (never the convertible default
		// of GetOfferingDetails) so a standard dry run quotes the standard price.
		pricer, ok := client.(classPricer)
		if !ok {
			return nil, errors.New("dry run: the EC2 client cannot price an offering class (GetOfferingDetailsForClass missing, cloud-commitments-go#574)")
		}
		od, err := pricer.GetOfferingDetailsForClass(ctx, rec, offeringClass)
		if err != nil {
			return nil, fmt.Errorf("dry run: cannot fetch the EC2 offering's upfront price: %w", err)
		}
		return od, nil
	})
}

// previewUpfrontQuote is previewUpfront for any service: price resolves the
// client and fetches the offering, and must word its own errors.
func previewUpfrontQuote(ctx context.Context, rec common.Recommendation, dryRun bool, service string, price func(context.Context) (*common.OfferingDetails, error)) (*upfrontQuote, error) {
	if !dryRun || !requiresApprovedUpfront(rec.PaymentOption) {
		return nil, nil
	}
	od, err := price(ctx)
	if err != nil {
		return nil, err
	}
	return quoteUpfront(od, rec, service)
}

// quoteFromOffering is quoteUpfront for EC2.
func quoteFromOffering(od *common.OfferingDetails, rec common.Recommendation) (*upfrontQuote, error) {
	return quoteUpfront(od, rec, "EC2")
}

// quoteUpfront turns the per-reservation offering price into the
// total upfront for rec.Count, rounded to cents. An upfront offering with
// no positive upfront, or one not priced in USD, cannot produce a figure
// the confirming call could pass, so the dry run fails instead.
func quoteUpfront(od *common.OfferingDetails, rec common.Recommendation, service string) (*upfrontQuote, error) {
	if od == nil {
		return nil, fmt.Errorf("dry run: the provider returned no %s offering details", service)
	}
	unit := od.UpfrontCost
	if math.IsNaN(unit) || math.IsInf(unit, 0) || unit <= 0 {
		return nil, fmt.Errorf("dry run: the %s %s offering reports upfront %v; expected a positive amount", rec.PaymentOption, service, unit)
	}
	if cur := strings.TrimSpace(od.Currency); cur != approvedCurrency {
		return nil, fmt.Errorf("dry run: the %s offering is priced in %q; only %s purchases can be approved", service, od.Currency, approvedCurrency)
	}
	total := math.Round(unit*float64(rec.Count)*100) / 100
	if math.IsInf(total, 0) || total <= 0 {
		return nil, fmt.Errorf("dry run: the %s offering's total upfront %v is not a usable amount", service, total)
	}
	return &upfrontQuote{total: total, currency: approvedCurrency}, nil
}
