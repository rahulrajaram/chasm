#!/bin/sh
set -eu

# Install the pinned package set from a chasm bootstrap profile into a guest,
# create the neutral guest account every core path assumes, and verify the
# sealed receipt. The interface is shared with the other
# guest layer scripts:
# one mode argument (--check|--apply) plus a required bootstrap profile name.
# Run through sudo as root; require_site_host runs first, and require_site_profile
# resolves the bootstrap profile the builder chose. A guest is idempotent:
# re-running --apply reinstalls nothing harmful and rewrites the receipt.
#
# The sealed package set is the deterministic union of the mandatory core
# packages (ca-certificates, zsh, systemd-timesyncd — see
# scripts/lib/core-package-union.sh) with the chosen profile's packages:
# a profile may declare only ADDON packages, while the core provides its
# own prerequisites (TLS trust, the guest account's zsh login shell, and
# the clock-recovery time-sync unit). The receipt records the effective
# installed list beside the raw chosen profile packages and the chosen
# profile manifest digest, so verification is bound to exactly what was
# installed and to the bytes that chose it.
#
# The guest account is template identity, not agent provisioning: the sealed
# scrub checks /home/agent ownership, the launcher provisions /workspace and
# /shared for the site guest uid, and `chasm shell` opens as this account
# with /usr/bin/zsh. The account carries no password, no supplementary
# groups, no sudo rights, and no per-user tooling. The account also carries
# a minimal /home/agent/.zshrc taken from guest-config/guest/zshrc, so an
# interactive zsh never runs its first-run zsh-newuser-install menu.

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=scripts/lib/require-site-host.sh
. "$SCRIPT_DIR/lib/require-site-host.sh"
# shellcheck source=scripts/lib/require-site-profile.sh
. "$SCRIPT_DIR/lib/require-site-profile.sh"
# shellcheck source=scripts/lib/core-package-union.sh
. "$SCRIPT_DIR/lib/core-package-union.sh"

fail() {
    echo "guest packages: $*" >&2
    exit 1
}

usage() {
    echo "usage: $0 --check|--apply <bootstrap-profile>" >&2
    exit 64
}

[ "$#" -eq 2 ] || usage
case "$1" in
    --check) mode=check ;;
    --apply) mode=apply ;;
    *) usage ;;
esac
BOOTSTRAP_PROFILE=$2

[ "$(id -u)" -eq 0 ] || fail "run through sudo as root"
require_site_host || fail "site identity check failed"

# The target VM defaults to the site's declared sandbox and fails closed when
# the site does not declare one.
# VM_NAME is the Incus instance name; the site's default_sandbox is a logical
# name, so the derived instance is sandbox-<default>. A site that declares no
# default requires an explicit VM_NAME.
if [ -z "${VM_NAME:-}" ]; then
    [ -n "$SITE_DEFAULT_SANDBOX" ] ||
        fail "the site descriptor declares no default_sandbox; pass VM_NAME explicitly"
    VM_NAME=sandbox-$SITE_DEFAULT_SANDBOX
fi
case "$VM_NAME" in
    [a-z0-9]*)
        case "$VM_NAME" in *[!a-z0-9-]*|*-) fail "unsafe VM name" ;; esac
        ;;
    *) fail "unsafe VM name" ;;
esac

require_site_profile "$BOOTSTRAP_PROFILE" ||
    fail "bootstrap profile could not be read"

# The effective package set: mandatory core unioned with the chosen
# profile's addon packages, deduplicated. The raw PROFILE_PACKAGES and
# PROFILE_MANIFEST_SHA256 stay pinned unchanged beside it for the receipt.
EFFECTIVE_PACKAGES=$(union_core_packages "$PROFILE_PACKAGES") ||
    fail "the effective package set could not be composed"

# The neutral guest zshrc ships in the provision tree beside the profiles and
# is template identity input (the identity helper digests it), so a change to
# it deliberately changes the sealed template identity.
provision_root=$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd)
GUEST_ZSHRC=$provision_root/guest-config/guest/zshrc
[ -f "$GUEST_ZSHRC" ] && [ ! -L "$GUEST_ZSHRC" ] ||
    fail "guest zshrc input is missing or unsafe: $GUEST_ZSHRC"
GUEST_ZSHRC_SUM=$(sha256sum "$GUEST_ZSHRC") ||
    fail "guest zshrc input could not be digested"
GUEST_ZSHRC_SHA256=${GUEST_ZSHRC_SUM%% *}

incus exec "$VM_NAME" -- true

