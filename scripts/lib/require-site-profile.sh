#!/bin/sh

# Bootstrap profile manifest and reader for chasm guest layers.
#
# Source this file, then call require_site_profile <name> before any mutating
# guest action that depends on the profile's sealed package set and pinned
# runtime revision. The library parses the named profile manifest into
# PROFILE_* variables and defines:
#
#   require_site_profile <name>      parse + validate one profile manifest
#   profile_value KEY                print one profile value, or fail
#
# A bootstrap profile is a flat manifest that names what a reproducible guest
# template seals: the Debian packages to install. The installed form lives at
# /etc/chasm/profiles/<name>.profile; the repository ships the source
# forms under guest-config/profiles/<name>.profile. The gate
# scripts/check-site-profiles.sh checks those repository forms against this
# reader and its recorded verdicts; it never reads the installed copies.
#
# Executed directly, the file parses and verifies one profile and prints a
# one-line confirmation:
#
#   require-site-profile.sh --verify <name>
#
# The manifest is read line by line and never sourced; nothing in it is ever
# eval'ed. Unknown keys, repeated keys, missing keys, duplicate or malformed
# packages, and any value that is not a plain token are all fatal. These rules
# are the shell-side contract; the gate holds every corpus verdict to them.

# shellcheck disable=SC2034
PROFILE_ROOT=${CHASM_PROFILE_ROOT:-/etc/chasm/profiles}

profile_fail() {
    echo "profile: $*" >&2
    return 1
}

