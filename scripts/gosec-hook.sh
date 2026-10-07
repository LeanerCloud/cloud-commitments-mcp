#!/usr/bin/env bash
# scripts/gosec-hook.sh
#
# Pre-commit gosec hook: runs gosec on only the Go packages that contain staged
# files. Fast by design: never scans the whole repo; each commit triggers at
# most one gosec invocation.
#
# The pinned version and the rule set live in scripts/run-gosec.sh, which this
# delegates to. ci.yml's security-scan job and the Makefile's
# security-scan-go target call that same script, so all three gates enforce
# identical rules.
#
# Called by pre-commit with pass_filenames: true and files: \.go$.
# Exits 0 when no staged .go files survive filtering (deleted / testdata).
# Exits 1 on any gosec finding; exits 2 on setup failure.

set -euo pipefail

[[ $# -eq 0 ]] && exit 0

REPO_ROOT="$(git rev-parse --show-toplevel)"

# Filter: skip deleted files and files under testdata/.
pkgs=()
for f in "$@"; do
    [[ -f "$f" ]] || continue
    case "$f" in
        */testdata/*) continue ;;
        testdata/*)   continue ;;
    esac
    dir=$(dirname "$f")
    [[ "$dir" == "." ]] && pkgs+=(".") || pkgs+=("./$dir")
done

[[ ${#pkgs[@]} -eq 0 ]] && exit 0

# Deduplicate: many changed files usually share a package. $(...) rather than
# mapfile: this runs under macOS's bash 3.2, which has no mapfile.
pkg_arg=$(printf '%s\n' "${pkgs[@]}" | sort -u | tr '\n' ' ')

# $pkg_arg intentionally unquoted: run-gosec.sh takes one package per
# argument, and this is the portable way to forward the list (bash 3.2).
# shellcheck disable=SC2086
exec bash "$REPO_ROOT/scripts/run-gosec.sh" scan $pkg_arg
