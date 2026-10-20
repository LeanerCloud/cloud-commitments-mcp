package tools

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/provider"
)

// purchaseMode is the outcome of the dry_run/confirm safety gate: either a
// local preview (no provider call) or a real execution (provider call with
// PurchaseSourceMCP + an idempotency token). There is deliberately no third
// "no-op" outcome -- an ambiguous combination of flags is always an error,
// never a silent do-nothing (feedback_no_silent_fallbacks).
type purchaseMode int

const (
	modePreview purchaseMode = iota
	modeExecute
)

// EnvEnableRealPurchases is the operator-side authorization control gating
// every real (non-preview) purchase this server executes. The model-supplied
// confirm flag (decidePurchaseMode) only proves the model asked to spend
// money; it says nothing about whether the operator running this MCP server
// process wants it able to. mcp/README.md previously documented confirm as
// "a guardrail against an accidental call, not an authorization control",
// which left a prompt-injected or simply hallucinating model, given ambient
// production credentials, able to execute a real purchase from a single tool
// call. This env var closes that gap: it must be explicitly set before
// ExecutePurchase will call into a provider for a real purchase, regardless
// of confirm. See realPurchasesEnabled for the fail-closed matching rule.
const EnvEnableRealPurchases = "CUDLY_MCP_ENABLE_REAL_PURCHASES"

// realPurchasesEnabled reports the operator's opt-in for EnvEnableRealPurchases.
// Fails closed: only an exact "1" or "true" (case-insensitive, surrounding
// whitespace trimmed) enables real purchases. Unset, empty, "0", "false", or
// any other value disables them -- there is no default-on value, matching
// the owner-decided default-off gate design.
func realPurchasesEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(EnvEnableRealPurchases)))
	return v == "1" || v == "true"
}

// decidePurchaseMode applies the safety rail from the design doc (§7): a
// real purchase requires confirm=true AND dry_run=false. dry_run=true always
// wins and returns a preview, regardless of confirm, so a caller previewing
// a purchase can leave confirm at its default. The only refusal case is
// dry_run=false with confirm=false: the caller asked for a real purchase
// but did not confirm it, which must surface as an explicit error rather
// than silently downgrading to a preview or silently doing nothing.
func decidePurchaseMode(dryRun, confirm bool) (purchaseMode, error) {
	if dryRun {
		return modePreview, nil
	}
	if confirm {
		return modeExecute, nil
	}
	return 0, fmt.Errorf("refusing real purchase: dry_run=false requires confirm=true (got confirm=false); " +
		"set dry_run=true to preview this purchase instead, or confirm=true to execute it")
}

// ResolveClientFunc lazily resolves the provider.ServiceClient that will
// receive the real PurchaseCommitment call. It is a func, not an
// already-resolved client, so ExecutePurchase can prove (and tests can
// assert) that a preview never triggers provider/credential resolution --
// only modeExecute invokes it.
type ResolveClientFunc func(ctx context.Context) (provider.ServiceClient, error)

// PurchaseRequest is the provider-agnostic input to ExecutePurchase. Each
// per-service tool handler builds one after validating its own typed
// parameters and constructing the common.Recommendation.
type PurchaseRequest struct {
	ResolveClient ResolveClientFunc
	Region        string

	// Nonce is optional. When non-empty, this call is treated as a
	// DISTINCT purchase from an otherwise-identical one (authorizes a
	// deliberate repeat, e.g. "buy 3 more RIs" on top of an earlier "buy 3
	// RIs" with the same parameters). When empty (the default), identical
	// purchases dedupe so retries never double-buy. See idempotencyKeyFor.
	Nonce string

	// CredentialScope identifies WHERE the purchase lands: the AWS profile,
	// Azure subscription, or GCP project the call is billed to. It is folded
	// into the idempotency key so two purchases that are identical in every
	// product dimension but target different accounts derive DIFFERENT
	// tokens. Real purchases include the same ambient fallback as provider
	// resolution; previews record only an explicit caller-supplied scope.
	// See idempotencyKeyFor for why omitting it is a double-spend/skipped-spend
	// hazard on Azure specifically.
	CredentialScope string

	// OfferingClass is the EC2 RI offering class (standard|convertible).
	// Empty for every other tool. It is price-affecting, so it is folded into
	// the idempotency key, echoed in the response, and passed to the provider.
	OfferingClass string

	Recommendation common.Recommendation
	DryRun         bool
	Confirm        bool
}

