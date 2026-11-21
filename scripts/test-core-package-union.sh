#!/bin/sh
umask 022
set -eu

# Hermetic regression harness for the mandatory core package union in
# scripts/lib/core-package-union.sh and its wiring into
# scripts/install-guest-packages.sh.
#
# The defect this harness pins: the packages layer used to install exactly
# the chosen profile's packages and failed closed when the profile did not
# list zsh/systemd-timesyncd, so a profile that legitimately declares only
# ADDON packages could not exist. The contract is that a profile may
# declare only addons while the core provides its own prerequisites: the
# effective sealed set is the deterministic union of ca-certificates, zsh,
# and systemd-timesyncd with the profile's packages, deduplicated.
#
# Everything runs against the pure helper and static wiring checks; no
# guest, Incus, sudo, or network is involved.
#
# Usage: scripts/test-core-package-union.sh   (repository root; no arguments)

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)

fail() {
    echo "core-package-union: $*" >&2
    exit 1
}

# shellcheck source=scripts/lib/core-package-union.sh
. "$SCRIPT_DIR/lib/core-package-union.sh"

# 1. An addon-only profile (no zsh, no systemd-timesyncd) still seals the
#    mandatory core set beside its addons.
union=$(union_core_packages 'ripgrep,fzf')
[ "$union" = 'ca-certificates,zsh,systemd-timesyncd,ripgrep,fzf' ] ||
    fail "addon-only union = '$union', want the core set plus the addons"

# 2. Deduplication: a profile re-listing a core package (or repeating a
#    package in a way the profile reader admits across roots — the union
#    is idempotent) never duplicates an apt-get argument.
union=$(union_core_packages 'zsh,ripgrep')
[ "$union" = 'ca-certificates,zsh,systemd-timesyncd,ripgrep' ] ||
    fail "core-duplicate union = '$union', want the package exactly once"

# 3. Deterministic order: core entries first, then the profile's items in
#    first-appearance order.
union=$(union_core_packages 'b,c,a')
[ "$union" = 'ca-certificates,zsh,systemd-timesyncd,b,c,a' ] ||
    fail "ordering union = '$union', want core first, profile order preserved"

# 4. Malformed lists fail closed and print nothing on stdout.
for bad in '' ',ripgrep' 'ripgrep,' 'rip,,grep'; do
    if union=$(union_core_packages "$bad" 2>/dev/null); then
        fail "malformed package list '$bad' was accepted"
    fi
    [ -z "$union" ] || fail "malformed package list '$bad' printed output"
done

# 5. Invalid package names fail closed (the union re-validates every item
#    with the profile reader's grammar; nothing reaches apt-get unvalidated).
for bad in 'RipGrep' 'rip grep' 'ripgrep;rm' '-zsh' '/bin/sh' 'pkg$(`id`)' 'a"b'; do
    if union_core_packages "$bad" >/dev/null 2>&1; then
        fail "invalid package list '$bad' was accepted"
    fi
done

# 6. No code execution from a profile: the helper must contain no eval,
#    no backtick command substitution, and no sourcing of profile data.
if grep -qE 'eval|`' "$SCRIPT_DIR/lib/core-package-union.sh"; then
    fail "the union helper contains eval or command substitution"
fi

# 7. Wiring gate: the packages layer must compose the effective set
#    through the helper, pass it (not the raw list) to the guest, and
#    seal a receipt bound to the effective list, the raw chosen profile
#    packages, and the chosen manifest digest.
grep -q 'EFFECTIVE_PACKAGES=$(union_core_packages "$PROFILE_PACKAGES")' \
    "$SCRIPT_DIR/install-guest-packages.sh" ||
    fail "the packages layer does not compose the effective set through the union helper"
grep -q 'EFFECTIVE_PACKAGES=$(union_core_packages "$PROFILE_PACKAGES") ||' \
    "$SCRIPT_DIR/install-guest-packages.sh" ||
    fail "the union composition does not fail closed"
grep -qF '"manifest_sha256=$manifest_sha256"' \
    "$SCRIPT_DIR/install-guest-packages.sh" ||
    fail "the receipt does not record the chosen profile manifest digest"
grep -qF '"profile_packages=$profile_packages"' \
    "$SCRIPT_DIR/install-guest-packages.sh" ||
    fail "the receipt does not record the raw chosen profile packages"
if grep -qF 'the profile must install zsh' "$SCRIPT_DIR/install-guest-packages.sh"; then
    fail "the packages layer still obliges the profile to install zsh"
fi
if grep -qE 'for pkg in \$packages' "$SCRIPT_DIR/install-guest-packages.sh"; then
    fail "the guest installer still installs the raw profile list instead of the effective union"
fi

# 8. Minimal-profile fixtures keep working: the shipped profiles union to
#    themselves plus the core set.
for profile_file in "$SCRIPT_DIR"/../guest-config/profiles/*.profile; do
    profile_packages=$(sed -n 's/^packages=//p' "$profile_file")
    [ -n "$profile_packages" ] || fail "$profile_file has no packages line"
    union=$(union_core_packages "$profile_packages") ||
        fail "the shipped profile $profile_file does not compose"
    case "$union" in
        ca-certificates,zsh,systemd-timesyncd*) ;;
        *) fail "the union for $profile_file does not start with the core set: $union" ;;
    esac
done

echo "core-package-union: all checks passed"
