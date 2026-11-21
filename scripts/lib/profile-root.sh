#!/bin/sh

# Pure profile-root selection for chasm profile readers.
#
# The resolved bootstrap profile can come from three places, in strict
# precedence order:
#
#   1. an explicit CHASM_PROFILE_ROOT (developer/test override; the caller
#      passes the value here and the caller owns validating that the
#      named directory actually contains the wanted profiles);
#   2. the installed production tree /etc/chasm/profiles, whenever it
#      exists — an installed chasm always carries the core provisioned
#      profiles there, and separately vended site profiles land beside
#      them, so production must win over whatever happens to sit in a
#      checkout copy of the tree;
#   3. the checkout's guest-config/profiles, only as a safe fallback when
#      no production tree is installed (developer builds from a source
#      checkout on a host without an installed chasm).
#
# The function is pure: every input is injected (no /etc or checkout
# probing inside), it prints exactly one path, and it fails closed with a
# message on stderr and a nonzero status when no root can be selected.
# This shape keeps the selection hermetically testable without touching a
# live /etc.

# select_profile_root <explicit_root> <provision_root> <production_root>
#   explicit_root    value of CHASM_PROFILE_ROOT, or '' when unset
#   provision_root   repository/checkout root (contains guest-config/profiles)
#   production_root  installed profile tree (normally /etc/chasm/profiles)
select_profile_root() {
    select_root_explicit=$1
    select_root_provision=$2
    select_root_production=$3

    if [ -n "$select_root_explicit" ]; then
        printf '%s\n' "$select_root_explicit"
        return 0
    fi
    if [ -d "$select_root_production" ]; then
        printf '%s\n' "$select_root_production"
        return 0
    fi
    if [ -d "$select_root_provision/guest-config/profiles" ]; then
        printf '%s\n' "$select_root_provision/guest-config/profiles"
        return 0
    fi
    echo "profile root: no profile root found (checked $select_root_production and $select_root_provision/guest-config/profiles)" >&2
    return 1
}
