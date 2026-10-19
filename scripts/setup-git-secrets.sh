#!/bin/bash
# Setup git-secrets to prevent committing sensitive information

set -e

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

echo "========================================"
echo "Git Secrets Setup"
echo "========================================"
echo ""

# Check if git-secrets is installed
if ! command -v git-secrets &> /dev/null; then
    echo -e "${RED}✗ git-secrets not installed${NC}"
    echo ""
    echo "Installation instructions:"
    echo "  macOS: brew install git-secrets"
    echo "  Linux: git clone https://github.com/awslabs/git-secrets.git && cd git-secrets && make install"
    echo ""
    exit 1
fi

echo -e "${GREEN}✓ git-secrets is installed${NC}"
echo ""

# Install git hooks
#
# git-secrets 1.3.0's install_hook() writes and chmods the hook file, then
# reports success via a `say` call. `say` was never its own function:
# git-secrets sources git's git-sh-setup and relied on `say` being defined
# there (introduced in git-sh-setup.sh in 2009). Git removed it in commit
# 5b893f7d81 ("git-sh-setup.sh: remove 'say' function, change last users"),
# first shipped in Git 2.38 (2022), because it was undocumented and unused
# within git's own tree, breaking git-secrets as an unintended side effect
# of a git upgrade rather than a git-secrets regression. On macOS the bare
# `say` call resolves to /usr/bin/say (the text-to-speech binary) instead
# and exits 0 by accident; on Linux (or on a new-enough git anywhere) there
# is no such fallback, so it's "command not found" and `git secrets
# --install -f` returns non-zero even though every hook file was already
# written correctly. Define `say` as a no-op here and export it so the
# exported function is visible in the git-secrets child process on both
# platforms, making the real hook-writing exit status the one that reaches
# the check below.
say() { :; }
export -f say

echo "Installing git-secrets hooks..."
if git secrets --install -f; then
    echo -e "${GREEN}✓ Git hooks installed${NC}"
else
    echo -e "${RED}✗ Failed to install git hooks${NC}"
    exit 1
fi

# The no-op say() above means "git secrets --install -f" returning success no
# longer proves anything: with say() removed the real signal was its exit
# status, but say() itself is a no-op that returns success regardless of
# whether install_hook actually wrote the file (e.g. a permissions error on
# chmod). Verify what the command was supposed to do instead: each hook file
# it writes exists, is executable, and contains the git-secrets invocation.
git_dir="$(git rev-parse --git-dir)"
hooks_ok=1
for hook_spec in "commit-msg:commit_msg_hook" "pre-commit:pre_commit_hook" "prepare-commit-msg:prepare_commit_msg_hook"; do
    hook_name="${hook_spec%%:*}"
    hook_cmd="${hook_spec##*:}"
    hook_path="${git_dir}/hooks/${hook_name}"
    if [ -d "${git_dir}/hooks/${hook_name}.d" ]; then
        hook_path="${git_dir}/hooks/${hook_name}.d/git-secrets"
    fi
    if [ ! -x "${hook_path}" ] || ! grep -qF "git secrets --${hook_cmd} -- \"\$@\"" "${hook_path}"; then
        echo -e "${RED}✗ ${hook_name} hook missing, not executable, or doesn't invoke git-secrets: ${hook_path}${NC}"
        hooks_ok=0
    fi
done
if [ "${hooks_ok}" -ne 1 ]; then
    echo -e "${RED}✗ Hook verification failed; git-secrets would not actually run on commit${NC}"
    exit 1
fi
echo -e "${GREEN}✓ Verified all three hooks are installed and executable${NC}"

# Register AWS secret patterns
echo ""
echo "Registering AWS secret patterns..."
git secrets --register-aws

# Add custom patterns
echo ""
echo "Adding custom secret patterns..."

# `git secrets --add` exits non-zero when the pattern is already registered,
# which under `set -e` aborted a re-run. Skip patterns already present
# (exact, fixed-string match) so the script is idempotent.
add_pattern() {
    if git config --get-all secrets.patterns | grep -qxF -- "$1"; then
        return 0
    fi
    git secrets --add -- "$1"
}

# AWS patterns
add_pattern 'AKIA[0-9A-Z]{16}'                                    # AWS Access Key ID
# macOS git grep has no \s, so the two --register-aws patterns that use it miss spaced assignments
add_pattern '("|'\'')?(AWS|aws|Aws)?_?(SECRET|secret|Secret)?_?(ACCESS|access|Access)?_?(KEY|key|Key)("|'\'')?[[:space:]]*(:|=>|=)[[:space:]]*("|'\'')?[A-Za-z0-9/\+=]{40}("|'\'')?'
add_pattern '("|'\'')?(AWS|aws|Aws)?_?(ACCOUNT|account|Account)_?(ID|id|Id)?("|'\'')?[[:space:]]*(:|=>|=)[[:space:]]*("|'\'')?[0-9]{4}\-?[0-9]{4}\-?[0-9]{4}("|'\'')?'
add_pattern 'aws(.{0,20})?['\''"][0-9a-zA-Z/+]{40}['\''"]'       # AWS Credentials

# GCP patterns
add_pattern 'AIza[0-9A-Za-z_-]{35}'                              # GCP API Key

# Azure patterns
add_pattern 'DefaultEndpointsProtocol=https'                      # Azure Connection String

# Generic secrets (require quoted values to avoid matching variable declarations)
add_pattern 'password[[:space:]]*[=:][[:space:]]*['\''"][^'\''"]{8,}'             # Password with quoted value
add_pattern 'api[_-]?key[[:space:]]*[=:][[:space:]]*['\''"][^'\''"]{8,}'         # API key with quoted value
add_pattern 'secret[_-]?key[[:space:]]*[=:][[:space:]]*['\''"][^'\''"]{8,}'      # Secret key with quoted value
add_pattern 'BEGIN[[:space:]]((RSA|DSA|EC|OPENSSH|ENCRYPTED)[[:space:]])?PRIVATE[[:space:]]KEY-----'  # PEM private keys

# Database connection strings
# The match must end on a host character: git-secrets scans with -w, which drops a match followed by a word character
add_pattern '(postgres(ql)?|mysql|mongodb(\+srv)?)://[^:/@[:space:]]+:[^@/[:space:]$<{][^@/[:space:]]*@[^[:space:]'\''"/?#]+'  # Database DSN with an inline password

# Allowed patterns live in .gitallowed (versioned, applied by every scan, including CI).
# They are matched against the whole "path:line:content" scanner output line, so an entry
# must describe the benign literal or anchor on the path; a bare keyword whitelists
# every line that contains it (#1972).

echo -e "${GREEN}✓ Secret patterns registered${NC}"

# Scan existing repository
echo ""
echo "========================================"
echo "Scanning Existing Repository"
echo "========================================"
echo ""

if git secrets --scan -r; then
    echo ""
    echo -e "${GREEN}✓ No secrets found in repository${NC}"
else
    echo ""
    echo -e "${RED}✗ Secrets detected in repository!${NC}"
    echo "Please remove them before committing."
    exit 1
fi

echo ""
echo "========================================"
echo "Setup Complete"
echo "========================================"
echo ""
echo "Git-secrets is now active and will prevent commits containing secrets."
echo ""
echo "Useful commands:"
echo "  git secrets --scan        - Scan staged files"
echo "  git secrets --scan-history - Scan entire history"
echo "  git secrets --list        - List registered patterns"
echo ""
