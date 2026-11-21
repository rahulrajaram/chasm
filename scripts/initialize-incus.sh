#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=scripts/lib/require-site-host.sh
. "$SCRIPT_DIR/lib/require-site-host.sh"

fail() {
    echo "initialize: $*" >&2
    exit 1
}

[ "$(id -u)" -eq 0 ] || fail "run through sudo as root"
require_site_host || fail "site identity check failed"

# Nested virtualization must stay off whether the host loads kvm_intel or
# kvm_amd. The admission rule and the module list live in require-site-host.sh
# (site_nested_kvm_disabled / site_nested_kvm_modules) so the advisory
# preflight in install-chasm.sh and this enforcing check cannot diverge.

kvm_found=0
for kvm_candidate in $(site_nested_kvm_modules); do
    kvm_parameter=/sys/module/$kvm_candidate/parameters/nested
    [ -r "$kvm_parameter" ] || continue
    kvm_found=1
    kvm_nested=$(cat "$kvm_parameter")
    site_nested_kvm_disabled "$kvm_nested" ||
        fail "nested KVM is still enabled ($kvm_candidate nested=$kvm_nested)"
done
[ "$kvm_found" -eq 1 ] || fail "neither kvm_intel nor kvm_amd is loaded"
[ -c /dev/kvm ] || fail "/dev/kvm is missing"

preseed_tmp=
cleanup() {
    if [ -n "$preseed_tmp" ]; then
        rm -f -- "$preseed_tmp"
    fi
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

incus admin waitready --timeout=60
if ! incus storage show "$SITE_STORAGE_POOL" >/dev/null 2>&1; then
    preseed_tmp=$(mktemp)
    "$SCRIPT_DIR/render-incus-preseed.sh" >"$preseed_tmp"
    incus admin init --preseed <"$preseed_tmp"
    rm -f -- "$preseed_tmp"
    preseed_tmp=
fi

# The firewall must never be enabled for an interface Incus did not create:
# a descriptor whose bridge_name names a foreign interface (or is missing
# here for any other reason) would otherwise render and load a ruleset that
# drops or rescues the wrong interface. Check BEFORE arming the unit so the
# failure is named and no wrong rules ever load.
incus network show "$SITE_BRIDGE_NAME" >/dev/null 2>&1 ||
    fail "the site bridge $SITE_BRIDGE_NAME does not exist as an Incus network; refusing to arm the firewall for an interface Incus never created (check bridge_name in the descriptor)"

systemctl enable --now chasm-firewall.service
incus storage show "$SITE_STORAGE_POOL"
incus network show "$SITE_BRIDGE_NAME"
incus profile show "$SITE_PROFILE_NAME"
nft list table inet chasm