// ResolveDryRunConfirm applies the dry_run=true / confirm=false defaults to a
// tool's optional flags. Go's zero value for bool cannot distinguish "caller
// omitted the field" from "caller explicitly set it false", which is why both
// arrive as *bool; this is the single place that resolves them to concrete
// booleans.
//
// Shared by every purchase tool rather than reimplemented per tool. This is
// the gate that decides whether real money moves, so seven hand-copied
// versions of it were seven chances for one to drift: a copy that defaulted
// dryRun to false would turn an unconfirmed preview into a live purchase, and
// nothing but review would catch it.
func ResolveDryRunConfirm(dryRun, confirm *bool) (effectiveDryRun, effectiveConfirm bool) {
	// Absent dry_run means preview. Never the reverse.
	effectiveDryRun = true
	if dryRun != nil {
		effectiveDryRun = *dryRun
	}
	if confirm != nil {
		effectiveConfirm = *confirm
	}
	return effectiveDryRun, effectiveConfirm
}

// PurchaseResponse is the structured result returned to the MCP caller for
// both preview and real-purchase outcomes. Error is a string (not the Go
// error) because it crosses the MCP JSON-RPC boundary as tool output, not a
// protocol-level error -- ExecutePurchase itself still returns a Go error
// for gate refusals and provider-call failures.
//
// Executed Cost is the provider-reported total upfront cost: nil is omitted,
// while a known zero is retained. Unpriced recommendation fields are omitted.
//
// EffectiveDate is a pointer for the same reason, and it is not a
// hypothetical concern: `omitempty` on a string only drops "", so a
// PurchaseResult whose Timestamp was never populated formatted its zero
// time.Time to the literal "0001-01-01T00:00:00Z" and shipped that as the
// commitment's start date. That is a fabricated date presented as real, on
// a field a caller may key billing or renewal reminders off. Nil (and
// omitted) when the provider reported no timestamp.
type PurchaseResponse struct {
	Cost              *float64 `json:"cost,omitempty" jsonschema:"USD. Preview (Savings Plans only): total commitment over the whole term, not per hour. Executed purchase: provider-reported upfront cost. Omitted when unknown"`
	OnDemandCost      *float64 `json:"on_demand_cost,omitempty"`
	EstimatedSavings  *float64 `json:"estimated_savings,omitempty"`
	SavingsPercentage *float64 `json:"savings_percentage,omitempty"`
	EffectiveDate     *string  `json:"effective_date,omitempty"`

	// Archera is the optional underutilization-insurance offer, populated
	// ONLY after a real purchase actually succeeds. See archeraOffer.
	Archera *ArcheraOffer `json:"archera,omitempty"`

	CommitmentID string `json:"commitment_id,omitempty"`
	Error        string `json:"error,omitempty"`
	// OfferingClass echoes the EC2 RI offering class so the user sees it in
	// the preview before buying; omitted for other products.
	OfferingClass string `json:"offering_class,omitempty"`
	TermYears     int    `json:"term_years,omitempty"`
	Success       bool   `json:"success"`
	DryRun        bool   `json:"dry_run"`
}

// ArcheraOffer is the post-purchase Archera underutilization-insurance offer.
//
// Both disclosure fields are non-optional parts of the payload, not
// decoration: CUDly commits to surfacing the sponsorship AND the fact that it
// works fully without Archera everywhere the signup link appears (the CLI's
// printArcheraPitch does the same after a real purchase). An MCP client
// renders this JSON through a model, so shipping the link without the
// disclosures alongside it would let the model present a sponsored
// recommendation as a neutral one.
type ArcheraOffer struct {
	// Pitch is what the coverage does and why it is relevant right now.
	Pitch string `json:"pitch"`
	// SignupURL is the Archera signup link carrying CUDly attribution.
	SignupURL string `json:"signup_url"`
	// NonGatingDisclosure states the offer is optional and CUDly works
	// without it.
	NonGatingDisclosure string `json:"non_gating_disclosure"`
	// SponsorshipDisclosure states the financial relationship behind the
	// recommendation.
	SponsorshipDisclosure string `json:"sponsorship_disclosure"`
	// EnrollmentWindowDays is how long from THIS purchase the buyer has to
	// enroll it. Surfaced as a number so a client can compute the deadline
	// from effective_date rather than parsing it out of prose.
	EnrollmentWindowDays int `json:"enrollment_window_days"`
}

