#!/bin/sh

# Pure core-package union for the chasm guest packages layer.
#
# The contract: a bootstrap profile may declare only ADDON packages, while
# the core provides its own prerequisites. Every guest install therefore
# seals the deterministic union of the mandatory core packages
# (ca-certificates for TLS trust, zsh for the guest account's login shell,
# systemd-timesyncd for clock recovery) with the chosen profile's packages,
# deduplicated, core entries first, in first-appearance order thereafter.
#
# The function is pure and re-validates every package name with the same
# grammar the profile reader enforces (^[a-z0-9][a-z0-9+.-]*$, no empty
# items, no duplicates): a malformed package list fails closed with a
# message on stderr and a nonzero status instead of ever reaching apt-get.
# Nothing here executes anything from the profile; the profile contributes
# plain package-name tokens only.

CORE_PACKAGES='ca-certificates zsh systemd-timesyncd'

# union_core_packages <profile_packages>
#   profile_packages  the chosen profile's comma-separated package list
# Prints the effective comma-separated package list on stdout.
union_core_packages() {
    union_profile_packages=$1

    case "$union_profile_packages" in
        *[!a-z0-9+.,-]*)
            echo "core package union: malformed package list: $union_profile_packages" >&2
            return 1
            ;;
    esac
    case "$union_profile_packages" in
        ''|,*|*,|*,,*)
            echo "core package union: malformed package list: $union_profile_packages" >&2
            return 1
            ;;
    esac

    union_all=$CORE_PACKAGES
    union_oldifs=$IFS
    IFS=,
    set -f
    # shellcheck disable=SC2086
    for union_pkg in $union_profile_packages; do
        union_all="$union_all $union_pkg"
    done
    set +f
    IFS=$union_oldifs

    union_seen=
    union_effective=
    for union_pkg in $union_all; do
        case "$union_pkg" in
            ''|*[!a-z0-9+.-]*|[!a-z0-9]*)
                echo "core package union: invalid package name '$union_pkg'" >&2
                return 1
                ;;
        esac
        case " $union_seen " in
            *" $union_pkg "*) continue ;;
        esac
        union_seen="$union_seen $union_pkg"
        case "$union_effective" in
            '') union_effective=$union_pkg ;;
            *) union_effective="$union_effective,$union_pkg" ;;
        esac
    done
    printf '%s\n' "$union_effective"
}
