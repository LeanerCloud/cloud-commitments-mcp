package tools

import (
	"errors"
	"fmt"
	"strings"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
)

// validateSearchArgs validates every money-neutral-but-still-typed field on
// args that does not require a live provider (provider name, payment
// option, lookback_period, term, Savings Plans type filters), returning the
// typed provider name, the normalised Recommendation term string
// ("1yr"/"3yr", or "" when args.TermYears was omitted), and args with
// applySavingsPlansSearchDefaults applied.
//
// Every invalid or missing field is collected and joined into a single
// error rather than returning on the first one found, so a caller missing
// several required fields (see requireSavingsPlansSearchFields) sees all of
// them at once instead of fixing them one call at a time.
func validateSearchArgs(args searchRecommendationsArgs) (common.ProviderType, string, searchRecommendationsArgs, error) {
	providerType, err := validateProviderName(args.Provider)
	if err != nil {
		return "", "", args, err
	}
	args = applySavingsPlansSearchDefaults(providerType, args)

	term, errs := collectSearchFieldErrors(providerType, args)
	if len(errs) > 0 {
		return "", "", args, errors.Join(errs...)
	}

	return providerType, term, args, nil
}

// collectSearchFieldErrors validates every per-field constraint on args
// (payment option, lookback window, term, Savings Plans type filters, blank
// filter-list entries, and the Savings Plans required-field safety net) and
// returns the normalised Recommendation term alongside EVERY error found,
// rather than stopping at the first. A caller that got several fields wrong
// then fixes them in one round instead of one call at a time.
//
// Split out of validateSearchArgs so that function stays under the repo's
// gocyclo gate (the pre-commit hook is -over 10, stricter than golangci's
// min-complexity 15).
func collectSearchFieldErrors(providerType common.ProviderType, args searchRecommendationsArgs) (string, []error) {
	var errs []error

	if args.PaymentOption != "" {
		if _, err := ValidatePaymentOption(args.PaymentOption); err != nil {
			errs = append(errs, err)
		}
	}

	if _, err := ValidateLookbackPeriod(args.LookbackPeriod); err != nil {
		errs = append(errs, err)
	}

	term := ""
	if args.TermYears != 0 {
		if ty, err := ValidateTermYears(args.TermYears); err != nil {
			errs = append(errs, err)
		} else {
			term = ty.RecommendationTerm()
		}
	}

	if err := validateSPTypeFilters(args.IncludeSPTypes, args.ExcludeSPTypes); err != nil {
		errs = append(errs, err)
	}

	if err := validateFilterListEntries(args); err != nil {
		errs = append(errs, err)
	}

	if err := requireSavingsPlansSearchFields(providerType, args); err != nil {
		errs = append(errs, err)
	}

	return term, errs
}

// isAWSSavingsPlansSearch reports whether args targets an AWS Savings Plans
// search -- the only combination where AWS's
// GetSavingsPlansPurchaseRecommendation requires term_years, payment_option,
// and lookback_period on every call. GetReservationPurchaseRecommendation
// (EC2/RDS/etc) defaults these server-side when omitted, so detection must
// never broaden past the Savings Plans family. Checked straight off the raw
// args -- no live provider needed -- via the same common.IsSavingsPlan
// family predicate the AWS recommendations client itself dispatches on
// (providers/aws/recommendations/client.go).
func isAWSSavingsPlansSearch(providerType common.ProviderType, service string) bool {
	return providerType == common.ProviderAWS && common.IsSavingsPlan(common.ServiceType(service))
}

// applySavingsPlansSearchDefaults defaults payment_option, term_years, and
// lookback_period to no-upfront/1yr/30d when args targets an AWS Savings
// Plans search and the caller omitted them. Without this, an AWS SP search
// with these fields blank fails against Cost Explorer one field at a time
// (issue #1506): GetSavingsPlansPurchaseRecommendation requires all three,
// unlike the EC2/RDS reservation path this tool otherwise shares, which
// defaults them server-side. A caller-supplied value always wins -- this
// only fills in what was left blank. No-op for every other provider/service
// combination.
func applySavingsPlansSearchDefaults(providerType common.ProviderType, args searchRecommendationsArgs) searchRecommendationsArgs {
	if !isAWSSavingsPlansSearch(providerType, args.Service) {
		return args
	}
	if args.PaymentOption == "" {
		args.PaymentOption = string(PaymentOptionNoUpfront)
	}
	if args.TermYears == 0 {
		args.TermYears = int(TermOneYear)
	}
	if args.LookbackPeriod == "" {
		args.LookbackPeriod = string(LookbackPeriod30Days)
	}
	return args
}

// requireSavingsPlansSearchFields is a validate-all-at-once safety net: after
// applySavingsPlansSearchDefaults runs, an AWS Savings Plans search should
// never still be missing payment_option/term_years/lookback_period. If a
// future change to the defaulting logic leaves one blank anyway, this fails
// loud -- naming every still-missing field in one error -- rather than
// letting the request reach Cost Explorer and fail one field at a time (the
// cascade issue #1506 fixes).
func requireSavingsPlansSearchFields(providerType common.ProviderType, args searchRecommendationsArgs) error {
	if !isAWSSavingsPlansSearch(providerType, args.Service) {
		return nil
	}
	var missing []string
	if args.PaymentOption == "" {
		missing = append(missing, "payment_option")
	}
	if args.TermYears == 0 {
		missing = append(missing, "term_years")
	}
	if args.LookbackPeriod == "" {
		missing = append(missing, "lookback_period")
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("savings plans search missing required field(s): %s", strings.Join(missing, ", "))
}

// validateSPTypeFilters validates every entry of include/exclude against
// the AWS Savings Plans type enum, naming which filter a bad entry came
// from.
func validateSPTypeFilters(include, exclude []string) error {
	for _, sp := range include {
		if _, err := ValidateSPType(sp); err != nil {
			return fmt.Errorf("include_sp_types: %w", err)
		}
	}
	for _, sp := range exclude {
		if _, err := ValidateSPType(sp); err != nil {
			return fmt.Errorf("exclude_sp_types: %w", err)
		}
	}
	return nil
}
