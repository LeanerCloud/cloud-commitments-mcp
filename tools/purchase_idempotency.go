package tools

import (
	"fmt"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
)

// idempotencyKeyFor derives a stable per-request key from every field that
// identifies what is being bought: provider, region, service, resource type,
// count, term, payment option, plus every service-specific dimension held in
// rec.Details (see detailsKeyComponent) -- platform/tenancy/scope for EC2
// RIs, engine/az_config for RDS RIs, engine for ElastiCache RIs, hourly
// commitment/instance family for Savings Plans, memory for GCP CUDs. A
// caller re-driving the exact same tool call (e.g. after a network timeout)
// reuses the same common.DeriveIdempotencyToken output and the provider
// dedupes the retry instead of double-purchasing; a request that differs in
// ANY price- or identity-affecting dimension derives a different key instead
// of silently colliding with an unrelated purchase (issue found in review:
// a $5/hr and $50/hr Compute Savings Plan previously shared a token because
// only HourlyCommitment differed and Details was never consulted). This is a
// request-scoped substitute for the purchase_executions row that the CLI/web
// paths use as their idempotency anchor (pkg/common/tokens.go) -- the MCP
// server has no such row, so the request's own identifying fields play that
// role.
//
// rec.Account is deliberately excluded: no *FromArgs constructor in this
// package populates it today, so folding it in would add an always-empty,
// misleading key component rather than real discrimination. scope carries
// that information instead -- see below.
//
// scope (the caller's AWS profile / Azure subscription / GCP project, via
// PurchaseRequest.CredentialScope) is folded in because the product
// dimensions above describe WHAT is bought but not WHERE it lands. Omitting
// it is not merely imprecise, it silently skips real purchases on Azure:
// reservations.FindReservationOrderByIdempotencyToken lists reservation
// orders from the TENANT-wide endpoint (no subscription prefix -- see
// ReservationOrdersListURL), so an identical VM reservation requested for a
// second subscription in the same tenant would match the first
// subscription's order by token, short-circuit, and report success without
// buying anything for the second subscription. AWS (per-account tag/
// ClientToken lookups) and GCP (per-project commitment names) scope their
// own dedupe, so this is defense in depth there and load-bearing on Azure.
//
// This function is deliberately fail-safe with respect to time: it folds in
// no clock reading of any kind. When nonce is empty (the default), two calls
// with identical dimensions ALWAYS derive the same key, no matter how far
// apart in time they happen -- so a retry that straddles any time boundary
// still dedupes at the provider instead of risking a double purchase. An
// earlier version of this function instead folded in an automatic hourly
// time bucket to distinguish "buy 3 RIs now" from a genuinely separate "buy
// 3 more next week" with identical parameters; that inverted the safety
// direction of this money path, because a retry that happened to straddle
// an hour boundary (e.g. a slow request issued at 12:59:58 retried at
// 13:00:02) derived a different key and could double-buy. The worst case of
// the current, fail-safe default is a skipped intentional repeat -- a
// caller who genuinely wants a second, identical purchase must say so
// explicitly. nonce is that explicit opt-in: when the caller supplies a
// non-empty nonce, it is folded into the key so an otherwise-identical
// purchase becomes a distinct one (e.g. "buy 3 now" then "buy 3 more next
// week" by passing a fresh nonce on the second call); the same nonce plus
// the same dimensions still dedupes a nonce'd retry.
func idempotencyKeyFor(region string, rec common.Recommendation, scope, nonce string) string {
	return fmt.Sprintf("mcp:%s:%s:%s:%s:%s:%d:%s:%s:%s:%s",
		rec.Provider, scope, region, rec.Service, rec.ResourceType, rec.Count, rec.Term, rec.PaymentOption,
		detailsKeyComponent(rec.Details), nonce)
}

// detailsKeyComponent returns a canonical, deterministic encoding of every
// field in rec.Details that the purchase tools in this package populate, so
// idempotencyKeyFor can fold service-specific price-affecting dimensions
// into the token. Each case lists every field of its concrete Details type
// explicitly (not a hand-picked subset) so a field added to one of these
// types later shows up here as a visible diff rather than a silent key gap.
// Returns "" for nil or an unrecognized Details (e.g.
// azure_compute_ri.go's tool, whose recommendation carries no Details at
// all).
func detailsKeyComponent(details common.ServiceDetails) string {
	switch d := details.(type) {
	case *common.ComputeDetails:
		return computeDetailsKey(d)
	case common.ComputeDetails:
		return computeDetailsKey(&d)
	case *common.DatabaseDetails:
		return databaseDetailsKey(d)
	case *common.CacheDetails:
		return cacheDetailsKey(d)
	case *common.SavingsPlanDetails:
		return savingsPlanDetailsKey(d)
	default:
		return ""
	}
}

func computeDetailsKey(d *common.ComputeDetails) string {
	if d == nil {
		return ""
	}
	return fmt.Sprintf("instance_type=%s;platform=%s;tenancy=%s;scope=%s;vcpu=%d;memory_gb=%g",
		d.InstanceType, d.Platform, d.Tenancy, d.Scope, d.VCPU, d.MemoryGB)
}

func databaseDetailsKey(d *common.DatabaseDetails) string {
	if d == nil {
		return ""
	}
	return fmt.Sprintf("engine=%s;engine_version=%s;az_config=%s;instance_class=%s;deployment=%s",
		d.Engine, d.EngineVersion, d.AZConfig, d.InstanceClass, d.Deployment)
}

func cacheDetailsKey(d *common.CacheDetails) string {
	if d == nil {
		return ""
	}
	return fmt.Sprintf("engine=%s;node_type=%s;shards=%d", d.Engine, d.NodeType, d.Shards)
}

func savingsPlanDetailsKey(d *common.SavingsPlanDetails) string {
	if d == nil {
		return ""
	}
	return fmt.Sprintf("plan_type=%s;hourly_commitment=%g;coverage=%s;instance_family=%s;region=%s;offering_id=%s",
		d.PlanType, d.HourlyCommitment, d.Coverage, d.InstanceFamily, d.Region, d.OfferingID)
}
