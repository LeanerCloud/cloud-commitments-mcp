package tools

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
	"github.com/LeanerCloud/cloud-commitments-go/providers/aws/services/ec2"
)

// committedUSD returns the total USD a purchase commits over the full term.
// Anything it cannot price with confidence is an error, never zero.
//
// offeringClass is the EC2 RI offering class the purchase will use. The
// provider's GetOfferingDetails always prices the convertible offering, so a
// standard EC2 purchase is priced through classPricer instead, and refused if
// the client cannot do that (never priced as convertible).
func committedUSD(ctx context.Context, client provider.ServiceClient, rec common.Recommendation, offeringClass string) (float64, error) {
	switch {
	case rec.CommitmentType == common.CommitmentSavingsPlan:
		return savingsPlanUSD(rec)
	case rec.Provider == common.ProviderGCP || rec.CommitmentType == common.CommitmentCUD:
		return 0, errors.New("refusing real purchase: USD totals for GCP committed-use discounts are unavailable at the pinned cloud-commitments-go (go#321), so real GCP CUD purchases are refused")
	}
	var od *common.OfferingDetails
	var err error
	if rec.Service == common.ServiceEC2 && offeringClass == string(OfferingClassStandard) {
		pricer, ok := client.(classPricer)
		if !ok {
			return 0, errors.New("refusing real purchase: cannot determine USD total: the EC2 client cannot price a standard offering class (GetOfferingDetailsForClass missing, cloud-commitments-go#574), so a standard purchase is refused rather than priced as convertible")
		}
		od, err = pricer.GetOfferingDetailsForClass(ctx, rec, offeringClass)
	} else {
		od, err = client.GetOfferingDetails(ctx, rec)
	}
	if err != nil {
		return 0, fmt.Errorf("refusing real purchase: cannot determine USD total: %w", err)
	}
	return unitOfferingUSD(od, rec.Count)
}

// classPricer is implemented by (*ec2.Client) since cloud-commitments-go#574.
type classPricer interface {
	GetOfferingDetailsForClass(ctx context.Context, rec common.Recommendation, offeringClass string) (*common.OfferingDetails, error)
}

var _ classPricer = (*ec2.Client)(nil)

// savingsPlanUSD is hourly commitment x hours x years, from the typed details
// that the purchase itself uses (never the caller-visible CommitmentCost).
func savingsPlanUSD(rec common.Recommendation) (float64, error) {
	details, ok := rec.Details.(*common.SavingsPlanDetails)
	if !ok || details == nil {
		return 0, errors.New("refusing real purchase: cannot determine USD total: savings plan details are missing")
	}
	hourly := details.HourlyCommitment
	if math.IsNaN(hourly) || math.IsInf(hourly, 0) || hourly <= 0 {
		return 0, fmt.Errorf("refusing real purchase: cannot determine USD total: invalid hourly commitment %v", hourly)
	}
	years := termYearsFromRecommendationTerm(rec.Term)
	if years != 1 && years != 3 {
		return 0, fmt.Errorf("refusing real purchase: cannot determine USD total: unsupported savings plan term %q", rec.Term)
	}
	return finiteUSD(hourly * savingsPlanHoursPerYear * float64(years))
}

// unitOfferingUSD scales the provider's per-reservation total by count.
func unitOfferingUSD(od *common.OfferingDetails, count int) (float64, error) {
	if od == nil {
		return 0, errors.New("refusing real purchase: cannot determine USD total: provider returned no offering details")
	}
	if math.IsNaN(od.TotalCost) || math.IsInf(od.TotalCost, 0) || od.TotalCost <= 0 {
		return 0, fmt.Errorf("refusing real purchase: cannot determine USD total: provider total cost %v is not a positive price", od.TotalCost)
	}
	if !strings.EqualFold(strings.TrimSpace(od.Currency), "USD") {
		return 0, fmt.Errorf("refusing real purchase: cannot determine USD total: offering currency %q is not USD", od.Currency)
	}
	if count < 1 {
		return 0, fmt.Errorf("refusing real purchase: cannot determine USD total: count %d is below 1", count)
	}
	return finiteUSD(od.TotalCost * float64(count))
}

func finiteUSD(total float64) (float64, error) {
	if math.IsNaN(total) || math.IsInf(total, 0) {
		return 0, errors.New("refusing real purchase: cannot determine USD total: computed total overflows")
	}
	return total, nil
}

// enforceUSDCap refuses a purchase whose total exceeds limit (equal is
// allowed) and returns the total otherwise.
func enforceUSDCap(ctx context.Context, client provider.ServiceClient, rec common.Recommendation, offeringClass string, limit float64) (float64, error) {
	if math.IsNaN(limit) || math.IsInf(limit, 0) || limit <= 0 {
		return 0, fmt.Errorf("refusing real purchase: %s limit %v must be a finite number above 0", EnvMaxCommitmentUSD, limit)
	}
	total, err := committedUSD(ctx, client, rec, offeringClass)
	if err != nil {
		return 0, err
	}
	if total > limit {
		return 0, fmt.Errorf("refusing real purchase: commitment total %.2f USD exceeds the operator cap %s=%.2f", total, EnvMaxCommitmentUSD, limit)
	}
	return total, nil
}
