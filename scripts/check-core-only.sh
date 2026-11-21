#!/bin/sh
umask 022
set -eu

# Zero-plugin core gate: prove that core builds, vets, and tests with the
# ENTIRE plugins/ directory absent (docs/plugins.md: core never requires
# any optional component). The check copies the tracked tree to a scratch
# directory WITHOUT plugins/, then runs the core Go gates and a provision
# wiring assertion there. Read-only on the repository; unprivileged.

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd)

fail() {
    echo "check-core-only: $*" >&2
    exit 1
}

scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT HUP INT TERM

# Copy the tracked tree except plugins/: git archive respects the index,
# so untracked scratch never leaks in and the copy is exactly HEAD's
# tracked content (the working tree's tracked files are what we test; a
# dirty tree is allowed — this gate runs on the candidate, not on HEAD).
( cd "$REPO_ROOT" && git ls-files ) >"$scratch/files" ||
    fail "could not list tracked files"
while IFS= read -r tracked_file; do
    case "$tracked_file" in
        plugins/*) continue ;;
    esac
    mkdir -p "$scratch/tree/$(dirname -- "$tracked_file")"
    cp "$REPO_ROOT/$tracked_file" "$scratch/tree/$tracked_file" ||
        fail "could not copy tracked file: $tracked_file"
done <"$scratch/files"

[ ! -e "$scratch/tree/plugins" ] ||
    fail "the copy still contains plugins/"

# The core Go gates in the plugin-free tree: build every package (the
# installer's payload builds ./cmd/...), vet, and the full test suite.
( cd "$scratch/tree" && go build ./... ) ||
    fail "core go build failed without plugins/"
( cd "$scratch/tree" && mkdir -p "$scratch/bin" && go build -o "$scratch/bin/" ./cmd/... ) ||
    fail "core cmd build failed without plugins/"
[ -f "$scratch/bin/chasm" ] && [ -f "$scratch/bin/chasmctl" ] &&
    [ -f "$scratch/bin/executor_server" ] && [ -f "$scratch/bin/supervisor_server" ] ||
    fail "the zero-plugin build did not produce the four control-plane binaries"
[ ! -e "$scratch/bin/chasm-clipboard" ] ||
    fail "core still builds the clipboard helper without the plugin"
( cd "$scratch/tree" && go vet ./... ) ||
    fail "core go vet failed without plugins/"
( cd "$scratch/tree" && go test ./... ) ||
    fail "core go test failed without plugins/"

# The provision payload must not require plugin trees: no plugin path in
# the embedded executable list or tree names.
if grep -v '^#' "$scratch/tree/scripts/lib/provision-tree.sh" | grep -q "plugins/"; then
    fail "the provision payload references plugins/, which core must not require"
fi

# The installer must not source the plugin libraries unconditionally: the
# only plugin loads are the flag-guarded dispatchers.
if grep -n 'lib/ssh-hardening\.sh\|lib/incus-install\.sh' "$scratch/tree/scripts/install-chasm.sh"; then
    fail "the installer sources a plugin library from the old core lib path"
fi

echo "check-core-only: clean (core builds, vets, and tests with plugins/ absent; payload references no plugin tree)"
