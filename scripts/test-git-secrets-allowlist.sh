#!/bin/bash
# Self-test for the git-secrets allowlist (#1972). Exercises the real
# scripts/setup-git-secrets.sh and .gitallowed in a throwaway repo, both
# directly via `git secrets --scan --cached` and through the installed
# pre-commit hook, in both directions: fixtures that must be caught, and
# fixtures that must scan clean.
#
# Fixture safety: every positive fixture below is a secret-shaped string
# that this script itself, and the repo's detect-private-key / git-secrets
# pre-commit hooks, would flag if it appeared as a contiguous literal. Each
# is assembled at runtime from adjacent string literals so this file's own
# source never contains a scannable token. Where a fixture shows \n below,
# it is the two literal characters backslash and n, as in a Go string
# literal, never an actual newline.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if ! command -v git-secrets >/dev/null 2>&1; then
    echo "git-secrets not found on PATH; install it before running this test" >&2
    exit 1
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/home"
# Isolate HOME: --register-aws reads ~/.aws/credentials, and git-secrets
# echoes the whole joined pattern list on a regex error, which would print
# real credentials into this log.
export HOME="$tmp/home"

git init -q "$tmp/repo"
mkdir -p "$tmp/repo/scripts"
cp "$REPO_ROOT/scripts/setup-git-secrets.sh" "$tmp/repo/scripts/setup-git-secrets.sh"
cp "$REPO_ROOT/.gitallowed" "$tmp/repo/.gitallowed"
cd "$tmp/repo"

if ! bash scripts/setup-git-secrets.sh > "$tmp/setup.log" 2>&1; then
    echo "scripts/setup-git-secrets.sh failed in the throwaway repo:" >&2
    cat "$tmp/setup.log" >&2
    exit 1
fi
# Same HOME-isolation reason: drop the provider before any further scan.
git config --unset-all secrets.providers

# The pre-commit hook ignores the args git would pass it and recomputes its
# own file list from the index (diff against HEAD, or the empty tree if
# there is no HEAD yet, which is always true in this never-committed
# throwaway repo). So it scans every currently staged path, not just
# $relpath -- calling it with no args, as git itself does, is correct.
HOOK_DIR="$(git rev-parse --git-dir)/hooks"

pass=0
fail=0

# Records one pass/fail against $expected, printing a FAIL line labeled
# $label on mismatch. Shared by both scan paths in run_case/run_staged_case
# so a direct-scan/hook disagreement surfaces as its own labeled failure
# instead of being silently reconciled.
check_result() {
    local label=$1 expected=$2 actual=$3
    if [ "$actual" -eq "$expected" ]; then
        pass=$((pass + 1))
    else
        fail=$((fail + 1))
        echo "FAIL: $label (expected exit $expected, got $actual)" >&2
    fi
}

# Writes $content to $relpath, stages it, scans it two ways -- directly via
# `git secrets --scan --cached` (exact file argument) and via the installed
# pre-commit hook (the real path a developer's commit takes) -- compares
# each exit code to $expected, then unstages and removes it.
run_case() {
    local label=$1 expected=$2 relpath=$3 content=$4
    mkdir -p "$(dirname "$relpath")"
    printf '%s\n' "$content" >"$relpath"
    git add "$relpath"
    local actual=0
    git secrets --scan --cached "$relpath" >/dev/null 2>&1 || actual=$?
    check_result "$label (direct scan)" "$expected" "$actual"
    local hook_actual=0
    "$HOOK_DIR/pre-commit" >/dev/null 2>&1 || hook_actual=$?
    check_result "$label (installed hook)" "$expected" "$hook_actual"
    git rm -q --cached "$relpath"
    rm -f "$relpath"
}

# Same as run_case, for a file already on disk (the copied setup script and
# .gitallowed), left staged afterward like any other tracked file would be.
run_staged_case() {
    local label=$1 expected=$2 relpath=$3
    git add "$relpath"
    local actual=0
    git secrets --scan --cached "$relpath" >/dev/null 2>&1 || actual=$?
    check_result "$label (direct scan)" "$expected" "$actual"
    local hook_actual=0
    "$HOOK_DIR/pre-commit" >/dev/null 2>&1 || hook_actual=$?
    check_result "$label (installed hook)" "$expected" "$hook_actual"
}

# Fixtures, assembled from adjacent literals so this file scans clean.
KEY="AKIA""0123456789ABCDEF"
PEM="-----BEGIN ""PRIVATE KEY-----"
AIZA="AIza$(head -c 35 /dev/zero | tr '\0' X)"

# Must be caught (exit 1): the #1972 hole and the coverage this PR adds.
run_case "terraform resource with a real-shaped key" 1 a.tf \
    'resource "aws_iam_access_key" "x" { key = "'"$KEY"'" }'
run_case "terraform var reference alongside a key" 1 b.tf \
    'secret = var.x # '"$KEY"
run_case "go test const with a key" 1 c_test.go \
    'const k = "'"$KEY"'"'
