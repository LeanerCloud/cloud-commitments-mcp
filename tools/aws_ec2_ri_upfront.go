package tools

import (
	"context"
	"errors"
	"fmt"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
)

// applyApprovedUpfront validates the approved-upfront arguments and, for a
// confirmed real purchase of a partial- or all-upfront RI, copies them into
// rec.CommitmentCost/CommitmentCurrency so the provider's go#611 guard
// compares the offering it is about to buy against them. A confirmed
// upfront purchase without both figures is refused here, before any
// credential, provider or AWS call. No-upfront purchases take neither field.
func applyApprovedUpfront(rec *common.Recommendation, args ec2RIPurchaseArgs, confirmedPurchase bool) error {
	return applyApprovedFigures(rec, args.ApprovedUpfront, args.ApprovedCurrency, "EC2", confirmedPurchase)
}

// previewUpfront prices the offering for a partial- or all-upfront dry run.
// It returns nil (and contacts nothing) for a real purchase or a no-upfront
// dry run. The lookup is read-only (DescribeReservedInstancesOfferings) and
// needs credentials, but not the real-purchase opt-in or the spend caps.
func (t *awsEC2RIPurchaseTool) previewUpfront(ctx context.Context, args ec2RIPurchaseArgs, region string, req *PurchaseRequest, offeringClass string) (*upfrontQuote, error) {
	return previewUpfrontQuote(ctx, req, "EC2", func(ctx context.Context) (*common.OfferingDetails, error) {
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
		od, err := pricer.GetOfferingDetailsForClass(ctx, req.Recommendation, offeringClass)
		if err != nil {
			return nil, fmt.Errorf("dry run: cannot fetch the EC2 offering's upfront price: %w", err)
		}
		return od, nil
	})
}

// quoteFromOffering is quoteUpfront for EC2.
func quoteFromOffering(od *common.OfferingDetails, rec common.Recommendation) (*upfrontQuote, error) {
	return quoteUpfront(od, rec, "EC2")
}
