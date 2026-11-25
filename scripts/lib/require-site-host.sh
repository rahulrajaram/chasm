#!/bin/sh

# Site descriptor and site identity for chasm host scripts.
#
# Source this file, then call require_site_host before any mutating host action.
# The library sources the site descriptor into SITE_* variables and defines:
#
#   site_host_fingerprint          print this machine's site fingerprint
#   require_site_host              verify the pinned identity, else refuse
#   provision_site_identity [--force]
#                                  pin this machine's identity (root, create-once)
#   site_value KEY                 print one descriptor value, or fail
#
# Executed directly, the file runs one of two modes:
#
#   require-site-host.sh --verify        (default) verify the pinned identity
#   require-site-host.sh --provision [--force]
#                                        pin this machine's identity
#   require-site-host.sh --fingerprint   print this machine's fingerprint
#
# The fingerprint formula is unchanged from the original host gate: hostname,
# machine-id, root filesystem UUID, and DMI product name. The pin lives in the
# root-owned 0444 marker file written by provisioning; nothing in the repository
# carries a pre-baked fingerprint for any host.

# shellcheck disable=SC2034
SITE_CONF_PATH=${CHASM_SITE_CONF:-/etc/chasm/site.conf}
SITE_MARKER_PATH=/etc/chasm/site-host-id
SITE_MARKER_MODE=444

site_fail() {
    echo "site identity: $*" >&2
    return 1
}

