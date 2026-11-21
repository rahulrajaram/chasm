#!/bin/sh
set -eu

# Render the per-site host configuration fragments into TARGET_DIR.
#
# These are the host-config files that carry a site value and therefore cannot
# be installed as-is:
#
#   sudoers.d/<name>.template   @OPERATOR_USER@  - the unprivileged user
#                               allowed to enter a sandbox through the root
#                               helper (rendered to $TARGET_DIR/<name>)
#   sysusers.d/<name>.template  @SUPERVISOR_USER@ - the unprivileged
#                               supervisor user and group the units run as
#                               (rendered to $TARGET_DIR/sysusers.d/<name>)
#   tmpfiles.d/<name>.template  @SUPERVISOR_USER@ - the owner of the
#                               supervisor-owned state directories
#                               (rendered to $TARGET_DIR/tmpfiles.d/<name>)
#
# Rendering fails closed if a token survives unreplaced, and validates the
# rendered sudoers fragment with visudo when it is available, so a malformed
# fragment is caught before it reaches /etc/sudoers.d.

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=scripts/lib/require-site-host.sh
. "$SCRIPT_DIR/lib/require-site-host.sh"

fail() {
    echo "host config render: $*" >&2
    exit 1
}

[ "$#" -eq 1 ] || { echo "usage: $0 TARGET_DIR" >&2; exit 2; }
target=$1
[ -d "$target" ] || fail "target directory does not exist: $target"

site_require_descriptor || exit 1

operator_user=$(printf '%s\n' "$SITE_OPERATOR_USER" | sed 's/[&|\\]/\\&/g')
supervisor_user=$(printf '%s\n' "$SITE_SUPERVISOR_USER" | sed 's/[&|\\]/\\&/g')

rendered_count=0
# Paths of the sudoers fragments rendered in this pass; only these are
# validated with visudo, because TARGET_DIR may legitimately hold other
# rendered artifacts (install-control-plane.sh renders the systemd units
# into the same directory).
rendered_sudoers=
# sudoers fragments stay flat in TARGET_DIR (the sudoers.d/chasm name is part
# of the render contract); the other fragments keep their destination
# subdirectory so install-control-plane.sh can install them by kind.
for template_dir in sudoers.d sysusers.d tmpfiles.d; do
    output_dir=$target
    [ "$template_dir" = "sudoers.d" ] || output_dir=$target/$template_dir
    mkdir -p "$output_dir"
    for template_file in "$SCRIPT_DIR/../host-config/$template_dir"/*.template; do
        [ -f "$template_file" ] || continue
        name=$(basename "$template_file" .template)
        case "$name" in
            *[!A-Za-z0-9._-]*|'') fail "unsafe fragment name derived from template" ;;
        esac
        rendered=$(
            sed -e "s|@OPERATOR_USER@|$operator_user|g" \
                -e "s|@SUPERVISOR_USER@|$supervisor_user|g" "$template_file"
        )
        case "$rendered" in
            *'@'[A-Za-z_][A-Za-z0-9_]*'@'*)
                fail "rendered fragment $name retains an unreplaced @TOKEN@" ;;
        esac
        printf '%s\n' "$rendered" >"$output_dir/$name"
        rendered_count=$((rendered_count + 1))
        if [ "$template_dir" = "sudoers.d" ]; then
            rendered_sudoers="$rendered_sudoers $output_dir/$name"
        fi
    done
done
[ "$rendered_count" -gt 0 ] || fail "no host configuration templates were found"

if command -v visudo >/dev/null 2>&1; then
    for fragment in $rendered_sudoers; do
        visudo -cf "$fragment" >/dev/null ||
            fail "rendered fragment $fragment is not a valid sudoers file"
    done
fi
