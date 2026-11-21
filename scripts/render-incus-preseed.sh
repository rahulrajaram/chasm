#!/bin/sh
set -eu

# Render the Incus preseed for this site to stdout. The template in
# host-config/incus-preseed.template.yaml carries @TOKEN@ placeholders for
# every site-derived value; rendering substitutes them from the site
# descriptor (see scripts/lib/require-site-host.sh) and refuses to emit any
# placeholder that has no value.
#
# The derived values mirror internal/siteconfig: the disk image source is
# /var/lib/incus/disks/<pool>.img, the profile eth0 address is
# sandbox_ipv4_prefix + 10, and the pool size is pool_size_gib with a GiB
# suffix. The root disk defaults to 60 GiB for a fresh profile.

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=scripts/lib/require-site-host.sh
. "$SCRIPT_DIR/lib/require-site-host.sh"

fail() {
    echo "preseed render: $*" >&2
    exit 1
}

[ "$#" -eq 0 ] || fail "this command accepts no arguments"
site_require_descriptor || exit 1

template=$SCRIPT_DIR/../host-config/incus-preseed.template.yaml
[ -f "$template" ] && [ ! -L "$template" ] ||
    fail "preseed template is missing or unsafe: $template"

escape_sed() {
    printf '%s\n' "$1" | sed 's/[&|\\]/\\&/g'
}

bridge_name=$(escape_sed "$SITE_BRIDGE_NAME")
bridge_ipv4=$(escape_sed "$SITE_BRIDGE_IPV4")
storage_pool=$(escape_sed "$SITE_STORAGE_POOL")
pool_size=$(escape_sed "${SITE_POOL_SIZE_GIB}GiB")
pool_source=$(escape_sed "/var/lib/incus/disks/$SITE_STORAGE_POOL.img")
profile_name=$(escape_sed "$SITE_PROFILE_NAME")
profile_eth0_ipv4=$(escape_sed "${SITE_SANDBOX_IPV4_PREFIX}10")
profile_root_pool=$storage_pool
profile_root_disk_gib=${SITE_PROFILE_ROOT_DISK_GIB:-60}
[ "$profile_root_disk_gib" -le "$SITE_POOL_SIZE_GIB" ] ||
    fail "profile root disk needs ${profile_root_disk_gib}GiB but pool_size_gib is $SITE_POOL_SIZE_GIB; set profile_root_disk_gib to fit or increase the pool size"

rendered=$(
    sed -e "s|@BRIDGE_NAME@|$bridge_name|g" \
        -e "s|@BRIDGE_IPV4@|$bridge_ipv4|g" \
        -e "s|@STORAGE_POOL@|$storage_pool|g" \
        -e "s|@POOL_SIZE@|$pool_size|g" \
        -e "s|@POOL_SOURCE@|$pool_source|g" \
        -e "s|@PROFILE_NAME@|$profile_name|g" \
        -e "s|@PROFILE_ETH0_IPV4@|$profile_eth0_ipv4|g" \
        -e "s|@PROFILE_ROOT_POOL@|$profile_root_pool|g" \
        -e "s|@PROFILE_ROOT_DISK_SIZE@|${profile_root_disk_gib}GiB|g" \
        "$template"
)
case "$rendered" in
    *'@'[A-Za-z_][A-Za-z0-9_]*'@'*)
        fail "rendered preseed retains an unreplaced @TOKEN@" ;;
esac
printf '%s\n' "$rendered"
