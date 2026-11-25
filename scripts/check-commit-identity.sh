#!/bin/sh
umask 022
set -eu

# Commit identity gate: the maintainer's commits use a GitHub noreply address.
#
# Every commit whose author or committer name is the maintainer's must use
# rahulrajaram@users.noreply.github.com or the ID+rahulrajaram form, so a
# private address never enters the public history. Other contributors'
# identities are not checked. Needs the full history: CI checks out with
# fetch-depth 0, and a shallow clone fails rather than passing on a partial
# view.

cd "$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"

fail() {
    echo "check-commit-identity: $*" >&2
    exit 1
}

[ "$(git rev-parse --is-shallow-repository)" = false ] ||
    fail "shallow clone; fetch the full history (fetch-depth: 0)"

bad=$(git log --all --format='%H%x09%an%x09%ae%x09%cn%x09%ce' | awk -F '\t' '
    function maintainer(name) { n = tolower(name); return n == "rahul rajaram" || n == "rahulrajaram" }
    function noreply(email) { return email ~ /^([0-9]+\+)?rahulrajaram@users\.noreply\.github\.com$/ }
    (maintainer($2) && !noreply($3)) || (maintainer($4) && !noreply($5)) { print substr($1, 1, 12) }
')

if [ -n "$bad" ]; then
    echo "check-commit-identity: maintainer commits without the GitHub noreply address:" >&2
    printf '  %s\n' $bad >&2
    exit 1
fi
echo "check-commit-identity: ok"