// archeraOffer builds the post-purchase Archera offer.
//
// Deliberately NOT attached to a preview: dry_run contacts no provider and
// buys nothing, so there is no commitment to insure and no enrollment window
// running. Deliberately NOT attached to a failed purchase either, for the
// same reason. This mirrors the CLI, which calls printArcheraPitch only under
// `else if riSuccess > 0` (cmd/multi_service_stats.go).
func archeraOffer() *ArcheraOffer {
	return &ArcheraOffer{
		Pitch:                 common.ArcheraPitch,
		SignupURL:             common.ArcheraSignupURL,
		EnrollmentWindowDays:  common.ArcheraEnrollmentWindowDays,
		NonGatingDisclosure:   common.ArcheraNonGatingDisclosure,
		SponsorshipDisclosure: common.ArcheraSponsorshipDisclosure,
	}
}

// termYearsFromRecommendationTerm extracts the integer commitment length in
// years from a Recommendation.Term string in the "<N>yr" format every
// *FromArgs constructor in this package writes via
// TermYears.RecommendationTerm() (enums.go). Returns 0 when term does not
// match that format (e.g. an empty Term), so PurchaseResponse.TermYears is
// simply omitted (it has `omitempty`) rather than reporting a fabricated
// value.
func termYearsFromRecommendationTerm(term string) int {
	years, err := strconv.Atoi(strings.TrimSuffix(term, "yr"))
	if err != nil {
		return 0
	}
	return years
}

// Recommendation's scalar cost and savings fields use zero for unpriced values.
func nonZeroCostPtr(v float64) *float64 {
	if v == 0 {
		return nil
	}
	return &v
}

// rfc3339OrNil formats t as RFC3339, or returns nil when t is the zero
// time.Time. This is the time analog of nonZeroCostPtr and exists for the
// same reason: common.PurchaseResult.Timestamp is a plain time.Time that not
// every provider client populates, and formatting an unset one yields the
// literal "0001-01-01T00:00:00Z" rather than an empty string, so
// `omitempty` on a string field could never drop it. Returning nil keeps
// "the provider did not tell us when this starts" distinguishable from a
// real date (feedback_nullable_not_zero).
func rfc3339OrNil(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.Format(time.RFC3339)
	return &s
}

func purchaseSucceeded(result common.PurchaseResult) bool {
	return result.Success && result.Error == nil
}

// logPurchaseAttempt and logPurchaseOutcome write the MCP server's stderr
// diagnostic trail for real, money-spending purchases. These lines are the
// live human-readable counterpart to the server's durable JSONL audit log.
//
// They go to the standard logger, which writes to STDERR. That is load
// bearing: the MCP stdio transport owns stdout for JSON-RPC framing, so
// anything written there would corrupt the protocol stream.
//
// Preview calls deliberately omit purchase attempt/outcome diagnostics on
// stderr: they contact no provider and spend nothing, and logging every dry
// run would bury real purchases. recordPurchaseAudit still runs for each
// preview; audit path or write failures may emit an "mcp audit log" warning.
//
// The idempotency token is masked (common.MaskToken) rather than written in
// full, matching how every provider client logs it: it is a stable
// per-request identifier, and the prefix is enough to correlate an attempt
// with its outcome.
func logPurchaseAttempt(req PurchaseRequest, rec common.Recommendation, token string) {
	log.Printf("mcp purchase ATTEMPT: provider=%s scope=%q region=%s service=%s resource=%s count=%d term=%s payment=%s token=%s",
		rec.Provider, req.CredentialScope, req.Region, rec.Service, rec.ResourceType, rec.Count,
		rec.Term, rec.PaymentOption, common.MaskToken(token))
}

const providerFailureWithoutDetail = "provider reported failure with no error detail"

// logPurchaseOutcome logs FAILED whenever success is false, even if err is
// nil: a provider can report PurchaseResult{Success: false, Error: nil} (no
// Go error, no result.Error, just a plain "did not buy anything"), and that
// case must never be misrecorded as "mcp purchase OK" in the audit trail.
func logPurchaseOutcome(rec common.Recommendation, token, commitmentID string, success bool, err error) {
	if !success {
		if err == nil {
			err = errors.New(providerFailureWithoutDetail)
		}
		label := "FAILED"
		if errors.Is(err, common.ErrOutcomeUnknown) {
			label = "OUTCOME UNKNOWN"
		}
		log.Printf("mcp purchase %s: provider=%s resource=%s token=%s: %v",
			label, rec.Provider, rec.ResourceType, common.MaskToken(token), err)
		return
	}
	log.Printf("mcp purchase OK: provider=%s resource=%s count=%d commitment_id=%s token=%s",
		rec.Provider, rec.ResourceType, rec.Count, commitmentID, common.MaskToken(token))
}

// purchaseLimits are the operator limits read before any credential use.
type purchaseLimits struct {
	usd    float64 // per-call USD cap
	budget float64 // cumulative per-process USD budget
}