# guest_is_current verifies, inside the guest, that the sealed profile receipt
# for this bootstrap profile exists, is a safe regular file owned by root with
# mode 0600 under a root-owned 0700 directory, and records exactly the profile
# id, package list, and guest zshrc digest this manifest names, and that the
# guest account (including its /home/agent/.zshrc) still matches. Absence is
# the empty state for a fresh template; the --check mode turns that into a
# fail.
guest_is_current() {
    # Expansion is intentionally deferred to the shell inside the guest.
    # shellcheck disable=SC2016
    incus exec "$VM_NAME" -- sh -eu -c '
        profile=$1
        packages=$2
        profile_packages=$3
        manifest_sha256=$4
        guest_uid=$5
        zshrc_sha256=$6
        dir=/var/lib/chasm/packages/$profile
        receipt=$dir/receipt

        test -d "$dir"
        test ! -L "$dir"
        test "$(stat -c %U:%G:%a "$dir")" = root:root:700
        test -f "$receipt"
        test ! -L "$receipt"
        test "$(stat -c %U:%G:%a "$receipt")" = root:root:600
        grep -Fx "profile=$profile" "$receipt" >/dev/null
        grep -Fx "packages=$packages" "$receipt" >/dev/null
        grep -Fx "profile_packages=$profile_packages" "$receipt" >/dev/null
        grep -Fx "manifest_sha256=$manifest_sha256" "$receipt" >/dev/null
        grep -Fx "guest_uid=$guest_uid" "$receipt" >/dev/null
        grep -Fx "zshrc_sha256=$zshrc_sha256" "$receipt" >/dev/null
        grep -Fx "result=passed" "$receipt" >/dev/null
        test "$(id -u agent 2>/dev/null)" = "$guest_uid"
        test "$(id -g agent 2>/dev/null)" = "$guest_uid"
        test "$(getent passwd agent | cut -d: -f6)" = /home/agent
        test "$(getent passwd agent | cut -d: -f7)" = /usr/bin/zsh
        test -d /home/agent
        test ! -L /home/agent
        test "$(stat -c %U:%G /home/agent)" = agent:agent
        test -f /home/agent/.zshrc
        test ! -L /home/agent/.zshrc
        test "$(stat -c %U:%G:%a /home/agent/.zshrc)" = agent:agent:644
        test "$(sha256sum /home/agent/.zshrc | cut -d" " -f1)" = "$zshrc_sha256"
        test "$(systemctl is-enabled systemd-timesyncd 2>/dev/null)" = enabled
    ' sh "$BOOTSTRAP_PROFILE" "$EFFECTIVE_PACKAGES" "$PROFILE_PACKAGES" \
        "$PROFILE_MANIFEST_SHA256" "$SITE_GUEST_UID" "$GUEST_ZSHRC_SHA256"
}

if guest_is_current; then
    echo "guest packages: current profile=$BOOTSTRAP_PROFILE packages=$EFFECTIVE_PACKAGES"
    exit 0
fi
[ "$mode" = apply ] || fail "guest packages are absent, incomplete, or drifted"

# The zshrc content crosses as an argument (stdin carries the script); the
# guest re-digests what it wrote and compares it to the host-side digest, so
# transport mangling fails closed. The input must end with exactly one
# newline for the round trip to be byte-exact.
incus exec "$VM_NAME" -- sh -s -- "$BOOTSTRAP_PROFILE" "$EFFECTIVE_PACKAGES" \
    "$PROFILE_PACKAGES" "$PROFILE_MANIFEST_SHA256" "$SITE_GUEST_UID" \
    "$GUEST_ZSHRC_SHA256" "$(cat "$GUEST_ZSHRC")" <<'GUEST_SCRIPT'
set -eu

profile=$1
effective_packages=$2
profile_packages=$3
manifest_sha256=$4
guest_uid=$5
zshrc_sha256=$6
zshrc_content=$7

fail() {
    echo "guest packages installer: $*" >&2
    return 1
}

# Guard every external tool before it is used, so a missing tool fails with a
# clear message instead of a shell error.
for tool in apt-get dpkg-query install chmod mv mktemp stat cut getent groupadd useradd sha256sum rm systemctl; do
    command -v "$tool" >/dev/null 2>&1 ||
        fail "guest packages: missing tool: $tool"
done

case "$guest_uid" in
    ''|*[!0-9]*) fail "guest packages: guest uid is invalid" ;;
esac
[ "$guest_uid" -gt 0 ] || fail "guest packages: guest uid is invalid"

# Re-validate the profile name inside the guest before it ever touches a path.
case "$profile" in
    ''|*[!a-z0-9-]*) fail "guest packages: unsafe profile name" ;;
    -*) fail "guest packages: unsafe profile name" ;;
    *-) fail "guest packages: unsafe profile name" ;;
