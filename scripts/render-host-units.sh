#!/bin/sh
set -eu

# Render every host-control unit template into TARGET_DIR for this site.
#
# The templates carry three tokens: @SITE_MARKER_PATH@ (the product-constant
# site identity marker path), @OPERATOR_USER@ (supervisor socket group), and
# @SUPERVISOR_USER@ (executor socket group and supervisor service identity).
# Rendering fails closed if any token survives unreplaced or if the required
# unit set is incomplete.

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=scripts/lib/require-site-host.sh
. "$SCRIPT_DIR/lib/require-site-host.sh"

fail() {
    echo "unit render: $*" >&2
    exit 1
}

[ "$#" -eq 1 ] || { echo "usage: $0 TARGET_DIR" >&2; exit 2; }
target=$1
[ -d "$target" ] || fail "target directory does not exist: $target"

site_require_descriptor || exit 1

marker_path=$(printf '%s\n' "$SITE_MARKER_PATH" | sed 's/[&|\\]/\\&/g')
operator_user=$(printf '%s\n' "$SITE_OPERATOR_USER" | sed 's/[&|\\]/\\&/g')
supervisor_user=$(printf '%s\n' "$SITE_SUPERVISOR_USER" | sed 's/[&|\\]/\\&/g')

units_dir=$(CDPATH='' cd -- "$SCRIPT_DIR/../host-control" && pwd)

for template_file in "$units_dir"/*.template; do
    [ -f "$template_file" ] || continue
    name=$(basename "$template_file" .template)
    case "$name" in
        *[!A-Za-z0-9._-]*|'') fail "unsafe unit name derived from template" ;;
    esac
    rendered=$(
        sed -e "s|@SITE_MARKER_PATH@|$marker_path|g" \
            -e "s|@OPERATOR_USER@|$operator_user|g" \
            -e "s|@SUPERVISOR_USER@|$supervisor_user|g" \
            "$template_file"
    )
    case "$rendered" in
        *'@'[A-Za-z_][A-Za-z0-9_]*'@'*)
            fail "rendered unit $name retains an unreplaced @TOKEN@" ;;
    esac
    printf '%s\n' "$rendered" >"$target/$name"
done

for unit in \
    chasm-executor.service \
    chasm-executor.socket \
    chasm-firewall.service \
    chasm-supervisor.service \
    chasm-supervisor.socket
do
    [ -f "$target/$unit" ] || fail "rendered unit is missing: $unit"
done
