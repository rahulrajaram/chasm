#!/bin/sh
set -eu

# Render the nftables ruleset for this site to stdout. The template in
# host-config/firewall.nft.template carries the @BRIDGE_NAME@ token; the
# RFC1918/bogons drop list is a product constant.
#
# host-control/chasm-firewall performs the same substitution after
# verifying the pinned site identity; this script exists so the equivalence
# gate and unprivileged dry runs can render without touching nftables.

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=scripts/lib/require-site-host.sh
. "$SCRIPT_DIR/lib/require-site-host.sh"

fail() {
    echo "firewall render: $*" >&2
    exit 1
}

[ "$#" -eq 0 ] || fail "this command accepts no arguments"
site_require_descriptor || exit 1

template=$SCRIPT_DIR/../host-config/firewall.nft.template
[ -f "$template" ] && [ ! -L "$template" ] ||
    fail "firewall template is missing or unsafe: $template"

bridge_name=$(printf '%s\n' "$SITE_BRIDGE_NAME" | sed 's/[&|\\]/\\&/g')
rendered=$(sed -e "s|@BRIDGE_NAME@|$bridge_name|g" "$template")
case "$rendered" in
    *'@'[A-Za-z_][A-Za-z0-9_]*'@'*)
        fail "rendered firewall ruleset retains an unreplaced @TOKEN@" ;;
esac
printf '%s\n' "$rendered"
