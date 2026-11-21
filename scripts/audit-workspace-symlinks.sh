#!/bin/sh
set -eu

# Audit (and optionally prune) symlinks in chasm's virtiofs workspace and
# shared volumes.
#
# Boundary semantics (verified against a live site 2026-09-19): a symlink stored in
# a share resolves in whichever kernel walks the path. Guest processes always
# resolve against the guest root, so a sandbox cannot read host files through
# a symlink. Host processes resolve against the HOST root: a symlink written
# by a sandbox redirects a host reader (cat, backup jobs, cp -L) to host
# files. This audit makes that surface visible and prunable.
#
# Usage:
#   scripts/audit-workspace-symlinks.sh            report; exit 1 if any found
#   scripts/audit-workspace-symlinks.sh --prune    delete every symlink found
#   CHASM_POOLS_ROOT=/var/lib/incus/storage-pools override for testing

[ "$#" -eq 0 ] || [ "$1" = "--prune" ] || {
    echo "usage: $0 [--prune]" >&2
    exit 2
}
prune=0
[ "${1:-}" = "--prune" ] && prune=1

POOLS_ROOT=${CHASM_POOLS_ROOT:-/var/lib/incus/storage-pools}
[ -d "$POOLS_ROOT" ] || {
    echo "audit-workspace-symlinks: no pools root at $POOLS_ROOT" >&2
    exit 2
}

found=0
link_list=$(find "$POOLS_ROOT" -type d -name custom -exec find {} -type l \; 2>/dev/null || true)
ifs_old=$IFS
IFS='
'
for link in $link_list; do
    IFS=$ifs_old
    [ -n "$link" ] || continue
    found=$((found + 1))
    volume=${link#"$POOLS_ROOT"/}
    volume=${volume%%/*}
    target=$(readlink "$link")
    printf '%s: %s -> %s\n' "$volume" "$link" "$target"
    if [ "$prune" -eq 1 ]; then
        rm -f -- "$link"
    fi
done
IFS=$ifs_old

if [ "$found" -eq 0 ]; then
    echo "audit-workspace-symlinks: clean (no symlinks in any volume)"
    exit 0
fi

if [ "$prune" -eq 1 ]; then
    echo "audit-workspace-symlinks: pruned $found symlink(s)"
    exit 0
fi

echo "audit-workspace-symlinks: $found symlink(s) present; host readers must not dereference them (run with --prune to remove)" >&2
exit 1