# site_require_descriptor verifies that the descriptor is a safe regular file,
# then parses it line by line instead of sourcing it. The descriptor is flat
# key=value with no quoting or expansions, so the two readers (this library and
# internal/siteconfig) must accept and reject exactly the same descriptors:
# unknown keys, repeated keys, values that are not plain tokens, malformed
# integers and booleans, unsafe names, and the cross-key rules are all fatal.
# The shell must never execute anything the descriptor contains, so nothing in
# this function is ever eval'ed; every value is validated as a plain token
# before it is stored.
site_require_descriptor() {
    [ -f "$SITE_CONF_PATH" ] && [ ! -L "$SITE_CONF_PATH" ] ||
        site_fail "site descriptor $SITE_CONF_PATH is missing or unsafe" || return 1
    [ -z "$(find "$SITE_CONF_PATH" -maxdepth 0 -perm /022 -print 2>/dev/null)" ] ||
        site_fail "site descriptor is group- or world-writable" || return 1
    site_descriptor_uid=$(stat -c '%u' "$SITE_CONF_PATH" 2>/dev/null || echo "")
    case "$site_descriptor_uid" in
        0|"$(id -u)") ;;
        *) site_fail "site descriptor is owned by neither root nor the invoking user" || return 1 ;;
    esac

    # Reinitialize: a previous failed parse must not leak values into this one.
    SITE_SITE_NAME=
    SITE_BRIDGE_NAME=
    SITE_BRIDGE_IPV4=
    SITE_SANDBOX_IPV4_PREFIX=
    SITE_STORAGE_POOL=
    SITE_POOL_SIZE_GIB=
    SITE_PROFILE_ROOT_DISK_GIB=
    SITE_PROFILE_NAME=
    SITE_BASE_IMAGE_ALIAS=
    SITE_BASE_IMAGE_REMOTE=
    SITE_OPERATOR_USER=
    SITE_GUEST_UID=
    SITE_SUPERVISOR_USER=
    SITE_HOST_RESERVE_MIB=
    SITE_DEFAULT_SANDBOX=
    SITE_BOOTSTRAP_PROFILE=
    SITE_RESOURCE_PREFIX=
    # Space-delimited list of keys seen so far, for the duplicate check.
    site_seen=

    # `|| [ -n "$site_line" ]` keeps a final line that has no trailing
    # newline: a bare `while read` would skip it, silently ignoring the last
    # key (including an unknown or malformed one).
    while IFS= read -r site_line || [ -n "$site_line" ]; do
        # A '#' always starts a comment, exactly as in the Go reader.
        site_line=${site_line%%#*}
        while [ "$site_line" != "${site_line#[ 	]}" ]; do site_line=${site_line#[ 	]}; done
        while [ "$site_line" != "${site_line%[ 	]}" ]; do site_line=${site_line%[ 	]}; done
        [ -n "$site_line" ] || continue
        case "$site_line" in
            *=*) ;;
            *) site_fail "site descriptor has a line that is not a key=value pair" || return 1 ;;
        esac
        site_key=${site_line%%=*}
        site_line_value=${site_line#*=}
        # The Go reader trims the key and the value again after splitting, so
        # whitespace around the separator is legal; do the same here.
        while [ "$site_key" != "${site_key#[ 	]}" ]; do site_key=${site_key#[ 	]}; done
        while [ "$site_key" != "${site_key%[ 	]}" ]; do site_key=${site_key%[ 	]}; done
        while [ "$site_line_value" != "${site_line_value#[ 	]}" ]; do site_line_value=${site_line_value#[ 	]}; done
        while [ "$site_line_value" != "${site_line_value%[ 	]}" ]; do site_line_value=${site_line_value%[ 	]}; done
        case "$site_key" in
            ''|*[!a-z0-9_]*|[0-9]*) site_fail "site descriptor has an invalid key" || return 1 ;;
        esac
        case " $site_seen " in
            *" $site_key "*) site_fail "site descriptor key $site_key is repeated" || return 1 ;;
        esac
        site_seen="$site_seen $site_key"
        # Values must be plain tokens: no whitespace, quotes, backslashes,
        # command substitutions, or parameter expansions.
        case "$site_line_value" in
            '' ) ;;
            *' '*|*"	"*|*'"'*|*"'"*|*"\\"*|*'$'*|*'`'*)
                site_fail "site descriptor key $site_key has a value that is not a plain token" || return 1 ;;
        esac
        # Integer and boolean keys validate their syntax immediately; the
        # per-key range rules are applied after the whole file is read.
        case "$site_key" in
            guest_uid|host_reserve_mib|pool_size_gib)
                site_is_int "$site_line_value" ||
                    site_fail "site descriptor key $site_key is not an integer" || return 1 ;;
        esac
        case "$site_key" in
            guest_uid)
                site_is_canonical_uid "$site_line_value" ||
                    site_fail "site descriptor key guest_uid is not a canonical positive integer (no sign, no leading zeros)" || return 1 ;;
            host_reserve_mib)
                site_is_canonical_mib "$site_line_value" ||
                    site_fail "site descriptor key host_reserve_mib is not a canonical nonnegative integer (no sign, no leading zeros)" || return 1 ;;
        esac
        case "$site_key" in
            site_name) SITE_SITE_NAME=$site_line_value ;;
            bridge_name) SITE_BRIDGE_NAME=$site_line_value ;;
            bridge_ipv4) SITE_BRIDGE_IPV4=$site_line_value ;;
            sandbox_ipv4_prefix) SITE_SANDBOX_IPV4_PREFIX=$site_line_value ;;
            storage_pool) SITE_STORAGE_POOL=$site_line_value ;;
            pool_size_gib) SITE_POOL_SIZE_GIB=$site_line_value ;;
            profile_root_disk_gib) SITE_PROFILE_ROOT_DISK_GIB=$site_line_value ;;
            profile_name) SITE_PROFILE_NAME=$site_line_value ;;
            base_image_alias) SITE_BASE_IMAGE_ALIAS=$site_line_value ;;
            base_image_remote) SITE_BASE_IMAGE_REMOTE=$site_line_value ;;
            operator_user) SITE_OPERATOR_USER=$site_line_value ;;
            guest_uid) SITE_GUEST_UID=$site_line_value ;;
            supervisor_user) SITE_SUPERVISOR_USER=$site_line_value ;;
            host_reserve_mib) SITE_HOST_RESERVE_MIB=$site_line_value ;;
            default_sandbox) SITE_DEFAULT_SANDBOX=$site_line_value ;;
            bootstrap_profile) SITE_BOOTSTRAP_PROFILE=$site_line_value ;;
            resource_prefix) SITE_RESOURCE_PREFIX=$site_line_value ;;
            *) site_fail "site descriptor has an unknown key $site_key" || return 1 ;;
        esac
    done < "$SITE_CONF_PATH"

    # Every schema key must be present; an empty value is still a present key
    # and is judged by its own rule.
    for site_required_key in \
        bridge_name bridge_ipv4 sandbox_ipv4_prefix storage_pool pool_size_gib \
        profile_name base_image_alias base_image_remote operator_user guest_uid \
        supervisor_user host_reserve_mib
    do
        case " $site_seen " in
            *" $site_required_key "*) ;;
            *) site_fail "site descriptor is missing a required key $site_required_key" || return 1 ;;
        esac
    done

    [ -n "$SITE_SITE_NAME" ] || SITE_SITE_NAME=$(site_kernel_hostname)
    [ -n "$SITE_SITE_NAME" ] ||
        site_fail "site_name is absent and the hostname is unavailable" || return 1
    [ -n "$SITE_OPERATOR_USER" ] || SITE_OPERATOR_USER=${SUDO_USER:-${USER:-${LOGNAME:-}}}
    [ -n "$SITE_OPERATOR_USER" ] ||
        site_fail "operator_user is absent and the invoking user cannot be resolved" || return 1
    [ -n "$SITE_BOOTSTRAP_PROFILE" ] || SITE_BOOTSTRAP_PROFILE=minimal
    # resource_prefix derives every Incus object name and user.<prefix>.*
    # metadata key the site owns; the default matches the Go reader's
    # DefaultResourcePrefix.
    [ -n "$SITE_RESOURCE_PREFIX" ] || SITE_RESOURCE_PREFIX=chasm
    printf '%s' "$SITE_RESOURCE_PREFIX" | grep -Eq '^[a-z][a-z0-9]*(-[a-z0-9]+)*$' ||
        site_fail "resource_prefix is not a safe name" || return 1
    # The bound keeps derived Incus names (<prefix>-workspace-sandbox-<name>,
    # <prefix>-template-<profile>-<identity>) inside Incus's name limits;
    # it must match the Go reader's MaxResourcePrefixLength.
    [ "${#SITE_RESOURCE_PREFIX}" -le 16 ] ||
        site_fail "resource_prefix is above the permitted range" || return 1

    for site_name_check in "$SITE_BRIDGE_NAME" "$SITE_STORAGE_POOL" "$SITE_PROFILE_NAME" "$SITE_SUPERVISOR_USER"; do
        printf '%s' "$site_name_check" | grep -Eq '^[a-z][a-z0-9]*(-[a-z0-9]+)*$' ||
            site_fail "site name $site_name_check is unsafe" || return 1
    done
    printf '%s' "$SITE_OPERATOR_USER" | grep -Eq '^[a-z_][a-z0-9_-]*$' ||
        site_fail "operator_user is not an unprivileged user name" || return 1
    case "$SITE_OPERATOR_USER" in
        root) site_fail "operator_user is not an unprivileged user name" || return 1 ;;
    esac
    [ "$SITE_OPERATOR_USER" != "$SITE_SUPERVISOR_USER" ] ||
        site_fail "operator_user and supervisor_user must differ" || return 1
    [ "$SITE_GUEST_UID" -gt 0 ] && [ "$SITE_GUEST_UID" -le 2147483647 ] ||
        site_fail "guest_uid is outside the permitted range" || return 1
    [ "$SITE_HOST_RESERVE_MIB" -ge 0 ] ||
        site_fail "host_reserve_mib must not be negative" || return 1
    # Upper bound shared with the Go reader (MaxHostReserveMiB): the
    # supported range is 0..1048576 MiB (1 TiB). It keeps the admission
    # math's MiB sums far inside signed 64-bit shell arithmetic, which
    # otherwise wraps for huge-but-numeric values and silently inverts the
    # memory comparisons.
    [ "$SITE_HOST_RESERVE_MIB" -le 1048576 ] ||
        site_fail "host_reserve_mib is above the permitted range" || return 1
    [ "$SITE_POOL_SIZE_GIB" -gt 0 ] ||
        site_fail "pool_size_gib must be positive" || return 1
    # An upper bound keeps the preflight KiB arithmetic (pool + headroom,
    # times 1048576) far inside signed 64-bit shell math, which otherwise
    # wraps for huge-but-previously-valid values and silently inverts the
    # disk-headroom comparison.
    [ "$SITE_POOL_SIZE_GIB" -le 65536 ] ||
        site_fail "pool_size_gib is above the permitted range" || return 1
    case " $site_seen " in
        *' profile_root_disk_gib '*)
            site_is_canonical_uid "$SITE_PROFILE_ROOT_DISK_GIB" &&
                [ "${#SITE_PROFILE_ROOT_DISK_GIB}" -le 4 ] &&
                [ "$SITE_PROFILE_ROOT_DISK_GIB" -le 4096 ] ||
                site_fail "profile_root_disk_gib must be a canonical positive integer (1..4096)" || return 1
            ;;
    esac
    [ -n "$SITE_BASE_IMAGE_ALIAS" ] ||
        site_fail "base_image_alias is required" || return 1
    case "$SITE_BASE_IMAGE_REMOTE" in
        *:*)
            [ -n "${SITE_BASE_IMAGE_REMOTE%%:*}" ] ||
                site_fail "base_image_remote must be remote:image" || return 1 ;;
        *)
            site_fail "base_image_remote must be remote:image" || return 1 ;;
    esac
    # bridge_ipv4 must be a /24 IPv4 CIDR: four decimal octets, no leading
    # zeros, each at most 255, and a mask of exactly 24 bits. The address may
    # carry host bits; the network prefix below is derived from its first
    # three octets, matching the Go reader.
    site_bridge_address=${SITE_BRIDGE_IPV4%/*}
    case "$SITE_BRIDGE_IPV4" in
        */*) ;;
        *) site_fail "bridge_ipv4 must be an IPv4 /24" || return 1 ;;
    esac
    site_bridge_mask=${SITE_BRIDGE_IPV4##*/}
    case "$site_bridge_mask" in
        ''|*[!0-9]*) site_fail "bridge_ipv4 must be an IPv4 /24" || return 1 ;;
    esac
    [ "$site_bridge_mask" -eq 24 ] ||
        site_fail "bridge_ipv4 must be an IPv4 /24" || return 1
    # Splitting the dotted address into its four octet words is the point
    # of this set; every octet is validated individually right below.
    # shellcheck disable=SC2046
    set -- $(printf '%s\n' "$site_bridge_address" | tr '.' ' ')
    [ "$#" -eq 4 ] || site_fail "bridge_ipv4 must be an IPv4 /24" || return 1
    site_bridge_ok=1
    for site_octet in "$@"; do
        printf '%s' "$site_octet" | grep -Eq '^(0|[1-9][0-9]{0,2})$' && [ "$site_octet" -le 255 ] ||
            { site_bridge_ok=0; break; }
    done
    [ "$site_bridge_ok" -eq 1 ] || site_fail "bridge_ipv4 must be an IPv4 /24" || return 1
    case "${site_bridge_address##*.}" in
        0|10|11|255) site_fail "bridge_ipv4 uses a reserved host address (.0, .10, .11, .255)" || return 1 ;;
    esac
    site_bridge_prefix=${site_bridge_address%.*}.
    [ "$site_bridge_prefix" = "$SITE_SANDBOX_IPV4_PREFIX" ] ||
        site_fail "sandbox_ipv4_prefix is not derivable from bridge_ipv4" || return 1
    if [ -n "$SITE_DEFAULT_SANDBOX" ]; then
        printf '%s' "$SITE_DEFAULT_SANDBOX" | grep -Eq '^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$' ||
            site_fail "default_sandbox is not a safe sandbox name" || return 1
    fi
    # bootstrap_profile is optional; empty or absent already resolved to the
    # default above. Boolean-looking values are not names and are refused even
    # though they match the name grammar, matching the Go reader.
    if [ -n "$SITE_BOOTSTRAP_PROFILE" ]; then
        case "$SITE_BOOTSTRAP_PROFILE" in
            true|false)
                site_fail "bootstrap_profile is not a safe sandbox name" || return 1 ;;
        esac
        printf '%s' "$SITE_BOOTSTRAP_PROFILE" | grep -Eq '^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$' ||
            site_fail "bootstrap_profile is not a safe sandbox name" || return 1
    fi
    return 0
}

