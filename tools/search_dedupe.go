package tools

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/recfilter"
)

// dedupeMode says what the search does with a recommendation that recent
// commitments appear to cover. The library key (resource type, region, engine,
// deployment) is exact only for some services, so a rec is hidden only where
// that key is sufficient; elsewhere it stays visible and is flagged.
type dedupeMode int

const (
	// dedupeNotApplied: no check is run (Savings Plans, unlisted services).
	dedupeNotApplied dedupeMode = iota
	// dedupeFlagOnly: the rec is kept and flagged possibly_covered.
	dedupeFlagOnly
	// dedupeSuppress: a fully covered rec is removed and listed in the summary.
	dedupeSuppress
)

const (
	dedupeWindow = "24h" // recfilter.DefaultDuplicateCheckLookbackHours

	statusPossiblyCovered   = "possibly_covered"
	statusPartiallyCovered  = "partially_covered"
	statusNotCheckedAccount = "not_checked_other_account"

	reasonEC2Lossy = "a recent reservation of this instance type exists in this region; platform, tenancy, scope, AZ and term are not compared, so it may not cover this recommendation"
	reasonGCP      = "family has a recent CUD; the library defers the whole machine family without comparing vCPU or memory amounts"
	reasonAzure    = "a recent reservation of this size exists in this region; scope and term are not compared, so it may not cover this recommendation"
	reasonMemoryDB = "MemoryDB reserved nodes are listed with a fixed engine, so engine matching is not reliable"
	reasonWildcard = "a recent ElastiCache reservation with an unspecified engine exists and could cover any engine"
)

// dedupeModeFor is the single support table. Anything not listed is not
// checked: an unknown service must never be silently deduplicated.
func dedupeModeFor(rec common.Recommendation) (mode dedupeMode, reason string) {
	if common.IsSavingsPlan(rec.Service) {
		return dedupeNotApplied, "Savings Plan recommendations have no resource type or region to match"
	}
	switch rec.Provider {
	case common.ProviderAWS:
		switch rec.Service {
		case common.ServiceRDS, common.ServiceRelationalDB,
			common.ServiceOpenSearch, common.ServiceSearch, common.ServiceRedshift, common.ServiceDataWarehouse,
			common.ServiceElastiCache, common.ServiceCache:
			return dedupeSuppress, ""
		case common.ServiceMemoryDB:
			return dedupeFlagOnly, reasonMemoryDB
		case common.ServiceEC2, common.ServiceCompute:
			return dedupeFlagOnly, reasonEC2Lossy
		}
	case common.ProviderAzure:
		if rec.Service == common.ServiceCompute {
			return dedupeFlagOnly, reasonAzure
		}
	case common.ProviderGCP:
		if rec.Service == common.ServiceCompute {
			return dedupeFlagOnly, reasonGCP
		}
	}
	return dedupeNotApplied, "no duplicate check is defined for this service"
}

func isElastiCache(s common.ServiceType) bool {
	return s == common.ServiceElastiCache || s == common.ServiceCache
}

// listPermission names what the credentials need to list existing commitments,
// for the failure message.
func listPermission(rec common.Recommendation) string {
	switch rec.Provider {
	case common.ProviderAWS:
		switch rec.Service {
		case common.ServiceEC2, common.ServiceCompute:
			return "ec2:DescribeReservedInstances"
		case common.ServiceRDS, common.ServiceRelationalDB:
			return "rds:DescribeReservedDBInstances"
		case common.ServiceElastiCache, common.ServiceCache:
			return "elasticache:DescribeReservedCacheNodes"
		case common.ServiceMemoryDB:
			return "memorydb:DescribeReservedNodes"
		case common.ServiceOpenSearch, common.ServiceSearch:
			return "es:DescribeReservedInstances"
		case common.ServiceRedshift, common.ServiceDataWarehouse:
			return "redshift:DescribeReservedNodes"
		}
	case common.ProviderAzure:
		return "read access to reservation orders (Reservations Reader)"
	case common.ProviderGCP:
		return "compute.regionCommitments.list"
	}
	return "read access to existing commitments"
}

type dedupeGroup struct {
	Service string `json:"service"`
	Region  string `json:"region"`
	Status  string `json:"status"` // checked | not_applied
	Reason  string `json:"reason,omitempty"`
}