run_case "testdata fixture with a key" 1 testdata/creds.txt \
    'key='"$KEY"
run_case "placeholder line with a key" 1 d.txt \
    'placeholder '"$KEY"
run_case "untruncated PKCS8 body" 1 e.json \
    '"private_key": "'"$PEM"'\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC\n"'
run_case "corrected GCP API key range" 1 f.txt \
    "$AIZA"
run_case "password with real spaces around the separator" 1 n.txt \
    'pass''word = "hunter2hunter2"'
run_case "secret before an otherwise-allowed truncated PEM, same line" 1 l.go \
    'k := "'"$KEY"'"; PrivateKey: "'"$PEM"'\n...",'
run_case "secret after an otherwise-allowed truncated PEM, same line" 1 m.go \
    '"private_key": "'"$PEM"'\nMIIEvQIBADANBg...\n'"$KEY"'"'

# Bare placeholder digits/UUID must not exempt a line from the other detectors
# (#24): each line below carries a fixture that is caught on its own.
run_case "key beside a placeholder account ID" 1 o.txt \
    'acct 123456789012 key '"$KEY"
run_case "password beside a placeholder account ID" 1 p.txt \
    'account''_id = "123456789012" // pass''word = "hunter2hunter2"'
run_case "GCP key beside a placeholder UUID" 1 q.txt \
    "$AIZA 11111111-1111-1111-1111-111111111111"
# Live-detector control: a non-placeholder account ID is flagged, so the
# clean cases below are not passing because the detector is dead.
run_case "account ID assignment is still detected" 1 r.txt \
    'account''_id="123456789013"'

# DSN detectors (#58): git-secrets scans with -w, so a pattern ending in the
# credential separator never matched; each of these must be caught.
DSN_TAIL="://app:S3cretPw@db.example.com/app"
run_case "postgres DSN with a password" 1 v1.txt \
    'dsn: postgre''s'"$DSN_TAIL"
run_case "postgresql DSN with a password" 1 v2.txt \
    'dsn: postgre''sql'"$DSN_TAIL"
run_case "mysql DSN with a password" 1 v3.txt \
    'dsn: mysq''l'"$DSN_TAIL"
run_case "mongodb DSN with a password" 1 v4.txt \
    'dsn: mongo''db'"$DSN_TAIL"
run_case "mongodb+srv DSN with a password" 1 v5.txt \
    'dsn: mongo''db+srv'"$DSN_TAIL"
run_case "DSN with a percent-encoded password" 1 v6.txt \
    'dsn: postgre''s://app:p%40ss%3Aw0rd@db.example.com/app'
run_case "DSN with an IPv6 host" 1 v7.txt \
    'dsn: postgre''s://app:S3cretPw@[::1]:5432/app'
run_case "quoted DSN" 1 v8.txt \
    'url = "postgre''s'"$DSN_TAIL"'"'
# The POSIX-class twins: git grep on macOS has no \s (#58).
run_case "spaced account ID assignment" 1 w1.txt \
    'account''_id = "123456789013"'
run_case "camelCase account ID with a space" 1 w2.txt \
    'account''Id: "123456789013"'
run_case "spaced AWS secret access key assignment" 1 w3.txt \
    'aws_secret_access''_key = '"$(head -c 40 /dev/zero | tr '\0' a)"

# DSN lookalikes that must stay clean.
run_case "DSN with a shell-variable password" 0 x1.txt \
    'dsn: postgre''s://app:${DB_PASS}@db.example.com/app'
run_case "DSN with a placeholder password" 0 x2.txt \
    'dsn: postgre''s://app:<password>@db.example.com/app'
run_case "DSN without a password" 0 x3.txt \
    'dsn: postgre''s://app@db.example.com/app'
run_case "http URL with userinfo is not a database DSN" 0 x4.txt \
    'see https://user:pw@example.com/'

# Negative controls for the setup-script allowlist entries below, each
# closing one instance of the same class of hole (#1972 reintroduced at
# the scale of one file) that a narrower-but-still-wrong anchor leaves
# open. Both mutate the tracked copy of the setup script in place at its
# real path, since the allowlist entries are anchored on that path, and
# restore the pristine copy afterward so the later self-scan case below
# sees it intact.

# A secret appended to a DIFFERENT git-secrets registration line in that
# file (the GCP one, not one of the three that legitimately self-match)
# must still be caught. A prefix-only entry anchored on just "git secrets
# --add '" would hide this.
run_case "secret appended to an unrelated registration line stays caught" 1 \
    scripts/setup-git-secrets.sh \
    "$(sed "s/# GCP API Key\$/# GCP API Key ${KEY}/" "$REPO_ROOT/scripts/setup-git-secrets.sh")"
cp "$REPO_ROOT/scripts/setup-git-secrets.sh" scripts/setup-git-secrets.sh

