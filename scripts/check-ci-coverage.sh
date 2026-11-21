#!/bin/sh
umask 022
set -eu

# CI coverage gate: every tracked gate and test script stays wired into CI.
#
# Fails when any of the following holds:
#   1. a tracked gate or test script — scripts/check-*.sh, scripts/test-*.sh,
#      or the plugins' own plugins/*/test-*.sh — is never invoked by
#      .github/workflows/ci.yml;
#   2. ci.yml invokes a gate or test script that git does not track (a typo
#      or a stale reference to a deleted script);
#   3. the gate and test script list in CONTRIBUTING.md ("CI runs exactly
#      these commands") differs from what ci.yml invokes.
#
# Reads tracked files only (git ls-files, plus the workflow and the guide as
# text) and never executes any of the scripts it inspects. Runs unprivileged
# from anywhere inside the repository.

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd)
cd "$REPO_ROOT"

WORKFLOW=.github/workflows/ci.yml
GUIDE=CONTRIBUTING.md

fail() {
    echo "check-ci-coverage: $*" >&2
    exit 1
}

[ -f "$WORKFLOW" ] || fail "$WORKFLOW is missing"
[ -f "$GUIDE" ] || fail "$GUIDE is missing"

scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT

# Tracked gate and test scripts, one repository-relative path per line:
# the core scripts and the plugins' own test scripts (plugins/* are
# optional add-ons, but a plugin test that exists must still run in CI).
git ls-files -- 'scripts/check-*.sh' 'scripts/test-*.sh' 'plugins/*/test-*.sh' >"$scratch/tracked"
[ -s "$scratch/tracked" ] || fail "git tracks no gate or test scripts"

# Gate and test scripts ci.yml invokes: one-line "run: <path>.sh" steps
# under scripts/ or plugins/<name>/. POSIX BRE has no alternation, so the
# shape is matched here and the check-/test- restriction is applied with
# grep -E below.
sed -n -e 's/^[[:space:]]*run:[[:space:]]*\(scripts\/[a-z0-9-]*\.sh\)[[:space:]]*$/\1/p' \
       -e 's/^[[:space:]]*run:[[:space:]]*\(plugins\/[a-z0-9-]*\/test-[a-z0-9-]*\.sh\)[[:space:]]*$/\1/p' "$WORKFLOW" \
    | grep -E '^(scripts/(check|test)-[a-z0-9-]*|plugins/[a-z0-9-]*/test-[a-z0-9-]*)\.sh$' \
    | sort >"$scratch/invoked"

status=0

# 1. Every tracked gate and test script must be invoked in the workflow.
while IFS= read -r script; do
    grep -qxF "$script" "$scratch/invoked" || {
        echo "check-ci-coverage: $script is tracked but never run in $WORKFLOW" >&2
        status=1
    }
done <"$scratch/tracked"

# 2. The workflow must not invoke a gate or test script git does not track.
while IFS= read -r script; do
    grep -qxF "$script" "$scratch/tracked" || {
        echo "check-ci-coverage: $WORKFLOW runs $script but git does not track it" >&2
        status=1
    }
done <"$scratch/invoked"

# 3. The guide's runnable list must equal the workflow's set of gate and
# test scripts, so "CI runs exactly these commands" stays true. The guide
# records core scripts as scripts/<name>.sh and plugin tests as
# plugins/<name>/test-<name>.sh (the (cd ... && go ...) clipboard line is
# not a tracked script and is covered by the go vet/test steps itself).
grep -E '^(scripts/(check|test)-[a-z0-9-]*|plugins/[a-z0-9-]*/test-[a-z0-9-]*)\.sh$' "$GUIDE" | sort >"$scratch/documented"
if ! cmp -s "$scratch/documented" "$scratch/invoked"; then
    {
        echo "check-ci-coverage: $GUIDE's gate and test script list differs from $WORKFLOW"
        echo "check-ci-coverage: (< $GUIDE, > $WORKFLOW)"
        diff "$scratch/documented" "$scratch/invoked" >&2 || true
    }
    status=1
fi

if [ "$status" -eq 0 ]; then
    echo "check-ci-coverage: clean ($(wc -l <"$scratch/tracked" | tr -d '[:space:]') tracked gate and test scripts, all run in CI and listed in the guide)"
fi
exit "$status"
