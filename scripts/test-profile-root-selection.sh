#!/bin/sh
umask 022
set -eu

# Hermetic regression harness for the profile-root selection in
# scripts/lib/profile-root.sh and its identity binding through
# scripts/lib/require-site-profile.sh.
#
# The defect this harness pins: the template identity helper used to pick
# the checkout's guest-config/profiles whenever it existed, even on an
# installed chasm, so a separately vended site profile under
# /etc/chasm/profiles was silently ignored (the colocated checkout copy
# won) and the sealed template identity was bound to the wrong bytes.
#
# The precedence contract now is: an explicit CHASM_PROFILE_ROOT wins
# (developer/test override), then the installed production tree whenever
# it exists, and only a host without a production tree falls back to the
# checkout tree.
#
# Everything is argument-injected fixture directories under a temporary
# root; this harness never reads or edits a live /etc/chasm.
#
# Usage: scripts/test-profile-root-selection.sh   (repository root; no arguments)

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)

fail() {
    echo "profile-root-selection: $*" >&2
    exit 1
}

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

# shellcheck source=scripts/lib/profile-root.sh
. "$SCRIPT_DIR/lib/profile-root.sh"

# Fixture: a checkout tree with a minimal profile, and an installed
# production tree whose minimal profile has DIFFERENT bytes (the
# separately vended external override).
mkdir -p "$WORK/checkout/guest-config/profiles" "$WORK/installed"
printf 'packages=curl\n' >"$WORK/checkout/guest-config/profiles/minimal.profile"
printf 'packages=curl,wget\n' >"$WORK/installed/minimal.profile"

# 1. Explicit override wins over both trees.
selected=$(select_profile_root "$WORK/explicit" "$WORK/checkout" "$WORK/installed")
[ "$selected" = "$WORK/explicit" ] ||
    fail "explicit root: selected '$selected', want $WORK/explicit"

# 2. The installed production tree wins over the colocated checkout tree
#    (the fixed defect: the checkout used to shadow the installed profiles).
selected=$(select_profile_root '' "$WORK/checkout" "$WORK/installed")
[ "$selected" = "$WORK/installed" ] ||
    fail "production precedence: selected '$selected', want $WORK/installed"

# 3. Without a production tree, the checkout is the fallback.
selected=$(select_profile_root '' "$WORK/checkout" "$WORK/nonexistent")
[ "$selected" = "$WORK/checkout/guest-config/profiles" ] ||
    fail "checkout fallback: selected '$selected', want the checkout tree"

# 4. Fails closed when neither tree exists.
if select_profile_root '' "$WORK/empty" "$WORK/nonexistent" >/dev/null 2>&1; then
    fail "missing roots: selection must fail closed"
fi

# 5. Identity binding: the selected root's bytes are the ones that enter
#    the profile digest. The reader must pin the installed minimal
#    profile's manifest digest (the external bytes), not the colocated
#    checkout copy's, and the digest must change when the external bytes
#    change. The reader binds PROFILE_ROOT at source time, so each case
#    sources it fresh with CHASM_PROFILE_ROOT already set.
run_reader() {
    run_reader_root=$1
    # shellcheck source=scripts/lib/require-site-profile.sh
    CHASM_PROFILE_ROOT=$run_reader_root . "$SCRIPT_DIR/lib/require-site-profile.sh"
    require_site_profile minimal || fail "the minimal profile under $run_reader_root could not be read"
}
run_reader "$WORK/installed"
installed_digest=$PROFILE_MANIFEST_SHA256
installed_packages=$PROFILE_PACKAGES
run_reader "$WORK/checkout/guest-config/profiles"
checkout_digest=$PROFILE_MANIFEST_SHA256
[ "$installed_packages" = "curl,wget" ] ||
    fail "installed profile packages = '$installed_packages', want curl,wget"
[ "$installed_digest" != "$checkout_digest" ] ||
    fail "the two different profile bytes produced the same manifest digest"
[ "$installed_digest" = "$(sha256sum "$WORK/installed/minimal.profile" | cut -d' ' -f1)" ] ||
    fail "the pinned digest is not the digest of the selected external bytes"
[ "$checkout_digest" = "$(sha256sum "$WORK/checkout/guest-config/profiles/minimal.profile" | cut -d' ' -f1)" ] ||
    fail "the checkout digest is not the digest of the checkout bytes"

# 6. Wiring gate: the identity helper must select through the pure
#    selector and the old checkout-first precedence branch must not creep
#    back.
grep -q 'select_profile_root' "$SCRIPT_DIR/lib/sandbox-template-identity.sh" ||
    fail "sandbox-template-identity.sh no longer selects through profile-root.sh"
if grep -qF 'if [ -z "${CHASM_PROFILE_ROOT:-}" ] && [ -d "$provision_root/guest-config/profiles" ]' \
    "$SCRIPT_DIR/lib/sandbox-template-identity.sh"; then
    fail "the checkout-first precedence branch is still present"
fi

echo "profile-root-selection: all checks passed"
