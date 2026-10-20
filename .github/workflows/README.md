# GitHub Actions Workflows

CI and release workflows for the cloud-commitments-mcp server. CI needs no cloud
credentials; `snyk-scan` reads the `SNYK_TOKEN` secret.

| Workflow | Trigger | Purpose |
|----------|---------|---------|
| `ci.yml` | PR and push to `main`/`develop`, manual | Lint, test, build and scan |
| `pre-commit.yml` | PR and push to `main` | Runs the repo's pre-commit hooks |
| `mcp-server-json.yml` | PRs touching `server.json` or its validator, `v*` tags, manual | Validates `server.json` |
| `release.yml` | `v*` tag push | Builds and publishes a release |

## ci.yml

Jobs: `lint` (golangci-lint, `go vet`, cyclomatic complexity), `workflow-lint` (actionlint,
zizmor), `unit-tests` (coverage check, warns under 80%), `mcp-build` (builds
`bin/cudly-mcp` on an OS matrix), `integration-tests`, `security-scan` (gitleaks, govulncheck, gosec,
Trivy) and `snyk-scan`.

The `ci-success` job aggregates `lint`, `workflow-lint`, `unit-tests`, `integration-tests`,
`security-scan` and `mcp-build`. It fails unless each of them reports exactly `success`, so a
skipped job cannot pass the gate. `snyk-scan` is not in that list. It skips fork PRs, and its
step is `continue-on-error`. Use `ci-success` as the required status check.

## pre-commit.yml

Installs the hook tooling (gocyclo, Trivy, git-secrets, pre-commit), self-tests the
git-secrets allowlist, then runs `pre-commit` against `.pre-commit-config.yaml`.

## mcp-server-json.yml

`validate-schema` checks `server.json` against the MCP Registry schema. On `v*` tags,
`assert-version-matches-tag` also requires the `server.json` version to equal the tag.

## release.yml

Pushing a `v*` tag runs, in order: `consistency-gate` (`server.json` and
`mcpb/manifest.json` versions match the tag), `test`, `goreleaser` (binaries and GitHub
Release), `mcpb` (bundle asset) and `publish-registry` (MCP Registry via GitHub OIDC).
Only one run per ref at a time. The publishing jobs use the `release` environment.

## Running the checks locally

```bash
make build            # bin/cudly-mcp
make test-unit
make vet
make lint
make ci               # fmt, vet, complexity, test-unit, security-scan
pip install pre-commit && pre-commit install   # then: pre-commit run --all-files
```