# site_is_int VALUE: true iff VALUE is a decimal integer with an optional
# sign, matching strconv.Atoi's grammar and its int64 bounds. Values larger
# than 9223372036854775807 fail closed exactly where Atoi would, so the
# range comparisons below never need arithmetic beyond the shell's integer
# range.
site_is_int() {
    site_magnitude=${1#[-+]}
    case "$site_magnitude" in
        ''|*[!0-9]*) return 1 ;;
    esac
    if [ "${#site_magnitude}" -ge 19 ]; then
        # Lexical comparison is exact for fixed-length decimal strings, so
        # awk keeps this deterministic without shell arithmetic overflow.
        site_magnitude="$site_magnitude" awk 'BEGIN {
            m = ENVIRON["site_magnitude"]
            if (length(m) > 19 || (length(m) == 19 && m > "9223372036854775807")) exit 1
        }' || return 1
    fi
    return 0
}

# site_is_canonical_mib VALUE: true iff VALUE is a nonnegative decimal
# integer in canonical form — digits only, no sign, and no leading zeros
# ("0" itself is canonical). host_reserve_mib feeds the builder admission's
# $((...)) arithmetic (scripts/lib/host-resources.sh), which reads a
# leading-zero constant as octal, and a leading sign makes every admission
# comparison refuse, so both this reader and the Go reader
# (internal/siteconfig canonicalNonnegativeInt) reject non-canonical
# spellings outright; the descriptor-case fixtures reserve-leading-zero and
# reserve-plus pin the parity between the two readers.
site_is_canonical_mib() {
    case "$1" in
        0|[1-9]|[1-9][0-9]*) return 0 ;;
    esac
    return 1
}