esac
case "$profile" in
    [a-z0-9]*) ;;
    *) fail "guest packages: unsafe profile name" ;;
esac

# The comma-separated effective list (mandatory core unioned with the
# chosen profile's addon packages) was composed and validated host-side;
# split it on commas inside the guest and re-validate every item, so
# nothing is ever word-split unvalidated and no item can smuggle a shell
# metacharacter.
case "$effective_packages" in
    ''|,*|*,|*,,*) fail "guest packages: malformed package list" ;;
esac

install -d -o root -g root -m 0700 "/var/lib/chasm/packages/$profile"

# The staging directory must be exactly a root-owned 0700 directory: `install
# -d` creates missing components with the given attributes but never repairs
# an existing one, and the untrusted agent account must have no write access
# to the directory that receives the pending zshrc (below) and the sealed
# receipt (at the end). Anything else fails closed instead of letting a root
# write follow an agent-planted name.
pkg_dir=/var/lib/chasm/packages/$profile
[ -d "$pkg_dir" ] && [ ! -L "$pkg_dir" ] &&
    [ "$(stat -c %U:%G:%a "$pkg_dir")" = root:root:700 ] ||
    fail "guest packages: staging directory is unsafe: $pkg_dir"

pkg_argv=
old_ifs=$IFS
IFS=,
set -f
# shellcheck disable=SC2086
for pkg in $effective_packages; do
    case "$pkg" in
        ''|*[!a-z0-9+.-]*) fail "guest packages: invalid package name: $pkg" ;;
    esac
    case "$pkg" in
        [a-z0-9]*) ;;
        *) fail "guest packages: invalid package name: $pkg" ;;
    esac
    case "$pkg_argv" in
        '') pkg_argv=$pkg ;;
        *) pkg_argv="$pkg_argv $pkg" ;;
    esac
done
set +f
IFS=$old_ifs

export DEBIAN_FRONTEND=noninteractive
apt-get update

# shellcheck disable=SC2086
apt-get install -y $pkg_argv

# Postflight: every package in the effective set must now report installed,
# so a
# silent partial install still fails closed.
set -f
for pkg in $pkg_argv; do
    if [ "$(dpkg-query -W -f='${db:Status-Status}' "$pkg" 2>/dev/null)" != installed ]; then
        fail "guest packages: package did not install: $pkg"
    fi
done
set +f

# Guest clock: the sealed template must carry an enabled systemd-timesyncd,
# so a guest can resynchronize its wall clock after a long pause (a frozen
# VM keeps its pre-pause clock, and without an NTP client resumed guests
# have lagged the host by hours, breaking timestamps and TLS). Enabling is
# offline-safe: it only creates the unit's wants symlink.
systemctl enable systemd-timesyncd >/dev/null 2>&1 ||
    fail "guest packages: systemd-timesyncd could not be enabled"

# The neutral guest account every core path assumes: name `agent`, uid and
# gid pinned to the site guest_uid (the launcher, `chasm shell`, and the
# exec-server catalog all execute as this numeric identity), home /home/agent
# owned agent:agent with mode 0750, and the zsh login shell `chasm shell`
# opens. zsh is a mandatory core package, so its presence is a core
# postflight check, not a profile obligation; fail closed if the core set
# did not land. No password, no
# supplementary groups, no sudo rights, and no linger are provisioned.
[ -x /usr/bin/zsh ] ||
    fail "guest packages: /usr/bin/zsh is missing; the core package set did not install"
if getent passwd agent >/dev/null 2>&1; then
    [ "$(id -u agent)" = "$guest_uid" ] && [ "$(id -g agent)" = "$guest_uid" ] ||
        fail "guest account agent already exists with a different uid or gid"
    [ "$(getent passwd agent | cut -d: -f6)" = /home/agent ] &&
        [ "$(getent passwd agent | cut -d: -f7)" = /usr/bin/zsh ] ||
        fail "guest account agent already exists with a different home or shell"
else
    if getent group agent >/dev/null 2>&1; then
        [ "$(getent group agent | cut -d: -f3)" = "$guest_uid" ] ||
            fail "guest group agent already exists with a different gid"
    else
        groupadd --gid "$guest_uid" agent
    fi
    useradd --uid "$guest_uid" --gid "$guest_uid" --create-home \
        --home-dir /home/agent --shell /usr/bin/zsh agent
fi
chmod 0750 /home/agent

