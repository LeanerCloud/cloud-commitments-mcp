#!/usr/bin/env bash
# scripts/run-gosec.sh
#
# Single source of truth for the gosec scan in this repository: the pinned
# gosec version and the rule set. Invoked by all three call sites so a local
# run and the CI job enforce identical rules:
#
#   scripts/gosec-hook.sh           pre-commit hook (changed packages only)
#   .github/workflows/ci.yml        security-scan job
#   Makefile                        security-scan-go target
#
# Rules: no `-exclude` anywhere. gosec's default rule set passes clean on this
# tree (verified 2026-10-07 at 86a698e: 0 issues over 17 files, 3744 lines),
# so all three gates run the full set. The pre-commit hook previously excluded
# 15 rules and the Makefile 8 while CI excluded none, so a clean local run
# predicted nothing about CI; dropping the lists removes that divergence
# rather than moving it, and it is the stricter of the two configurations.
# If a rule has to be muted, annotate the finding line with `#nosec <rule>`
# and a reason instead of widening the exclude list here.
#
# Usage:
#   run-gosec.sh scan [PKG ...]   scan PKGs (default ./...), findings on stdout
#   run-gosec.sh report FMT FILE  scan ./... and write FMT (sarif|json) to FILE
#   run-gosec.sh install          pre-warm the pinned binary, no scan
#   run-gosec.sh version          print the pinned version, e.g. v2.28.0
#
# Exits 0 when gosec reports no findings, 1 on findings, 2 on setup failure.

set -euo pipefail

GOSEC_VERSION="2.28.0"
GOSEC_MODULE="github.com/securego/gosec/v2/cmd/gosec"

# Version-stamped cache dir, not GOPATH/bin, so the pin above stays
# authoritative even when a different gosec sits on PATH.
GOSEC_BIN="${HOME}/.cache/pre-commit-gosec/v${GOSEC_VERSION}/gosec"

usage() {
    sed -n 's/^#   run-gosec.sh /usage: run-gosec.sh /p' "$0" >&2
    exit 2
}

# Pre-warm the cached binary without scanning, for `make install-dev-tools`.
cmd_install() {
    ensure_gosec
    echo "run-gosec: gosec v${GOSEC_VERSION} ready at $GOSEC_BIN" >&2
}

ensure_gosec() {
    local need_install=0
    if [[ -x "$GOSEC_BIN" ]]; then
        # gosec built via `go install` embeds "dev" in `gosec -version`
        # regardless of tag; read the real module version from the binary's
        # build info instead.
        local installed_ver
        installed_ver=$(go version -m "$GOSEC_BIN" 2>/dev/null \
            | awk '$1=="mod" && $2~/gosec/{print $3}')
        if [[ "$installed_ver" != "v${GOSEC_VERSION}" ]]; then
            echo "run-gosec: cached binary is ${installed_ver:-unknown}, need v${GOSEC_VERSION}; reinstalling" >&2
            need_install=1
        fi
    else
        need_install=1
    fi

    if [[ $need_install -eq 1 ]]; then
        echo "run-gosec: installing ${GOSEC_MODULE}@v${GOSEC_VERSION} -> $(dirname "$GOSEC_BIN")" >&2
        mkdir -p "$(dirname "$GOSEC_BIN")"
        GOBIN="$(dirname "$GOSEC_BIN")" go install "${GOSEC_MODULE}@v${GOSEC_VERSION}" || {
            echo "run-gosec: install failed (is Go on PATH?)" >&2
            exit 2
        }
    fi
}

cmd_scan() {
    local pkgs=("$@")
    [[ ${#pkgs[@]} -eq 0 ]] && pkgs=("./...")
    ensure_gosec
    echo "run-gosec: scanning ${pkgs[*]}" >&2
    # gosec takes one package per argument; joining them into one word and
    # letting the shell split it back out is the portable way to pass a list
    # (bash 3.2 has no nameref/mapfile). Package paths never contain spaces.
    local pkg_arg
    pkg_arg=$(IFS=' '; echo "${pkgs[*]}")
    # shellcheck disable=SC2086
    "$GOSEC_BIN" -quiet -exclude-dir=vendor $pkg_arg
}

cmd_report() {
    local fmt="${1:-}" out="${2:-}"
    [[ -n "$fmt" && -n "$out" ]] || usage
    ensure_gosec
    echo "run-gosec: writing $fmt report to $out" >&2
    local rc=0
    "$GOSEC_BIN" -fmt "$fmt" -out "$out" -exclude-dir=vendor ./... || rc=$?
    # gosec exits non-zero both for findings and for scan errors; a missing
    # report file tells the two apart.
    [[ -f "$out" ]] || { echo "run-gosec: gosec produced no $fmt output" >&2; exit 2; }
    exit "$rc"
}

case "${1:-}" in
    scan)
        shift
        cmd_scan "$@"
        ;;
    report)
        shift
        cmd_report "$@"
        ;;
    install)
        cmd_install
        ;;
    version)
        printf 'v%s\n' "$GOSEC_VERSION"
        ;;
    *)
        usage
        ;;
esac
