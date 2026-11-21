#!/bin/sh
umask 022
# Portability gate: the public tree must stay free of machine-local facts.
#
# Built-in check: no tracked file outside the reviewed allowlist may carry a
# machine-local absolute path under /home (guest product paths such as
# /home/agent and the /nonexistent placeholder are exempt), and every file is
# scanned through the neutralization described below.
#
# Optional check: set CHASM_PORTABILITY_PATTERNS_FILE to the path of a file
# whose non-empty lines are extended regular expressions (a leading # starts
# a comment). Each pattern is applied to the same files through the same
# allowlist and neutralization. If the variable is unset, or the file it
# names does not exist or is empty, the extra patterns are skipped. Private
# host, user, and project names belong in that file, never in this script.
#
# The allowlist is exactly three kinds of file:
#   docs/            documentation may quote the names that were removed
#   testdata/        fixtures and golden corpora, including the legacy-site fixture
#   this script
# Everything else - Go sources, shell scripts, units, host config - must be
# free of machine-local absolute paths and of any extra pattern the operator
# supplies. Guest paths (/home/agent) are product paths, not host coupling,
# and the module path carries the owner name on purpose (it is neutralized
# below by reading go.mod, not by naming the owner here).
set -eu

cd "$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"

HOST_PATH='/home/[a-z0-9_.-][a-z0-9_.-]*/'
GUEST_PATH='/home/agent/'

is_allowed() {
    case "$1" in
        docs/*|testdata/*|scripts/check-portability.sh)
            return 0
            ;;
        *)
            return 1
            ;;
    esac
}

# The module identity is read from go.mod at run time. The module path and
# the owner handle it contains (for example in the LICENSE copyright line)
# name the project owner on purpose, so both are replaced with placeholders
# before the patterns run; this script itself never names the owner.
MODULE_PATH=$(sed -n 's/^module[[:space:]][[:space:]]*//p' go.mod | head -n 1)
MODULE_OWNER=
case "$MODULE_PATH" in
    */*)
        MODULE_OWNER=${MODULE_PATH#*/}
        MODULE_OWNER=${MODULE_OWNER%%/*}
        ;;
esac

neutralize_module_path() {
    if [ -n "$MODULE_OWNER" ]; then
        sed -e "s|$MODULE_PATH|@MODULE@|g" -e "s|$MODULE_OWNER|@OWNER@|g" "$1"
    else
        sed -e "s|$MODULE_PATH|@MODULE@|g" "$1"
    fi
}

# Extra patterns are optional: absent variable or absent file means skip.
EXTRA_PATTERNS_FILE=${CHASM_PORTABILITY_PATTERNS_FILE:-}
EXTRA_PATTERN=
if [ -n "$EXTRA_PATTERNS_FILE" ] && [ -f "$EXTRA_PATTERNS_FILE" ]; then
    EXTRA_PATTERN=$(sed -e 's/[[:space:]]#.*$//' -e '/^[[:space:]]*#/d' -e '/^[[:space:]]*$/d' "$EXTRA_PATTERNS_FILE" | paste -sd '|' -)
fi

report() {
    echo "check-portability: $1" >&2
    shift
    sed 's/^/  /' "$@" >&2
}

status=0
scratch=$(mktemp)
trap 'rm -f "$scratch"' EXIT

for file in $(git ls-files); do
    if is_allowed "$file"; then
        continue
    fi
    [ -f "$file" ] || continue
    case "$file" in
        *.png|*.jpg|*.gif|*.gz|*.zip|*.ico) continue ;;
    esac

    if [ -n "$EXTRA_PATTERN" ]; then
        if neutralize_module_path "$file" | grep -nIE "$EXTRA_PATTERN" >"$scratch" 2>/dev/null; then
            report "$file matches an extra portability pattern:" "$scratch"
            status=1
        fi
    fi
    if grep -nIE "$HOST_PATH" "$file" 2>/dev/null | grep -vF "$GUEST_PATH" | grep -vF '/nonexistent' >"$scratch"; then
        report "$file carries a machine-local absolute path:" "$scratch"
        status=1
    fi
done

if [ "$status" -eq 0 ]; then
    echo "check-portability: clean (no machine-local facts outside the allowlist)"
fi
exit "$status"