// authorizeRealPurchase runs every gate a real purchase must clear, in the
// order that touches the least. It is only ever reached with mode ==
// modeExecute (ExecutePurchase returns the preview before calling it), so a
// dry run clears none of these and needs none of them.
//
// Gathered into one function rather than inlined so ExecutePurchase stays
// under the repo's gocyclo:10 pre-commit gate as gates accumulate, and so
// "what must be true before this server spends money" has a single place to
// read. Order matters: both the operator opt-in and the credential-scope
// check run before ResolveClient, so a refusal never resolves credentials or
// contacts a provider.
func authorizeRealPurchase(req PurchaseRequest, rec common.Recommendation) (purchaseLimits, error) {
	// Operator authorization. See EnvEnableRealPurchases for why the
	// model-supplied confirm flag alone is not enough.
	if !realPurchasesEnabled() {
		return purchaseLimits{}, fmt.Errorf("real purchases are disabled: set %s=1 to allow the MCP server to execute real purchases",
			EnvEnableRealPurchases)
	}
	// Operator spend ceiling, per call. See enforceSpendCaps.
	if err := enforceSpendCaps(rec); err != nil {
		return purchaseLimits{}, err
	}
	// The USD cap is read here, before any credential or network use; the
	// total itself needs the provider client, so it is checked in ExecutePurchase.
	usdLimit, err := readUSDCap()
	if err != nil {
		return purchaseLimits{}, err
	}
	budget, err := readBudget()
	if err != nil {
		return purchaseLimits{}, err
	}
	// Target account must be determinable. See requireCredentialScope for why
	// an undeterminable one is a double-purchase hazard, not a cosmetic gap.
	if err := requireCredentialScope(rec.Provider, req.CredentialScope); err != nil {
		return purchaseLimits{}, err
	}
	if req.ResolveClient == nil {
		return purchaseLimits{}, fmt.Errorf("internal error: no ResolveClient configured for real purchase")
	}
	return purchaseLimits{usd: usdLimit, budget: budget}, nil
}

// preflightRealPurchase runs every gate that can refuse a real purchase
// before PurchaseCommitment is called: authorizeRealPurchase, client
// resolution, the USD cap and the cumulative budget. Any error it returns is
// a refusal, and ExecutePurchase audits it once via recordPurchaseRefusal.
func preflightRealPurchase(ctx context.Context, req PurchaseRequest, rec common.Recommendation) (provider.ServiceClient, *budgetReservation, error) {
	limits, err := authorizeRealPurchase(req, rec)
	if err != nil {
		return nil, nil, err
	}
	client, err := req.ResolveClient(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve %s service client: %w", rec.Provider, err)
	}
	usd, err := enforceUSDCap(ctx, client, rec, req.OfferingClass, limits.usd)
	if err != nil {
		return nil, nil, err
	}
	reservation, err := reserveBudget(usd, limits.budget)
	if err != nil {
		return nil, nil, err
	}
	log.Printf("mcp budget: reserved %.2f of %.2f USD (committed %.2f)", usd, limits.budget, reservation.committed)
	return client, reservation, nil
}

// recordPurchaseRefusal is the single place a pre-purchase refusal leaves a
// trace: one stderr line and one audit record with status "refused", the
// refusal text in error_message and the requested scope in the recommendation
// fields. Free-form values are %q-quoted so one refusal stays on one stderr
// line. Nothing was bought, so no commitment ID or provider result exists.
func recordPurchaseRefusal(req PurchaseRequest, rec common.Recommendation, reason error) {
	log.Printf("mcp purchase REFUSED: provider=%s scope=%q region=%q service=%q resource=%q count=%d term=%q payment=%q: %q",
		rec.Provider, req.CredentialScope, req.Region, rec.Service, rec.ResourceType, rec.Count,
		rec.Term, rec.PaymentOption, reason.Error())
	recordPurchaseAudit(rec, req.CredentialScope, common.PurchaseResult{Error: reason}, auditStatusRefused, false)
}

