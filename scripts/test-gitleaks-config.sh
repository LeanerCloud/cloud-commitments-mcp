#!/bin/bash
# Self-test for .gitleaks.toml: each allowlisted fixture literal must scan
# clean, and a real-shaped secret planted in the same file must still fail
# (gitleaks 8.30.1 skips a whole file when an allowlist combines `paths` with
# condition = "AND"). Usage: GITLEAKS=/path/to/gitleaks bash scripts/test-gitleaks-config.sh
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GITLEAKS="${GITLEAKS:-gitleaks}"
command -v "$GITLEAKS" >/dev/null || { echo "gitleaks not found (set GITLEAKS)" >&2; exit 1; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# Assembled at runtime so this file itself scans clean.
PAT="ghp""_aB3dE5gH7jK9mN1pQ3sT5vW7yZ9bC1dE3fG5"

scan() { "$GITLEAKS" dir "$1" --config "$REPO_ROOT/.gitleaks.toml" --no-banner --redact --exit-code 1 >/dev/null 2>&1; }

pass=0 fail=0
check() { # label expected_rc dir
    local rc=0
    scan "$3" || rc=$?
    if [ "$rc" -eq "$2" ]; then pass=$((pass + 1)); else fail=$((fail + 1)); echo "FAIL: $1 (expected rc $2, got $rc)" >&2; fi
}

# Every allowlist entry: its path, and its literal (read back from the config).
i=0
while IFS='|' read -r path literal; do
    i=$((i + 1))
    d="$tmp/c$i"
    mkdir -p "$d/$(dirname "$path")"
    printf 'const token = "%s"\n' "$literal" >"$d/$path"
    check "fixture literal in $path is allowed" 0 "$d"
    printf 'const real = "%s"\n' "$PAT" >>"$d/$path"
    check "real-shaped PAT beside the fixture in $path is caught" 1 "$d"
done <<'ENTRIES'
.github/workflows/pre-commit.yml|ad82d68ee924906a0401dfd48de5057731a9bc84
tools/archera_error_mask_test.go|test-key-SYNTHETIC-0000
ENTRIES

# The real tree must be clean under the repo config.
check "repository tree is clean" 0 "$REPO_ROOT"

echo "$pass PASS, $fail FAIL"
[ "$fail" -eq 0 ]
