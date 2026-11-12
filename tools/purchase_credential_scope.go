package tools

import (
	"fmt"
	"os"
	"strings"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
)

// CredentialScope resolves the account/subscription/project identifier that
// bounds where a purchase lands: the caller-supplied override when present,
// otherwise the first non-empty value among the ambient environment
// variables the matching provider factory itself consults (e.g.
// AZURE_SUBSCRIPTION_ID, providers/azure/provider.go's
// resolveDefaultSubscription).
//
// It returns "" when neither is set, which is correct rather than an error:
// a provider that resolves its account purely from ambient credentials
// (single visible Azure subscription, AWS STS identity, ambient GCP project)
// is unambiguous for the life of the server process, so there is no second
// scope for a token to collide with.
//
// Normalization here is limited to trimming, because it is the only rule that
// is safe for all three providers. Case is deliberately NOT folded: AWS named
// profiles are case-sensitive section names in ~/.aws/config, so lower-casing
// one could name a profile that does not exist or a different one that does.
// Azure subscription IDs ARE case-insensitive and do need folding -- see
// azureCredentialScope in azure_compute_ri.go, which wraps this.
func CredentialScope(explicit string, envVars ...string) string {
	if s := strings.TrimSpace(explicit); s != "" {
		return s
	}
	for _, env := range envVars {
		if s := strings.TrimSpace(os.Getenv(env)); s != "" {
			return s
		}
	}
	return ""
}

// requestCredentialScope resolves the audit/idempotency scope for the selected
// mode: previews must not record an ambient account they never resolved, while
// real purchases must keep the provider-env fallback used by authorization.
func requestCredentialScope(explicit string, dryRun bool, envVars ...string) string {
	if dryRun {
		return CredentialScope(explicit)
	}
	return CredentialScope(explicit, envVars...)
}

// credentialScopeSource describes where a provider's credential scope can come
// from: the tool argument that supplies it, and the ambient environment
// variable that can supply it instead, if the provider has one.
//
// envVar must name a variable that the purchase's provider configuration
// consumes (the matching factory itself for AWS and Azure, or the tool for
// GCP, which passes it on as the explicit project), so a scope inherited from
// the environment always identifies the account the purchase really lands in.
// A variable nothing consumes would make the token positively assert the wrong
// account (see credentialScopeSourceFor).
type credentialScopeSource struct {
	arg    string
	envVar string
}

// credentialScopeSourceFor returns how p's credential scope may be supplied,
// so requireCredentialScope's refusal can name exactly what the caller has to
// do. Unknown providers are an explicit error rather than a generic message: a
// provider added without an entry here would otherwise get a refusal it cannot
// act on (feedback_no_silent_fallbacks).
//
// GCP's variable is CUDly-namespaced (CUDLY_GCP_PROJECT_ID), not
// GOOGLE_CLOUD_PROJECT: the provider factory does not read it, so the MCP tool
// passes it to the provider as the explicit project, and the scope and the
// purchase target stay one value. Do not point this at a variable a hosting
// runtime sets implicitly.
func credentialScopeSourceFor(p common.ProviderType) (credentialScopeSource, error) {
	switch p {
	case common.ProviderAWS:
		return credentialScopeSource{arg: "aws_profile", envVar: "AWS_PROFILE"}, nil
	case common.ProviderAzure:
		return credentialScopeSource{arg: "azure_subscription_id", envVar: "AZURE_SUBSCRIPTION_ID"}, nil
	case common.ProviderGCP:
		return credentialScopeSource{arg: "gcp_project_id", envVar: gcpProjectEnvVar}, nil
	default:
		return credentialScopeSource{}, fmt.Errorf("internal error: no credential-scope source defined for provider %q", p)
	}
}

// requireCredentialScope refuses a REAL purchase whose credential scope could
// not be determined. Previews are unaffected (ExecutePurchase returns before
// calling this), so a caller can still price a purchase without naming an
// account.
//
// This exists because an empty scope is a DOUBLE-PURCHASE hazard, not merely
// an imprecise audit line. idempotencyKeyFor folds the scope into the token,
// and CredentialScope returns "" when neither the explicit argument nor the
// ambient environment variable is set. The same target account reached two
// ways therefore derives two DIFFERENT tokens:
//
//	omit azure_subscription_id (ambient resolves to sub-X) -> scope ""
//	pass azure_subscription_id="sub-X"                     -> scope "sub-X"
//
// Both purchase into sub-X, but every provider's dedupe is token-keyed --
// Azure's FindReservationOrderByIdempotencyToken, GCP's
// idempotentCommitmentName, and on AWS the EC2/Redshift tag lookups
// (findRIByIdempotencyToken), the RDS/ElastiCache/OpenSearch/MemoryDB
// idempotencyGuard, and Savings Plans' CreateSavingsPlanInput.ClientToken --
// so the second call's lookup misses and buys again. Previewing without the
// account and then re-calling with it explicit is ordinary self-correcting
// model behavior, which makes this a likely sequence rather than a corner
// case.
//
// The fix is to make "" unreachable for a real purchase. Note that explicit
// and ambient do NOT diverge once a value exists: CredentialScope falls back
// to the same environment variable the provider factory itself consults, so
// aws_profile="prod" and an ambient AWS_PROFILE=prod both yield "prod" and
// dedupe correctly. Only the both-absent case aliases, and this closes it.
//
// Deriving the effective account from the resolved client would be the
// stronger fix, but provider.ServiceClient exposes no account accessor
// (pkg/provider/interface.go), so that needs an interface change across every
// AWS/Azure/GCP service client and does not belong on this branch. Failing
// closed removes the hazard by construction in the meantime: on an
// LLM-driven money path, making the operator name the target account before
// spending is the same posture as EnvEnableRealPurchases.
func requireCredentialScope(p common.ProviderType, scope string) error {
	if strings.TrimSpace(scope) != "" {
		return nil
	}
	src, err := credentialScopeSourceFor(p)
	if err != nil {
		return err
	}
	return fmt.Errorf("refusing real purchase: the target account could not be determined, so pass %s explicitly "+
		"(or set %s). Leaving it to ambient credentials derives a different idempotency token than naming the "+
		"same account explicitly, so a retry would not dedupe and could purchase twice. Dry runs do not require it",
		src.arg, src.envVar)
}
