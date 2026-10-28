package tools

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
)

// ec2ApprovedCurrency is the only currency the EC2 approved-upfront guard
// (cloud-commitments-go#611) can compare an offering against.
const ec2ApprovedCurrency = "USD"

// ec2UpfrontQuote is what a partial- or all-upfront EC2 RI dry run reports:
// the offering's total upfront (FixedPrice x count) and its currency. The
// caller copies both into approved_upfront/approved_currency on the
// confirming call, and the purchase is refused if the offering it would buy
// then costs more than that figure plus go#611's tolerance.
type ec2UpfrontQuote struct {
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

// validateApprovedUpfront checks approved_upfront and approved_currency in
// isolation: when present, the upfront must be a finite amount above zero
// (go#611 skips its check for zero or less, so accepting one would buy
// unguarded) and the currency must be exactly USD.
func validateApprovedUpfront(args ec2RIPurchaseArgs) error {
	if args.ApprovedCurrency != "" && args.ApprovedCurrency != ec2ApprovedCurrency {
		return fmt.Errorf("invalid approved_currency %q: must be %s, the only currency the EC2 price check supports",
			args.ApprovedCurrency, ec2ApprovedCurrency)
	}
	if args.ApprovedUpfront == nil {
		return nil
	}
	if v := *args.ApprovedUpfront; math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
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
	if err := validateApprovedUpfront(args); err != nil {
		return err
	}
	supplied := args.ApprovedUpfront != nil || args.ApprovedCurrency != ""
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
	if args.ApprovedUpfront == nil || args.ApprovedCurrency == "" {
		return fmt.Errorf("refusing real purchase: a payment_option=%s purchase needs approved_upfront and approved_currency; "+
			"run this call with dry_run=true first and copy approved_upfront and approved_currency from its response into this call",
			rec.PaymentOption)
	}
	rec.CommitmentCost = *args.ApprovedUpfront
	rec.CommitmentCurrency = args.ApprovedCurrency
	return nil
}

// previewUpfront prices the offering for a partial- or all-upfront dry run.
// It returns nil (and contacts nothing) for a real purchase or a no-upfront
// dry run. The lookup is read-only (DescribeReservedInstancesOfferings) and
// needs credentials, but not the real-purchase opt-in or the spend caps.
func (t *awsEC2RIPurchaseTool) previewUpfront(ctx context.Context, args ec2RIPurchaseArgs, region string, rec common.Recommendation, offeringClass string, dryRun bool) (*ec2UpfrontQuote, error) {
	if !dryRun || !requiresApprovedUpfront(rec.PaymentOption) {
		return nil, nil
	}
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
	return quoteFromOffering(od, rec)
}

// quoteFromOffering turns the per-reservation offering price into the
// total upfront for rec.Count, rounded to cents. An upfront offering with
// no positive upfront, or one not priced in USD, cannot produce a figure
// the confirming call could pass, so the dry run fails instead.
func quoteFromOffering(od *common.OfferingDetails, rec common.Recommendation) (*ec2UpfrontQuote, error) {
	if od == nil {
		return nil, errors.New("dry run: the provider returned no EC2 offering details")
	}
	unit := od.UpfrontCost
	if math.IsNaN(unit) || math.IsInf(unit, 0) || unit <= 0 {
		return nil, fmt.Errorf("dry run: the %s EC2 offering reports upfront %v; expected a positive amount", rec.PaymentOption, unit)
	}
	if cur := strings.TrimSpace(od.Currency); cur != ec2ApprovedCurrency {
		return nil, fmt.Errorf("dry run: the EC2 offering is priced in %q; only %s purchases can be approved", od.Currency, ec2ApprovedCurrency)
	}
	total := math.Round(unit*float64(rec.Count)*100) / 100
	if math.IsInf(total, 0) || total <= 0 {
		return nil, fmt.Errorf("dry run: the EC2 offering's total upfront %v is not a usable amount", total)
	}
	return &ec2UpfrontQuote{total: total, currency: ec2ApprovedCurrency}, nil
}
