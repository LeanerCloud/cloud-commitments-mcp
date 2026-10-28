# Changelog

All notable changes to the CUDly MCP server are documented in this file.
The format is based on [Keep a Changelog](https://keepachangelog.com/).

## [Unreleased]

### Added

- `cudly_aws_ec2_ri_purchase`: a `partial-upfront` or `all-upfront` dry run
  looks up the offering (read-only) and returns its total upfront as
  `approved_upfront` and `approved_currency`. The confirming call must pass
  both back; it is refused without them, and the purchase is refused when the
  offering's upfront exceeds `approved_upfront` by more than 5% (go#611).
  `no-upfront` purchases are unchanged. Real upfront EC2 audit records now carry
  the approved upfront in `estimated_cost`.
- Every real purchase passes `PurchaseOptions.RequestedAt`, the first-dispatch
  time of its idempotency key, recorded before the provider call and reused on
  retries (go#515: Redshift finds an earlier purchase by start time). With
  `CUDLY_MCP_BUDGET_STATE` set it is persisted in `<state>.attempts` (30-day
  retention; a corrupt or unwritable file refuses the purchase). Without it the
  time is per process, and Redshift purchases are refused because their dedupe
  must survive a restart.
- `cudly_aws_ec2_ri_purchase` takes an optional `offering_class` (`standard` or
  `convertible`, default `convertible`). It is echoed in the response and
  recorded in the audit log (`raw_recommendation`).
- `CUDLY_MCP_BUDGET_STATE`: optional state file that persists the cumulative
  purchase budget per UTC calendar month, survives restarts and is shared by
  processes using the same path (flock, atomic writes). A corrupt or unreadable
  file refuses real purchases. Unset keeps the in-memory budget. Reset by
  deleting the file with all servers stopped.
- The purchase audit log records `status: "refused"` (reason in `error_message`) and
  stderr logs `mcp purchase REFUSED` for every real purchase refused before the
  provider call: disabled, spend limits, missing scope, client resolution or
  pricing failure.
- `CUDLY_MCP_BUDGET_USD`: a cumulative per-process USD purchase budget. It
  is in memory (resets on restart, not shared, blind to purchases made
  elsewhere); unknown outcomes count as spent.
- `cudly_archera_comparison`: a read-only comparison of an Archera commitment
  plan (current offer and alternatives per line item, plus plan-wide
  hypotheticals), with exact decimal money, unknown as null, and both Archera
  disclosures. Off until the operator sets `ARCHERA_API_KEY`, `ARCHERA_ORG_ID`
  and `ARCHERA_PLAN_ID`; unconfigured calls error and send no request. It
  reads `https://api.archera.ai` only, never retries, and makes no purchase.

- `CUDLY_MCP_MAX_COMMITMENT_USD`: a per-call ceiling on the total USD one
  real purchase commits over its full term, priced from the provider's own
  offering lookup (Savings Plans: hourly commitment x 8760 x years). USD only;
  unpriceable purchases are refused.

### Security

- Bump Go to 1.26.10 to pick up the fix for GO-2026-6809 (crypto/x509), which `govulncheck` now reports against 1.26.9.
- A small `CUDLY_MCP_MAX_COUNT` no longer admits an enormous purchase (50 x
  `u-24tb1` is about $286M): the USD cap bounds the real total.

- `cudly_archera_comparison`: a transport error that echoed the API key with
  zero-width, bidi or control characters spliced into it could reassemble the
  plain key in the tool error, because the key was masked before those
  characters were stripped. Text is now stripped, then masked, then capped. A
  transport error wrapping a context error is sanitized the same way (it
  previously passed through unchanged), and error text is withheld when the
  key is still readable after ignoring combining marks, spaces, variation
  selectors, Hangul fillers and invalid bytes.

### Changed

- Bumped the `cloud-commitments-go` pins to 8785de4 for the EC2 approved-upfront
  guard (go#611). `provider.ProviderConfig` is now a deprecated alias of
  `provider.Config`; the call sites use `provider.Config`. Behavior pulled in:
  RDS, MemoryDB, OpenSearch and Redshift purchases gain the same guard, which
  stays inactive for MCP purchases because those tools set no approved upfront;
  Azure reservation and savings plan purchases check the billing currency
  (an empty recommendation currency means USD).
- Bumped the `cloud-commitments-go` pins to 1ce87bd. `computeengine.NewClient` no
  longer takes a context (go#575); the GCP test call sites follow. Behavior
  pulled in: Savings Plans in aws-cn are refused before any AWS call (go#566);
  Azure Savings Plans refuse a malformed idempotency token before the purchase
  PUT (e735fdb, go#495); Redshift purchases are deduped by DescribeReservedNodes
  and require `RequestedAt` (go#515, see Added); remaining AWS and MemoryDB
  pagination loops are capped (go#573); RDS reservations count against other
  sizes of the same family in dedupe (go#564).
- Bumped the `cloud-commitments-go` pins to main (9dae64b). `ARCHERA_API_KEY`
  must now be printable ASCII (0x21-0x7E): the client rejects any other byte
  and its error never echoes the key. Library decode errors now show only
  short identifier-shaped values and are capped.
- `.gitallowed` no longer allow-lists bare placeholder account IDs and UUIDs,
  which exempted any line containing them from every detector (#24). The one
  fixture the account-ID detector flags is allowed by a path- and line-anchored
  entry, and the allowlist self-test covers the bypass.
- The purchase audit log records a new status, `unknown`, for a purchase that
  failed with `common.ErrOutcomeUnknown` (a lost EC2 or Redshift response: the
  commitment may exist). The stderr line reads `OUTCOME UNKNOWN`. Consumers
  of the JSONL that switch on `status` must handle the new value.
- `cudly_search_recommendations` now checks results against commitments bought
  in the last 24 hours and returns a `dedupe` block: fully covered RDS,
  OpenSearch, Redshift and ElastiCache recommendations are suppressed and
  listed, possibly covered EC2, Azure, GCP and MemoryDB ones are flagged.
  The search now needs permission to list existing commitments and fails
  loudly, naming it, when it cannot.

### Breaking

- **`CUDLY_MCP_BUDGET_USD` is now required for every real purchase.**
  Unset, empty or invalid refuses the purchase before credentials are
  resolved. The MCPB bundle has a matching setting that must be filled in.
- **`CUDLY_MCP_MAX_COMMITMENT_USD` is now required for every real purchase.**
  Unset, empty or invalid refuses the purchase before credentials are
  resolved. The MCPB bundle has a matching setting that must be filled in before any real purchase. Executing a
  purchase now also needs credentials and network access for the provider's
  offering lookup.
- **Real GCP CUD purchases are refused** until the `cloud-commitments-go` pin
  includes the CUD pricing fix (go#321), because their USD total cannot be
  determined yet. Previews are unaffected.

- **Real purchases now require operator spend caps.** With
  `CUDLY_MCP_ENABLE_REAL_PURCHASES=1`, every real purchase is refused unless
  the relevant cap is set to a valid value: `CUDLY_MCP_MAX_COUNT` (RIs, and
  vCPUs for GCP CUDs), `CUDLY_MCP_MAX_HOURLY_COMMITMENT` (Savings Plans) and
  `CUDLY_MCP_MAX_MEMORY_GB` (GCP CUDs). Caps are per call; the count and
  hourly caps are not USD caps (see `CUDLY_MCP_MAX_COMMITMENT_USD`). The MCPB bundle exposes them as optional settings. Previews are
  unaffected. Savings Plan previews now report `cost` (total commitment over
  the term), and Savings Plan audit lines now carry that figure as the
  estimated cost.

### Fixed

- The search's dedupe failure suggests the missing IAM permission only when
  listing existing commitments was refused (AWS `AccessDenied`,
  `UnauthorizedOperation`; Azure or GCP 401/403). Other listing errors, such as
  a commitment with an unparseable date, show the underlying error without the
  permission hint. The search still fails on any listing error.
- The Claude Desktop settings for `budget_usd` and `max_commitment_usd` now
  use the inclusive min 0 (like the other float caps) instead of 1, so sub-dollar amounts are accepted, matching the server, which accepts any
  positive amount (mcp#82). Tests also pin that a panic or a cancelled context
  inside the provider purchase call keeps the budget reservation counted.
- `cudly_aws_ec2_ri_purchase` with `offering_class=standard` is now capped by
  `CUDLY_MCP_MAX_COMMITMENT_USD` and counted against `CUDLY_MCP_BUDGET_USD` at
  the standard offering's price (`GetOfferingDetailsForClass`, go#574), not the
  convertible one. A client that cannot price a class refuses a standard
  purchase instead of falling back to convertible pricing.
- The MCPB bundle no longer blocks sub-unit spend caps (for example 0.5 USD/h):
  the hourly-commitment and memory caps now declare `min` 0, and the server
  still rejects values `<= 0`.

- Bumped the four cloud-commitments-go modules to 8a3d92b, which brings these
  purchase safeguards: RDS purchases match the offering exactly and refuse
  Oracle and SQL Server (go#300); EC2 and Redshift purchases are sent once and
  a lost response reports "purchase outcome unknown" instead of retrying
  (go#297); EC2 rejects an empty or unknown tenancy or scope (go#210);
  OpenSearch matches reserved offerings exactly (go#191); re-driven purchases
  that adopt an existing commitment are flagged by the library (go#263, go#264;
  the MCP response shape is unchanged).
- The RDS purchase tool schema no longer advertises engines the library refuses.
- A `tools/call` with `"arguments": null` no longer crashes the server; it is
  treated as `{}` (#95).

- Align pre-commit gocyclo threshold (10) with CI pipeline
- Pin tool versions in GitHub Actions for reproducible builds
- The local git-secrets setup script aborted on its PEM pattern and, when
  patched past that, made every scan fail on an invalid regex; its keyword
  allowlist whitelisted whole lines containing common Go/Terraform tokens.
  Allowlisting now lives in `.gitallowed` as literal entries and the PEM
  detector covers PKCS#8 keys (#1972)