# site_is_canonical_uid VALUE: site_is_canonical_mib without "0" — the
# canonical positive decimal spellings only. guest_uid is compared TEXTUALLY
# against the system uid by the guest layer (install-guest-packages.sh) and
# enters the template identity preimage verbatim, while the kernel reports
# the normalized numeric uid ("04242" becomes "4242", a "+" never
# survives), so a non-canonical spelling would pass the descriptor and then
# fail guest verification; both this reader and the Go reader
# (internal/siteconfig canonicalPositiveInt) therefore reject it outright,
# and the descriptor-case fixtures guest-uid-leading-zero and guest-uid-plus
# pin the parity.
site_is_canonical_uid() {
    case "$1" in
        [1-9]|[1-9][0-9]*) return 0 ;;
    esac
    return 1
}

site_value() {
    site_require_descriptor || return 1
    site_value_name=$1
    case "$site_value_name" in
        site_name) printf '%s\n' "$SITE_SITE_NAME" ;;
        bridge_name) printf '%s\n' "$SITE_BRIDGE_NAME" ;;
        bridge_ipv4) printf '%s\n' "$SITE_BRIDGE_IPV4" ;;
        bridge_address) printf '%s\n' "${SITE_BRIDGE_IPV4%/*}" ;;
        sandbox_ipv4_prefix) printf '%s\n' "$SITE_SANDBOX_IPV4_PREFIX" ;;
        storage_pool) printf '%s\n' "$SITE_STORAGE_POOL" ;;
        pool_size_gib) printf '%s\n' "$SITE_POOL_SIZE_GIB" ;;
        profile_root_disk_gib) printf '%s\n' "$SITE_PROFILE_ROOT_DISK_GIB" ;;
        profile_name) printf '%s\n' "$SITE_PROFILE_NAME" ;;
        base_image_alias) printf '%s\n' "$SITE_BASE_IMAGE_ALIAS" ;;
        base_image_remote) printf '%s\n' "$SITE_BASE_IMAGE_REMOTE" ;;
        operator_user) printf '%s\n' "$SITE_OPERATOR_USER" ;;
        guest_uid) printf '%s\n' "$SITE_GUEST_UID" ;;
        supervisor_user) printf '%s\n' "$SITE_SUPERVISOR_USER" ;;
        host_reserve_mib) printf '%s\n' "$SITE_HOST_RESERVE_MIB" ;;
        default_sandbox) printf '%s\n' "$SITE_DEFAULT_SANDBOX" ;;
        bootstrap_profile) printf '%s\n' "$SITE_BOOTSTRAP_PROFILE" ;;
        resource_prefix) printf '%s\n' "$SITE_RESOURCE_PREFIX" ;;
        *) site_fail "unknown site key $site_value_name" ;;
    esac
}