# The neutral zsh startup file: without it an interactive zsh runs its
# first-run zsh-newuser-install menu, which `chasm shell` would surface to
# the user. Resume rule: an existing /home/agent/.zshrc is never overwritten.
# The file is installed only when absent, and a present file that drifts
# from the template input (content, owner, or mode) fails closed, so a
# deliberately customized guest zshrc surfaces as an explicit decision
# instead of silent data loss.
if [ -e /home/agent/.zshrc ] || [ -L /home/agent/.zshrc ]; then
    [ -f /home/agent/.zshrc ] && [ ! -L /home/agent/.zshrc ] &&
        [ "$(stat -c %U:%G:%a /home/agent/.zshrc)" = agent:agent:644 ] &&
        [ "$(sha256sum /home/agent/.zshrc | cut -d' ' -f1)" = "$zshrc_sha256" ] ||
        fail "guest zshrc already exists with different content or attributes; remove it inside the guest to re-provision"
else
    # Stage the pending zshrc outside the agent-owned home: the staging
    # directory above is root-owned 0700, so the agent cannot plant any name
    # in it, and mktemp creates the pending file with O_EXCL semantics and
    # mode 0600, so no pre-existing entry (regular file or symlink) is ever
    # truncated or written through. Only after the staged bytes match the
    # template input digest does `install` copy them into place; install
    # unlinks its destination, so even a symlink races to a replaced link,
    # never a written-through target.
    zshrc_tmp=$(mktemp "$pkg_dir/.zshrc.pending.XXXXXX") ||
        fail "guest packages: could not stage the guest zshrc"
    printf '%s\n' "$zshrc_content" >"$zshrc_tmp"
    [ "$(sha256sum "$zshrc_tmp" | cut -d' ' -f1)" = "$zshrc_sha256" ] ||
        fail "guest zshrc content does not match the template input digest"
    [ ! -e /home/agent/.zshrc ] && [ ! -L /home/agent/.zshrc ] ||
        fail "guest packages: /home/agent/.zshrc appeared while provisioning; refusing to overwrite"
    install -o agent -g agent -m 0644 -- "$zshrc_tmp" /home/agent/.zshrc
    rm -f -- "$zshrc_tmp"
fi

# Postflight: the account contract must hold on every exit, so drift or a
# partially applied layer still fails closed.
[ "$(id -u agent)" = "$guest_uid" ] && [ "$(id -g agent)" = "$guest_uid" ] ||
    fail "guest account postflight failed: uid/gid"
[ "$(getent passwd agent | cut -d: -f6)" = /home/agent ] &&
    [ "$(getent passwd agent | cut -d: -f7)" = /usr/bin/zsh ] ||
    fail "guest account postflight failed: home/shell"
[ -d /home/agent ] && [ ! -L /home/agent ] ||
    fail "guest account postflight failed: home is unsafe"
[ "$(stat -c %U:%G /home/agent)" = agent:agent ] ||
    fail "guest account postflight failed: home ownership"
[ "$(stat -c %a /home/agent)" = 750 ] ||
    fail "guest account postflight failed: home mode"
[ -f /home/agent/.zshrc ] && [ ! -L /home/agent/.zshrc ] ||
    fail "guest account postflight failed: zshrc missing"
[ "$(stat -c %U:%G:%a /home/agent/.zshrc)" = agent:agent:644 ] ||
    fail "guest account postflight failed: zshrc ownership or mode"
[ "$(sha256sum /home/agent/.zshrc | cut -d' ' -f1)" = "$zshrc_sha256" ] ||
    fail "guest account postflight failed: zshrc content"

# Postflight: the time-sync unit must be enabled, or a sealed template would
# freeze a guest whose clock cannot recover after a long pause.
[ "$(systemctl is-enabled systemd-timesyncd 2>/dev/null)" = enabled ] ||
    fail "guest packages postflight failed: systemd-timesyncd is not enabled"

receipt=/var/lib/chasm/packages/$profile/receipt
receipt_tmp=$receipt.pending.$$
printf '%s\n' \
    "profile=$profile" \
    "packages=$effective_packages" \
    "profile_packages=$profile_packages" \
    "manifest_sha256=$manifest_sha256" \
    "guest_uid=$guest_uid" \
    "zshrc_sha256=$zshrc_sha256" \
    "result=passed" >"$receipt_tmp"
chmod 0600 "$receipt_tmp"
mv -- "$receipt_tmp" "$receipt"

echo "guest packages installer: installed profile=$profile packages=$effective_packages"
GUEST_SCRIPT

guest_is_current || fail "guest packages postflight failed"
echo "guest packages: installed profile=$BOOTSTRAP_PROFILE packages=$EFFECTIVE_PACKAGES"