# A secret appended to the END of one of the three self-matching lines
# THEMSELVES must also still be caught. An entry anchored on the path and
# pattern but missing a trailing $ would stop matching at the end of the
# pattern it names and let anything appended after it through, which is a
# narrower version of the same hole the entry above closes.
run_case "secret appended to a self-matching line itself stays caught" 1 \
    scripts/setup-git-secrets.sh \
    "$(sed "s/# Azure Connection String\$/# Azure Connection String ${KEY}/" "$REPO_ROOT/scripts/setup-git-secrets.sh")"
cp "$REPO_ROOT/scripts/setup-git-secrets.sh" scripts/setup-git-secrets.sh

# Must scan clean (exit 0): what the tree contains, and what the allowlist
# must keep clean.
run_staged_case "setup script's own self-matching lines" 0 scripts/setup-git-secrets.sh
run_case "GCP type marker alone, no key material" 0 g.json \
    '{"type": "service_account", "project_id": "p"}'
run_case "truncated PEM in a struct literal" 0 i.go \
    'PrivateKey: "'"$PEM"'\n...",'
run_case "truncated PEM in a JSON literal" 0 j.go \
    '"private_key": "'"$PEM"'\nMIIEvQIBADANBg...\n",'
run_case "PEM word fragments are not a real header" 0 k.txt \
    'this is PRIVATE data
algo EC here
-----BEGIN CERTIFICATE-----'
run_case "the one placeholder account ID fixture scans clean" 0 tools/purchase_test.go \
    '		Acc''ount:       "123456789012",'
run_case "placeholder fixture line with a trailing key is still caught" 1 tools/purchase_test.go \
    '		Acc''ount:       "123456789012", k="'"$KEY"'"'
run_case "Archera env name and test key scan clean" 0 u.go \
    'EnvArcheraAPIKey = "ARCHERA_API_KEY"; testArcheraKey = "test-archera-key-0000"'
run_staged_case ".gitallowed self-scan" 0 .gitallowed

# Structural guard (#24): every entry must be anchored at both ends and pin
# the path:line: prefix of git-secrets' output, so no entry can exempt a line
# that merely contains some literal. Checked on the real .gitallowed, and
# proven to reject each bare entry this file used to carry.
allowlist_unanchored() {
    grep -vE '^[[:space:]]*(#|$)' "$1" | grep -vE '^\^.*:\[0-9\]\+:.*\$$' || true
}
unanchored=$(allowlist_unanchored "$REPO_ROOT/.gitallowed")
if [ -n "$unanchored" ]; then
    fail=$((fail + 1))
    echo "FAIL: .gitallowed has entries not anchored as ^...:[0-9]+:...\$: $unanchored" >&2
else
    pass=$((pass + 1))
fi
for bare in 123456789012 210987654321 000000000000 111111111111 222222222222 \
    333333333333 444444444444 555555555555 666666666666 777777777777 \
    888888888888 999999999999 111222333444 555666777888 999888777666 \
    999988887777 111122223333 11111111-1111-1111-1111-111111111111; do
    { cat "$REPO_ROOT/.gitallowed"; printf '%s\n' "$bare"; } >"$tmp/mutated"
    if [ -n "$(allowlist_unanchored "$tmp/mutated")" ]; then
        pass=$((pass + 1))
    else
        fail=$((fail + 1))
        echo "FAIL: structural guard accepts bare entry $bare" >&2
    fi
done

# Working-tree (file-mode) scan, the path `pre-commit` takes, where the
# account-ID detector does fire. The one allowed fixture must scan clean only
# because of its anchored entry: removing the entry, or widening its path,
# must make it fail.
file_mode_scan() {
    local relpath=$1 content=$2 actual=0
    mkdir -p "$(dirname "$relpath")"
    printf '%s\n' "$content" >"$relpath"
    git secrets --scan "$relpath" >/dev/null 2>&1 || actual=$?
    rm -f "$relpath"
    return "$actual"
}
FIXTURE='		Acc''ount:       "123456789012",'
actual=0; file_mode_scan tools/purchase_test.go "$FIXTURE" || actual=$?
check_result "anchored account-ID fixture (file mode)" 0 "$actual"
actual=0; file_mode_scan tools/other_test.go "$FIXTURE" || actual=$?
check_result "same fixture in another path (file mode)" 1 "$actual"
cp .gitallowed "$tmp/gitallowed.real"
grep -v 'purchase_test' "$tmp/gitallowed.real" >.gitallowed
actual=0; file_mode_scan tools/purchase_test.go "$FIXTURE" || actual=$?
check_result "fixture without its entry (file mode)" 1 "$actual"
sed 's#^\^tools/purchase_test\\\.go:#^[^:]+:#' "$tmp/gitallowed.real" >.gitallowed
actual=0; file_mode_scan tools/other_test.go "$FIXTURE" || actual=$?
check_result "sensitivity: a widened entry path would allow another file (file mode)" 0 "$actual"
cp "$tmp/gitallowed.real" .gitallowed

echo "$pass PASS, $fail FAIL"
[ "$fail" -eq 0 ]