# require_site_profile verifies that <name> is a safe profile name (the same
# safe-name grammar the site reader applies to default_sandbox) and that its
# manifest is a safe regular file, then parses the manifest line by line. The
# manifest is flat key=value with no quoting or expansions; values are plain
# tokens only. Nothing in the file is ever sourced or eval'ed, so a profile
# can never execute anything on the host.
#
# Sets, in the caller's shell: PROFILE_ID (the profile's own name; NOT
# PROFILE_NAME, which the builder and launcher reserve for the Incus profile
# and which sourcing this reader must never clobber), PROFILE_PACKAGES, and
# PROFILE_MANIFEST_SHA256. All PROFILE_*
# variables are reinitialized before parsing, so a failed parse cannot leak
# values from an earlier one.
require_site_profile() {
    profile_name_arg=$1
    printf '%s' "$profile_name_arg" | grep -Eq '^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$' ||
        profile_fail "profile name $profile_name_arg is unsafe" || return 1
    PROFILE_MANIFEST_PATH=$PROFILE_ROOT/$profile_name_arg.profile
    [ -f "$PROFILE_MANIFEST_PATH" ] && [ ! -L "$PROFILE_MANIFEST_PATH" ] ||
        profile_fail "profile manifest $PROFILE_MANIFEST_PATH is missing or unsafe" || return 1
    [ -z "$(find "$PROFILE_MANIFEST_PATH" -maxdepth 0 -perm /022 -print 2>/dev/null)" ] ||
        profile_fail "profile manifest $PROFILE_MANIFEST_PATH is group- or world-writable (mode $(stat -c '%a' "$PROFILE_MANIFEST_PATH" 2>/dev/null || echo unknown))" || return 1
    profile_uid=$(stat -c '%u' "$PROFILE_MANIFEST_PATH" 2>/dev/null || echo "")
    case "$profile_uid" in
        0|"$(id -u)") ;;
        *) profile_fail "profile manifest is owned by neither root nor the invoking user" || return 1 ;;
    esac

    # Reinitialize: a previous failed parse must not leak values into this one.
    # PROFILE_NAME is deliberately absent: it belongs to the Incus profile in
    # the builder and launcher, and this reader never sets it.
    PROFILE_ID=
    PROFILE_PACKAGES=
    PROFILE_MANIFEST_SHA256=
    profile_seen=

    # `|| [ -n "$profile_line" ]` keeps a final line that has no trailing
    # newline: a bare `while read` would skip it, silently ignoring the last
    # key (including an unknown or malformed one).
    while IFS= read -r profile_line || [ -n "$profile_line" ]; do
        # A '#' always starts a comment, exactly as in the site reader.
        profile_line=${profile_line%%#*}
        while [ "$profile_line" != "${profile_line#[ 	]}" ]; do profile_line=${profile_line#[ 	]}; done
        while [ "$profile_line" != "${profile_line%[ 	]}" ]; do profile_line=${profile_line%[ 	]}; done
        [ -n "$profile_line" ] || continue
        case "$profile_line" in
            *=*) ;;
            *) profile_fail "profile manifest has a line that is not a key=value pair" || return 1 ;;
        esac
        profile_key=${profile_line%%=*}
        profile_value=${profile_line#*=}
        # The value is trimmed again after the split, so whitespace around the
        # separator is legal; do the same as the site reader.
        while [ "$profile_key" != "${profile_key#[ 	]}" ]; do profile_key=${profile_key#[ 	]}; done
        while [ "$profile_key" != "${profile_key%[ 	]}" ]; do profile_key=${profile_key%[ 	]}; done
        while [ "$profile_value" != "${profile_value#[ 	]}" ]; do profile_value=${profile_value#[ 	]}; done
        while [ "$profile_value" != "${profile_value%[ 	]}" ]; do profile_value=${profile_value%[ 	]}; done
        case "$profile_key" in
            ''|*[!a-z0-9_]*|[0-9]*) profile_fail "profile manifest has an invalid key" || return 1 ;;
        esac
        case " $profile_seen " in
            *" $profile_key "*) profile_fail "profile manifest key $profile_key is repeated" || return 1 ;;
        esac
        profile_seen="$profile_seen $profile_key"
        # Values must be plain tokens: no whitespace, quotes, backslashes,
        # command substitutions, or parameter expansions.
        case "$profile_value" in
            '' ) ;;
            *' '*|*"	"*|*'"'*|*"'"*|*"\\"*|*'$'*|*'`'*)
                profile_fail "profile manifest key $profile_key has a value that is not a plain token" || return 1 ;;
        esac
        case "$profile_key" in
            packages) PROFILE_PACKAGES=$profile_value ;;
            *) profile_fail "profile manifest has an unknown key $profile_key" || return 1 ;;
        esac
    done < "$PROFILE_MANIFEST_PATH"

    # Every schema key must be present; an empty value is still a present key
    # and is judged below by its own rule (packages= fails its own check, so
    # an explicitly empty value never passes).
    case " $profile_seen " in
        *" packages "*) ;;
        *) profile_fail "profile manifest is missing a required key packages" || return 1 ;;
    esac

    # packages: non-empty comma-separated Debian package names, each
    # ^[a-z0-9][a-z0-9+.-]*$, no empty items, no duplicates.
    case "$PROFILE_PACKAGES" in
        '') profile_fail "profile packages must not be empty" || return 1 ;;
    esac
    profile_oldifs=$IFS
    IFS=,
    set -f
    # shellcheck disable=SC2086
    set -- $PROFILE_PACKAGES
    set +f
    IFS=$profile_oldifs
    profile_pkg_seen=
    for profile_pkg in "$@"; do
        case "$profile_pkg" in
            ''|*[!a-z0-9+.-]*) profile_fail "profile packages contain an invalid package name '$profile_pkg'" || return 1 ;;
        esac
        case "$profile_pkg" in
            [a-z0-9]*) ;;
            *) profile_fail "profile packages contain an invalid package name '$profile_pkg'" || return 1 ;;
        esac
        case " $profile_pkg_seen " in
            *" $profile_pkg "*) profile_fail "profile packages list duplicate package $profile_pkg" || return 1 ;;
        esac
        profile_pkg_seen="$profile_pkg_seen $profile_pkg"
    done

    PROFILE_ID=$profile_name_arg
    PROFILE_MANIFEST_SHA256=$(sha256sum "$PROFILE_MANIFEST_PATH" | cut -d' ' -f1)
    return 0
}

# profile_value KEY prints one profile value, or fails. It re-resolves the
# manifest named by PROFILE_ID so a value is never served from stale state.
profile_value() {
    require_site_profile "${PROFILE_ID:-}" || return 1
    profile_value_name=$1
    case "$profile_value_name" in
        name) printf '%s\n' "$PROFILE_ID" ;;
        packages) printf '%s\n' "$PROFILE_PACKAGES" ;;
        manifest_path) printf '%s\n' "$PROFILE_MANIFEST_PATH" ;;
        manifest_sha256) printf '%s\n' "$PROFILE_MANIFEST_SHA256" ;;
        *) profile_fail "unknown profile key $profile_value_name" ;;
    esac
}

# Executed directly, the library is a small profile verifier.
if [ "$(basename -- "$0")" = "require-site-profile.sh" ]; then
    set -eu
    if [ "${1:-}" = "--verify" ] && [ "$#" -eq 2 ]; then
        require_site_profile "$2"
        echo "profile verified: $PROFILE_ID (packages=$PROFILE_PACKAGES)"
    else
        echo "usage: $0 --verify <name>" >&2
        exit 2
    fi
fi