type suppressedRec struct {
	Service      string `json:"service"`
	Region       string `json:"region"`
	ResourceType string `json:"resource_type"`
	Account      string `json:"account,omitempty"`
	Term         string `json:"term,omitempty"`
	Payment      string `json:"payment_option,omitempty"`
	Reason       string `json:"reason"`
	Count        int    `json:"count"`
}

// flaggedRec points at a returned recommendation by index. The recommendation
// itself is never modified, so its cost and savings figures stay those of the
// recommended count.
type flaggedRec struct {
	Status              string `json:"status"`
	Reason              string `json:"reason"`
	Service             string `json:"service"`
	Region              string `json:"region"`
	ResourceType        string `json:"resource_type"`
	CoveredCount        int    `json:"covered_count,omitempty"`
	RecommendationIndex int    `json:"recommendation_index"`
}

// searchDedupe is the audit trail of the duplicate check: nothing the check
// removes or doubts is hidden from the caller.
type searchDedupe struct {
	Window     string          `json:"window"`
	Groups     []dedupeGroup   `json:"groups"`
	Suppressed []suppressedRec `json:"suppressed"`
	Flagged    []flaggedRec    `json:"flagged"`
}

type recentFilterClient interface {
	FilterRecommendationsForRecentCommitments(recs []common.Recommendation, existing []common.Commitment) (passed, filtered []common.Recommendation, err error)
}

// memoClient lists existing commitments once per (service, region) per search
// and remembers whether any ElastiCache reservation has no engine.
type memoClient struct {
	provider.ServiceClient
	err      error
	list     []common.Commitment
	listed   bool
	wildcard bool
	checkEC  bool
}

func (m *memoClient) GetExistingCommitments(ctx context.Context) ([]common.Commitment, error) {
	if !m.listed {
		m.list, m.err = m.ServiceClient.GetExistingCommitments(ctx)
		m.listed = true
		if m.checkEC {
			for i := range m.list {
				if common.NormalizeEngineName(m.list[i].Engine) == "" {
					m.wildcard = true
				}
			}
		}
	}
	return m.list, m.err
}

// hookedMemoClient additionally forwards the provider's own duplicate filter
// (GCP), which recfilter discovers by type assertion and a plain embed hides.
type hookedMemoClient struct {
	*memoClient
	hook recentFilterClient
}

func (h hookedMemoClient) FilterRecommendationsForRecentCommitments(recs []common.Recommendation, existing []common.Commitment) (passed, filtered []common.Recommendation, err error) {
	return h.hook.FilterRecommendationsForRecentCommitments(recs, existing)
}

func newMemoClient(inner provider.ServiceClient, service common.ServiceType) (*memoClient, provider.ServiceClient) {
	m := &memoClient{ServiceClient: inner, checkEC: isElastiCache(service)}
	if hook, ok := inner.(recentFilterClient); ok {
		return m, hookedMemoClient{memoClient: m, hook: hook}
	}
	return m, m
}

type groupKey struct {
	service common.ServiceType
	region  string
}

type variantKey struct{ term, payment string }