// ExecutePurchase runs the shared dry_run/confirm safety gate, then every
// gate in authorizeRealPurchase (operator opt-in via EnvEnableRealPurchases,
// and a determinable target account), and for a real purchase that clears
// them all, resolves the service client and calls PurchaseCommitment with
// PurchaseSourceMCP and a derived idempotency token. It never calls
// ResolveClient in preview mode, so a preview makes zero provider/SDK calls;
// it also never calls ResolveClient when any authorizeRealPurchase gate
// refuses, so a disabled server -- or one that cannot tell which account a
// purchase would land in -- makes zero provider/SDK calls either.
func ExecutePurchase(ctx context.Context, req PurchaseRequest) (*PurchaseResponse, error) {
	mode, err := decidePurchaseMode(req.DryRun, req.Confirm)
	if err != nil {
		return nil, err
	}

	rec := req.Recommendation
	if mode == modePreview {
		recordPurchaseAudit(rec, req.CredentialScope, common.PurchaseResult{DryRun: true}, auditStatusSkipped, true)
		return &PurchaseResponse{
			Success:           true,
			DryRun:            true,
			Cost:              nonZeroCostPtr(rec.CommitmentCost),
			OnDemandCost:      nonZeroCostPtr(rec.OnDemandCost),
			EstimatedSavings:  nonZeroCostPtr(rec.EstimatedSavings),
			SavingsPercentage: nonZeroCostPtr(rec.SavingsPercentage),
			TermYears:         termYearsFromRecommendationTerm(rec.Term),
			OfferingClass:     req.OfferingClass,
		}, nil
	}

	client, reservation, refusal := preflightRealPurchase(ctx, req, rec)
	if refusal != nil {
		recordPurchaseRefusal(req, rec, refusal)
		return nil, refusal
	}

	token := common.DeriveIdempotencyToken(idempotencyKeyFor(req.Region, rec, req.CredentialScope, req.Nonce, req.OfferingClass), 0)
	requestedAt, err := firstAttemptTime(token, rec.Service)
	if err != nil {
		reservation.settle(false)
		recordPurchaseRefusal(req, rec, err)
		return nil, err
	}
	opts := common.PurchaseOptions{
		Source:           common.PurchaseSourceMCP,
		IdempotencyToken: token,
		OfferingClass:    req.OfferingClass,
		RequestedAt:      requestedAt,
	}

	return purchaseAndSettle(ctx, req, client, opts, reservation)
}

// purchaseAndSettle makes the purchase call, logs and audits it, and settles
// the budget reservation. Once PurchaseCommitment has been invoked the
// reservation stays counted unless purchaseSpent says nothing was bought.
func purchaseAndSettle(ctx context.Context, req PurchaseRequest, client provider.ServiceClient, opts common.PurchaseOptions, reservation *budgetReservation) (*PurchaseResponse, error) {
	rec, token := req.Recommendation, opts.IdempotencyToken
	logPurchaseAttempt(req, rec, token)

	result, err := client.PurchaseCommitment(ctx, rec, opts)
	reservation.settle(purchaseSpent(result, err))
	if err != nil {
		logPurchaseOutcome(rec, token, "", false, err)
		recordPurchaseAudit(rec, req.CredentialScope, common.PurchaseResult{Error: err}, auditStatusForError(err), false)
		// Full provider error text surfaces to the caller (feedback:
		// providers must never swallow the underlying SDK/HTTP error).
		return nil, fmt.Errorf("purchase commitment failed: %w", err)
	}
	auditResult := result
	if !purchaseSucceeded(auditResult) && auditResult.Error == nil {
		auditResult.Error = errors.New(providerFailureWithoutDetail)
	}
	logPurchaseOutcome(rec, token, auditResult.CommitmentID, purchaseSucceeded(auditResult), auditResult.Error)
	recordPurchaseAudit(rec, req.CredentialScope, auditResult, auditStatusFor(auditResult), false)

	resp := purchaseResponse(rec, result)
	resp.OfferingClass = req.OfferingClass
	return resp, nil
}

// purchaseResponse maps a provider result to the tool response.
func purchaseResponse(rec common.Recommendation, result common.PurchaseResult) *PurchaseResponse {
	resp := &PurchaseResponse{
		Success:           purchaseSucceeded(result),
		DryRun:            result.DryRun,
		CommitmentID:      result.CommitmentID,
		Cost:              result.Cost,
		OnDemandCost:      nonZeroCostPtr(rec.OnDemandCost),
		EstimatedSavings:  nonZeroCostPtr(rec.EstimatedSavings),
		SavingsPercentage: nonZeroCostPtr(rec.SavingsPercentage),
		EffectiveDate:     rfc3339OrNil(result.Timestamp),
		TermYears:         termYearsFromRecommendationTerm(rec.Term),
	}
	if result.Error != nil {
		resp.Error = result.Error.Error()
	}
	// Offer the insurance only once there is a real commitment to insure and
	// the enrollment window has actually started. A provider that reports
	// Success=false (or carries an Error) bought nothing, so pitching a
	// 7-day window against a purchase that did not happen would be wrong on
	// the facts, not merely premature.
	if purchaseSucceeded(result) {
		resp.Archera = archeraOffer()
	}
	return resp
}