# site_nested_kvm_modules — the KVM modules whose nested parameter gates a
# host. Shared so the advisory preflight and the enforcing init script cannot
# diverge on which modules are checked.
site_nested_kvm_modules() {
    printf '%s\n' kvm_intel kvm_amd
}

# site_nested_kvm_disabled VALUE — the one nested-virtualization admission
# rule, shared by install-chasm.sh's advisory preflight and the enforcing
# initialize-incus.sh: the kernel renders a bool module parameter as Y/N or
# as 1/0 depending on the module, so anything that is not an explicit "off"
# refuses.
site_nested_kvm_disabled() {
    case "$1" in
        N|n|0) return 0 ;;
        *) return 1 ;;
    esac
}

site_kernel_hostname() {
    if hostname_value=$(uname -n 2>/dev/null) && [ -n "$hostname_value" ]; then
        printf '%s\n' "$hostname_value"
    elif [ -r /proc/sys/kernel/hostname ]; then
        IFS= read -r hostname_value </proc/sys/kernel/hostname || true
        [ -n "$hostname_value" ] || return 1
        printf '%s\n' "$hostname_value"
    else
        return 1
    fi
}

site_host_fingerprint() {
    {
        printf 'hostname=%s\n' "$(site_kernel_hostname)"
        printf 'machine_id=%s\n' "$(cat /etc/machine-id)"
        printf 'root_uuid=%s\n' "$(findmnt -n -o UUID /)"
        printf 'product_name=%s\n' "$(cat /sys/class/dmi/id/product_name 2>/dev/null || true)"
    } | sha256sum | cut -d' ' -f1
}