// dedupeSearchResults applies the duplicate check to a search result. It
// returns the recommendations to show and the audit trail. Any listing error
// fails the whole search: returning the recs as if checked would show covered
// ones without a warning.
func dedupeSearchResults(ctx context.Context, prov provider.Provider, recs []common.Recommendation) ([]common.Recommendation, searchDedupe, error) {
	out := searchDedupe{Window: dedupeWindow, Groups: []dedupeGroup{}, Suppressed: []suppressedRec{}, Flagged: []flaggedRec{}}
	if len(recs) == 0 {
		return recs, out, nil
	}

	order, byGroup := groupRecs(recs)
	checker := recfilter.NewDuplicateChecker(0)
	callerAccount := accountResolver{prov: prov}
	decisions := make([]recDecision, len(recs))

	for _, gk := range order {
		idxs := byGroup[gk]
		first := recs[idxs[0]]
		mode, reason := dedupeModeFor(first)
		group := dedupeGroup{Service: string(gk.service), Region: gk.region, Status: "checked"}
		if mode == dedupeNotApplied {
			group.Status, group.Reason = "not_applied", reason
			out.Groups = append(out.Groups, group)
			continue
		}
		if err := checkGroup(ctx, prov, checker, &callerAccount, recs, idxs, mode, reason, decisions); err != nil {
			return nil, searchDedupe{}, dedupeFailure(gk, first, err)
		}
		out.Groups = append(out.Groups, group)
	}

	kept := make([]common.Recommendation, 0, len(recs))
	for i := range recs {
		rec := &recs[i]
		d := decisions[i]
		if d.suppress {
			out.Suppressed = append(out.Suppressed, suppressedRec{
				Service: string(rec.Service), Region: rec.Region, ResourceType: rec.ResourceType, Account: rec.Account,
				Term: rec.Term, Payment: rec.PaymentOption, Count: rec.Count, Reason: d.reason,
			})
			continue
		}
		if d.status != "" {
			out.Flagged = append(out.Flagged, flaggedRec{
				Status: d.status, Reason: d.reason, Service: string(rec.Service), Region: rec.Region,
				ResourceType: rec.ResourceType, CoveredCount: d.covered, RecommendationIndex: len(kept),
			})
		}
		kept = append(kept, *rec)
	}
	return kept, out, nil
}

// accountLookupError is a failure to resolve the caller's account, which needs
// a different permission than listing commitments.
type accountLookupError struct {
	err        error
	permission string
}

func (e *accountLookupError) Error() string { return e.err.Error() }
func (e *accountLookupError) Unwrap() error { return e.err }

// listingError is a failure to list existing commitments.
type listingError struct{ err error }

func (e *listingError) Error() string { return e.err.Error() }
func (e *listingError) Unwrap() error { return e.err }

// dedupeFailure builds the loud failure, naming only the permission that the
// failing step actually needs.
func dedupeFailure(gk groupKey, first common.Recommendation, err error) error {
	var acct *accountLookupError
	var list *listingError
	switch {
	case errors.As(err, &acct):
		return fmt.Errorf("dedupe check failed for %s in %s: %w; the credentials need %s to identify the calling account; no recommendations returned",
			gk.service, gk.region, err, acct.permission)
	case errors.As(err, &list):
		return fmt.Errorf("dedupe check failed for %s in %s: %w; the credentials need %s to list existing commitments; no recommendations returned",
			gk.service, gk.region, err, listPermission(first))
	}
	return fmt.Errorf("dedupe check failed for %s in %s: %w; no recommendations returned", gk.service, gk.region, err)
}

type recDecision struct {
	status   string
	reason   string
	covered  int
	suppress bool
}

func groupRecs(recs []common.Recommendation) (order []groupKey, byGroup map[groupKey][]int) {
	byGroup = map[groupKey][]int{}
	for i := range recs {
		k := groupKey{recs[i].Service, recs[i].Region}
		if _, ok := byGroup[k]; !ok {
			order = append(order, k)
		}
		byGroup[k] = append(byGroup[k], i)
	}
	return order, byGroup
}

// checkGroup runs the duplicate check for one (service, region) group and
// records a decision per recommendation.
func checkGroup(ctx context.Context, prov provider.Provider, checker *recfilter.DuplicateChecker, accounts *accountResolver,
	recs []common.Recommendation, idxs []int, mode dedupeMode, flagReason string, decisions []recDecision) error {
	first := recs[idxs[0]]
	svc, err := prov.GetServiceClient(ctx, first.Service, first.Region)
	if err != nil {
		return fmt.Errorf("get service client: %w", err)
	}
	if svc == nil {
		return fmt.Errorf("no service client available")
	}
	memo, client := newMemoClient(svc, first.Service)

	variants, variantOrder, err := partitionVariants(ctx, accounts, recs, idxs, decisions)
	if err != nil {
		return err
	}

	for _, vk := range variantOrder {
		vidx := variants[vk]
		batch := make([]common.Recommendation, len(vidx))
		for j, i := range vidx {
			batch[j] = recs[i]
		}
		passed, filtered, err := checker.AdjustRecommendationsForExisting(ctx, batch, client)
		if err != nil {
			return &listingError{err: err}
		}
		outcomes, err := pairOutcomes(batch, passed, filtered)
		if err != nil {
			return err
		}
		for j, i := range vidx {
			decisions[i] = decide(outcomes[j], mode, flagReason)
		}
	}

	if memo.wildcard {
		flagWildcard(idxs, decisions)
	}
	return nil
}

