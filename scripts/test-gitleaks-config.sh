#!/bin/bash
# Self-test for .gitleaks.toml. gitleaks 8.30.1 skips a whole file when an
# allowlist uses `paths` (alone, or with condition = "AND"), so a real secret
# next to a fixture would go undetected. This test:
#   1. fails if the config has ANY paths/condition/stopwords/commits/regexTarget
#      key (the only allowed matcher is an exact-literal, ^...$ anchored regex);
#   2. proves that guard rejects an injected `paths` entry for an unrelated file;
#   3. derives every allowlisted literal from the config, finds the file(s) that
#      contain it, checks the literal scans clean there and that a planted
#      real-shaped PAT in the same file is still caught;
#   4. checks the real tree is clean under the config.
# Usage: GITLEAKS=/path/to/gitleaks bash scripts/test-gitleaks-config.sh
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONFIG="$REPO_ROOT/.gitleaks.toml"
GITLEAKS="${GITLEAKS:-gitleaks}"
command -v "$GITLEAKS" >/dev/null || { echo "gitleaks not found (set GITLEAKS)" >&2; exit 1; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# Assembled at runtime so this file itself scans clean.
PAT="ghp""_aB3dE5gH7jK9mN1pQ3sT5vW7yZ9bC1dE3fG5"

pass=0 fail=0
ok() { pass=$((pass + 1)); }
bad() { fail=$((fail + 1)); echo "FAIL: $1" >&2; }

# Non-comment lines that set a key able to skip whole files or narrow matching.
forbidden() { grep -nE '^[[:space:]]*(paths|condition|stopwords|commits|targetRules|regexTarget)[[:space:]]*=' "$1" || true; }

scan() { "$GITLEAKS" dir "$1" --config "$2" --no-banner --redact --exit-code 1 >/dev/null 2>&1; }
check() { # label expected_rc dir config
    local rc=0
    scan "$3" "$4" || rc=$?
    if [ "$rc" -eq "$2" ]; then ok; else bad "$1 (expected rc $2, got $rc)"; fi
}

# 1. The repo config must not use any forbidden key.
if [ -n "$(forbidden "$CONFIG")" ]; then bad ".gitleaks.toml uses a forbidden key: $(forbidden "$CONFIG")"; else ok; fi

# 2. The guard must catch an injected `paths` allowlist for an unrelated file.
cp "$CONFIG" "$tmp/injected.toml"
printf '\n[[allowlists]]\ndescription = "injected"\npaths = [%s]\n' "'''tools/purchase\\.go'''" >>"$tmp/injected.toml"
if [ -n "$(forbidden "$tmp/injected.toml")" ]; then ok; else bad "guard missed an injected paths allowlist"; fi

# 3. Every allowlisted literal, read back from the config, in each file holding it.
i=0
while IFS= read -r literal; do
    files=$(grep -rlF --exclude-dir=.git --exclude=.gitleaks.toml --exclude=test-gitleaks-config.sh -- "$literal" "$REPO_ROOT" || true)
    [ -n "$files" ] || { bad "allowlisted literal $literal appears in no file (stale entry)"; continue; }
    while IFS= read -r file; do
        i=$((i + 1))
        rel="${file#"$REPO_ROOT"/}"
        d="$tmp/c$i"
        mkdir -p "$d/$(dirname "$rel")"
        printf 'const token = "%s"\n' "$literal" >"$d/$rel"
        check "fixture literal in $rel is allowed" 0 "$d" "$CONFIG"
        printf 'const real = "%s"\n' "$PAT" >>"$d/$rel"
        check "real-shaped PAT beside the fixture in $rel is caught" 1 "$d" "$CONFIG"
    done <<<"$files"
done < <(sed -nE "s/^[[:space:]]*regexes[[:space:]]*=[[:space:]]*\\['''\\^(.*)\\\$'''\\][[:space:]]*\$/\\1/p" "$CONFIG")
[ "$i" -gt 0 ] || bad "no allowlisted literals found in the config"

# 4. The real tree must be clean under the repo config.
check "repository tree is clean" 0 "$REPO_ROOT" "$CONFIG"

echo "$pass PASS, $fail FAIL"
[ "$fail" -eq 0 ]