# require_site_host verifies that this machine is the site the marker pinned.
# It returns non-zero without explaining how to bypass anything: re-pinning is a
# deliberate provisioning action, not a recovery step.
require_site_host() {
    site_require_descriptor || return 1
    site_actual=$(site_host_fingerprint)
    [ -f "$SITE_MARKER_PATH" ] && [ ! -L "$SITE_MARKER_PATH" ] ||
        { site_fail "site identity marker $SITE_MARKER_PATH is missing or unsafe" || return 1; }
    [ "$(stat -c '%u:%a' "$SITE_MARKER_PATH" 2>/dev/null || true)" = "0:$SITE_MARKER_MODE" ] ||
        { site_fail "site identity marker ownership or mode is invalid" || return 1; }
    [ "$(cat "$SITE_MARKER_PATH" 2>/dev/null || true)" = "$site_actual" ] ||
        { site_fail "this host is not the pinned site" || return 1; }
    return 0
}

# provision_site_identity pins this machine: create-once, then refuse.
provision_site_identity() {
    site_require_descriptor || return 1
    [ "$(id -u)" -eq 0 ] ||
        { site_fail "provisioning the site identity requires root" || return 1; }
    site_force=0
    if [ "${1:-}" = "--force" ]; then
        site_force=1
    elif [ "${1:-}" != "" ]; then
        { site_fail "usage: provision_site_identity [--force]" || return 1; }
    fi

    site_actual=$(site_host_fingerprint)
    if [ -e "$SITE_MARKER_PATH" ]; then
        if [ "$site_force" -eq 0 ]; then
            require_site_host ||
                { site_fail "identity is already pinned and does not match this host; use --force to re-pin" || return 1; }
            echo "site identity: already pinned to $SITE_MARKER_PATH" >&2
            return 0
        fi
        echo "site identity: re-pinning $SITE_MARKER_PATH (--force)" >&2
    fi

    site_marker_dir=$(dirname "$SITE_MARKER_PATH")
    install -d -o root -g root -m 0755 "$site_marker_dir" || return 1
    site_marker_tmp=$SITE_MARKER_PATH.tmp.$$
    (umask 022 && printf '%s\n' "$site_actual" >"$site_marker_tmp") || return 1
    chown root:root "$site_marker_tmp" || return 1
    chmod "$SITE_MARKER_MODE" "$site_marker_tmp" || return 1
    mv -f "$site_marker_tmp" "$SITE_MARKER_PATH" || return 1
    sync "$SITE_MARKER_PATH" 2>/dev/null || true
    echo "site identity: pinned $SITE_MARKER_PATH to $site_actual" >&2
    return 0
}

# Executed directly, the library is a small identity tool.
if [ "$(basename -- "$0")" = "require-site-host.sh" ]; then
    set -eu
    site_mode=${1:---verify}
    case "$site_mode" in
        --verify)
            site_require_descriptor
            require_site_host
            echo "site identity: verified $SITE_CONF_PATH on $SITE_SITE_NAME"
            ;;
        --provision)
            site_require_descriptor
            provision_site_identity "${2:-}"
            ;;
        --fingerprint)
            site_host_fingerprint
            ;;
        *)
            echo "usage: $0 [--verify|--provision [--force]|--fingerprint]" >&2
            exit 2
            ;;
    esac
fi