// flagWildcard keeps every rec of the group visible: an engine-less
// reservation could cover any engine. Recs for another account stay as they are.
func flagWildcard(idxs []int, decisions []recDecision) {
	for _, i := range idxs {
		if decisions[i].status == statusNotCheckedAccount {
			continue
		}
		decisions[i] = recDecision{status: statusPossiblyCovered, reason: reasonWildcard}
	}
}

// partitionVariants splits a group into (term, payment) variants: variants are
// alternatives for the same demand, so each needs a fresh budget rather than
// sharing one. On AWS, recs for an account other than the caller's are
// decided here as not checked and left out.
func partitionVariants(ctx context.Context, accounts *accountResolver, recs []common.Recommendation, idxs []int,
	decisions []recDecision) (variants map[variantKey][]int, order []variantKey, err error) {
	variants = map[variantKey][]int{}
	for _, i := range idxs {
		if recs[i].Provider == common.ProviderAWS {
			caller, callerErr := accounts.callerID(ctx)
			if callerErr != nil {
				return nil, nil, callerErr
			}
			if caller == "" || recs[i].Account != caller {
				decisions[i] = recDecision{status: statusNotCheckedAccount,
					reason: "this recommendation is for a different account than the credentials; commitments there are not visible to this check"}
				continue
			}
		}
		k := variantKey{recs[i].Term, recs[i].PaymentOption}
		if _, ok := variants[k]; !ok {
			order = append(order, k)
		}
		variants[k] = append(variants[k], i)
	}
	return variants, order, nil
}

type outcome struct {
	fully   bool
	covered int
}

// pairOutcomes matches the library's passed (possibly count-reduced) and
// filtered (fully covered) lists back to the input order.
func pairOutcomes(batch, passed, filtered []common.Recommendation) (out []outcome, err error) {
	if len(passed)+len(filtered) != len(batch) {
		return nil, fmt.Errorf("duplicate checker returned %d+%d recommendations for %d inputs", len(passed), len(filtered), len(batch))
	}
	out = make([]outcome, len(batch))
	pi, fi := 0, 0
	for i := range batch {
		if fi < len(filtered) && reflect.DeepEqual(filtered[fi], batch[i]) {
			out[i] = outcome{fully: true, covered: batch[i].Count}
			fi++
			continue
		}
		if pi >= len(passed) {
			return nil, fmt.Errorf("duplicate checker output does not match its input")
		}
		out[i] = outcome{covered: batch[i].Count - passed[pi].Count}
		pi++
	}
	return out, nil
}

func decide(o outcome, mode dedupeMode, flagReason string) recDecision {
	switch {
	case o.fully && mode == dedupeSuppress:
		return recDecision{suppress: true, reason: "fully covered by recent commitments within " + dedupeWindow}
	case o.fully && mode == dedupeFlagOnly:
		return recDecision{status: statusPossiblyCovered, reason: flagReason}
	case o.covered > 0 && mode == dedupeSuppress:
		return recDecision{status: statusPartiallyCovered, covered: o.covered,
			reason: "recent commitments cover part of the recommended count; the recommendation is shown unchanged, including its cost and savings figures"}
	case o.covered > 0:
		return recDecision{status: statusPossiblyCovered, covered: o.covered, reason: flagReason}
	}
	return recDecision{}
}

// accountResolver resolves the caller's AWS account id once per search.
type accountResolver struct {
	prov provider.Provider
	id   string
	done bool
}

func (a *accountResolver) callerID(ctx context.Context) (string, error) {
	if a.done {
		return a.id, nil
	}
	accounts, err := a.prov.GetAccounts(ctx)
	if err != nil {
		permission := "sts:GetCallerIdentity"
		if strings.Contains(err.Error(), "organizations") {
			permission = "organizations:ListAccounts"
		}
		return "", &accountLookupError{err: fmt.Errorf("resolve caller account: %w", err), permission: permission}
	}
	a.done = true
	for _, acct := range accounts {
		if acct.IsDefault {
			a.id = acct.ID
			break
		}
	}
	return a.id, nil
}
