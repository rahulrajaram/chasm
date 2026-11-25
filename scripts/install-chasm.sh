#!/bin/sh
set -eu

# install-chasm.sh: the site installer.
#
# Modes:
#   --check      read-only drift report (site descriptor vs system state)
#   --provision  first run on a fresh host; every root step is approved
#                individually before it runs
#   --verify     identity + drift + service health; also the default when the
#                script is run with no argument or on re-run
#
#   --harden-ssh applies the (off-by-default) sshd hardening step of
#   --provision; see the safety note there.
#
# Provisioning order (each step detects and skips work that already happened):
#   1. preflight (read-only): Debian family, systemd, nftables, KVM with
#      nested virtualization disabled, disk headroom (the full pool size
#      when the pool does not exist yet; the margin plus the remaining
#      growth of the existing loop pool on a re-run), Docker detection,
#      bridge-prefix collision (the site's own bridge route is accepted),
#      Go toolchain, Incus presence.
#   2. write /etc/chasm/site.conf from host-config/site.conf.example.
#   3. pin the site identity marker (create-once).
#   4. build the Go binaries and install the payload: binaries, the provision
#      tree (scripts, guest-config, host-config), the firewall helper with its
#      site-identity library and ruleset template, then the control plane
#      (units, sudoers, sysusers/tmpfiles, sockets) via install-control-plane.sh.
#   5. optionally harden the host ssh daemon (off by default; --harden-ssh
#      installs the sshd_config drop-ins from host-config/ssh, validates
#      with sshd -t, and reloads the active ssh service). The step warns
#      before approval that it disables SSH password and root login and
#      that the operator must already hold a working SSH key, and its
#      approval must be typed interactively (CHASM_ASSUME_YES does not
#      cover it). Staging is signal-safe: copies of the previous drop-ins
#      are restored on any failure or interruption until the step succeeds.
#   6. initialize Incus from the rendered preseed (initialize-incus.sh).
#   7. build the sealed template from the site's bootstrap profile
#      (build-sandbox-template.sh).
#   8. post-install verify and the quickstart.
#
# Incus, when absent, is installed from Debian's own repositories only (no
# third-party source) as its own approved step: the release's own packages
# on trixie (13) and newer, bookworm-backports on bookworm (12).
#
# Approvals: every root step prompts on /dev/tty before it runs. Setting
# CHASM_ASSUME_YES=1 pre-approves every step; that is the operator's own
# explicit choice and is logged when it happens.
#
# --check and --verify perform no mutation. Probes that need root (Incus,
# nftables) are skipped with an explanation when run unprivileged.

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
PROVISION_ROOT=$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd)

# A root invocation must not evaluate shell code from a checkout another user
# can modify. Unprivileged check and verify modes can use caller-owned
# checkouts; the root guard runs before the first dot command.
checkout_guard_fail() {
    echo "install-chasm.sh: refusing unsafe checkout: $1" >&2
    exit 1
}

if [ "$(id -u)" -eq 0 ]; then
    CHECKOUT_ALLOWED_UID=0
    case ${SUDO_UID-} in
        ''|*[!0-9]*) ;;
        *) CHECKOUT_ALLOWED_UID=$SUDO_UID ;;
    esac
    [ ! -L "$0" ] || checkout_guard_fail "installer entry point is a symlink: $0"

    # One root-only tree walk checks owner, world-write, and symlink targets
    # before any checkout code or data is evaluated. Git metadata is excluded;
    # links into it are still refused. Avoid parsing file names in the shell.
    checkout_guard_unsafe=$(find "$PROVISION_ROOT" -path "$PROVISION_ROOT/.git" ! -type l -prune -o \
        \( -type l -a ! -exec sh -c '
            target=$(readlink -f "$1" 2>/dev/null) || exit 1
            case $target in "$2/.git"|"$2/.git"/*) exit 1 ;; esac
            case $target in "$2"|"$2"/*) exit 0 ;; *) exit 1 ;; esac
        ' sh {} "$PROVISION_ROOT" \; \) -print -o \
        \( ! -type l -a \( \( ! -uid 0 -a ! -uid "$CHECKOUT_ALLOWED_UID" \) -o -perm -0002 \) \) -print 2>/dev/null) ||
        checkout_guard_fail "cannot enumerate or inspect checkout paths"
    [ -z "$checkout_guard_unsafe" ] ||
        checkout_guard_fail "unsafe checkout entries (untrusted owner, world-writable path, or escaping symlink): $checkout_guard_unsafe"
    checkout_guard_group=$(find "$PROVISION_ROOT" -path "$PROVISION_ROOT/.git" ! -type l -prune -o \
        ! -type l -perm -0020 -print -quit 2>/dev/null) ||
        checkout_guard_fail "cannot inspect group-write modes in checkout"
    if [ -n "$checkout_guard_group" ]; then
        echo "install-chasm.sh: warning: checkout has group-writable paths (for example $checkout_guard_group); root will use these files" >&2
    fi
fi

# shellcheck source=scripts/lib/require-site-host.sh
. "$SCRIPT_DIR/lib/require-site-host.sh"
# shellcheck source=scripts/lib/incus-names.sh
. "$SCRIPT_DIR/lib/incus-names.sh"
# The sshd-hardening and Incus-acquisition steps are optional plugins
# (plugins/ssh-hardening, plugins/install-incus; see docs/plugins.md). The
# installer sources them ONLY when the operator explicitly requests the
# matching flag; with no flag core has no sshd or distro package-install
# side effect and the plugin directories may be entirely absent.
# shellcheck source=scripts/lib/host-resources.sh
. "$SCRIPT_DIR/lib/host-resources.sh"
# shellcheck source=scripts/lib/provision-tree.sh
. "$SCRIPT_DIR/lib/provision-tree.sh"

INSTALLED_SITE_CONF=/etc/chasm/site.conf
INSTALL_LIB=/usr/local/lib/chasm
INSTALL_PROVISION=$INSTALL_LIB/provision
INSTALL_GO=/usr/local/sbin
INSTALL_OLD_GO=$INSTALL_LIB/go
INSTALL_INTERIM_GO=/usr/local/libexec/chasm
INSTALL_BIN=/usr/local/bin
INSTALL_FIREWALL_HELPER=/usr/local/sbin/chasm-firewall
INSTALL_FIREWALL_TEMPLATE=/etc/chasm/firewall.nft.template
# Used by the sourced ssh-hardening plugin.
# shellcheck disable=SC2034
INSTALL_SSH_CONFIG_DIR=/etc/ssh/sshd_config.d
INSTALL_SITE_HOST_LIB=$INSTALL_LIB/require-site-host.sh

# The loop pool lives on the filesystem that carries /var/lib/incus/disks; the
# headroom check keeps a fixed margin above the pool size.
POOL_HEADROOM_GIB=10

GO_BINARIES="chasm chasmctl executor_server supervisor_server"
go_install_path() {
    case $1 in
        chasm|chasmctl) printf '%s/%s\n' "$INSTALL_BIN" "$1" ;;
        executor_server) printf '%s/chasm-executor-server\n' "$INSTALL_GO" ;;
        supervisor_server) printf '%s/chasm-supervisor-server\n' "$INSTALL_GO" ;;
        *) fail "unknown Go binary: $1" ;;
    esac
}
# Core's own reserved guest profile names: refreshed into the production
# profile root only when absent (docs/plugins.md, upgrade semantics).
CORE_PROFILE_NAMES="minimal"
SSH_HARDENING_PLUGIN=$PROVISION_ROOT/plugins/ssh-hardening/ssh-hardening.sh
SSH_HARDENING_DROPINS=$PROVISION_ROOT/plugins/ssh-hardening/drop-ins
INCUS_INSTALL_PLUGIN=$PROVISION_ROOT/plugins/install-incus/incus-install.sh
UNITS="chasm-executor.service chasm-executor.socket \
chasm-firewall.service chasm-supervisor.service \
chasm-supervisor.socket"

fail() {
    echo "install: $*" >&2
    exit 1
}

usage() {
    cat >&2 <<'USAGE'
usage: install-chasm.sh [--help|--check|--provision|--verify]
                        [--harden-ssh] [--install-incus]

  --help       show this help and exit
  --check      alias of --verify with a check summary label
  --provision  first-run installation; every root step prompts first
  --verify     identity, drift, and service health (default with no argument
               and on re-run)
  --harden-ssh
               apply the sshd hardening step of --provision (off by
               default; requires the plugins/ssh-hardening add-on); loud
               warning, no key guard, approval must be typed
               (CHASM_ASSUME_YES does not cover it)
  --install-incus
               install Incus with --provision when it is absent (off by
               default; requires the plugins/install-incus add-on, Debian
               12/13 only). Without the flag core installs no packages:
               install incus and virtiofsd yourself.
  --allow-untested-host
               explicitly bypass the non-Debian qualification gate for
               --provision; prerequisites still must pass. Equivalent to
               CHASM_ALLOW_UNTESTED_HOST=1; does not mark the distro tested.

The plugin flags apply only to --provision and are rejected (usage error)
with any other mode.
USAGE
    exit 2
}

show_help() {
    cat <<'HELP'
install-chasm.sh — provision, check, and verify a chasm site

usage: install-chasm.sh [--help|--check|--provision|--verify]
                        [--harden-ssh] [--install-incus]

Modes:
  --help       show this help and exit
  --check      read-only drift report (no mutation, no approvals); an alias
               of --verify with a check summary label
  --provision  first-run installation; every root step prompts first
               (CHASM_ASSUME_YES=1 pre-approves every step)
  --verify     identity, drift, and service health; the default when no
               mode is given and on re-run

Options (--provision only):
  --harden-ssh
               apply the sshd hardening step of --provision. Hardening is
               OFF by default: the step installs sshd_config drop-ins that
               disable SSH password and root login (public-key login only),
               so pass this flag only when you already hold a working SSH
               key or have console access. The step warns before approval
               and restores the previous drop-ins on any failure. Its
               approval must be typed (yes); CHASM_ASSUME_YES does not
               cover it. Requires the plugins/ssh-hardening add-on.
  --install-incus
               install Incus when it is absent, from Debian's own
               repositories (trixie and newer) or bookworm-backports
               (bookworm). OFF by default: without the flag the installer
               installs no packages and requires incus and virtiofsd to be
               present already. Requires the plugins/install-incus add-on.
  --allow-untested-host
               explicitly proceed on a non-Debian, unqualified distro.
               Capability checks still run; this does not qualify the host.

Environment:
  CHASM_ASSUME_YES=1   pre-approve every provisioning step
  CHASM_ALLOW_UNTESTED_HOST=1   explicitly bypass the non-Debian qualification gate
  CHASM_SITE_CONF      alternate descriptor path (rejected under --provision)

Steps --provision runs (one approval per step):
  preflight, Incus install (only with --install-incus and only if absent;
  otherwise incus and virtiofsd must already be present), descriptor write,
  identity pin, payload install (binaries, provision tree, firewall helper,
  control plane, core profile manifests), sshd hardening drop-ins (opt-in
  with --harden-ssh, off by default), Incus initialization, sealed template
  build, post-install verify and quickstart.
HELP
    exit 0
}

# approve STEP: refuse unless the operator says yes on the terminal.
approve() {
    if [ "${CHASM_ASSUME_YES:-}" = "1" ]; then
        echo "install: step approved via CHASM_ASSUME_YES: $1"
        return 0
    fi
    if [ ! -r /dev/tty ]; then
        fail "step '$1' needs approval but no terminal is available (set CHASM_ASSUME_YES=1 to pre-approve)"
    fi
    printf 'install: %s\nProceed? [y/N] ' "$1" > /dev/tty
    answer=
    IFS= read -r answer < /dev/tty || answer=
    case "$answer" in
        y|Y|yes|YES|Yes) return 0 ;;
        *) fail "step refused by the operator: $1" ;;
    esac
}

am_root() {
    [ "$(id -u)" -eq 0 ]
}

# approve_hardening STEP: the hardening step's approval. Unlike approve, it
# is never satisfied by CHASM_ASSUME_YES: disabling SSH password and root
# login can lock the operator out of the host, so the confirmation must be
# typed (yes) on the terminal even on otherwise unattended runs.
approve_hardening() {
    if [ ! -r /dev/tty ] || [ ! -w /dev/tty ]; then
        fail "step '$1' needs an explicit typed confirmation and no terminal is available; CHASM_ASSUME_YES does not cover --harden-ssh"
    fi
    printf 'install: %s\nType yes to proceed: ' "$1" > /dev/tty
    hardening_answer=
    IFS= read -r hardening_answer < /dev/tty || hardening_answer=
    case "$hardening_answer" in
        yes) return 0 ;;
        *) fail "step refused by the operator: $1" ;;
    esac
}

# --- drift probes ------------------------------------------------------------

DRIFT_COUNT=0
SKIP_COUNT=0
DESCRIPTOR_OK=0

ok()    { echo "ok    $*"; }
drift() { echo "DRIFT $*"; DRIFT_COUNT=$((DRIFT_COUNT + 1)); }
skip()  { echo "skip  $* (needs root)"; SKIP_COUNT=$((SKIP_COUNT + 1)); }

# template_sealed INSTANCE
#
# The completion contract shared with the build script's fast path and the
# launch gate: the sealed snapshot exists and the bound receipt records
# result=passed. A name-only match would report a half-sealed stranded
# instance — a seal interrupted after incus move — as present, so the drift
# report lies about exactly the state that needs attention and the build
# step would skip the rebuild forever. Root reads the receipt; an
# unprivileged run falls back to the snapshot signal, which already
# distinguishes the dominant stranding (snapshot never created).
template_sealed() {
    instance=$1
    [ "$(incus snapshot list "$instance" --format csv 2>/dev/null | awk -F, '$1 == "sealed" { print $1; exit }')" = sealed ] || return 1
    receipt=$(incus config get "$instance" "$(incus_metadata_key "$SITE_RESOURCE_PREFIX" template-receipt)" 2>/dev/null) || return 1
    [ -n "$receipt" ] || return 1
    if am_root; then
        [ -f "$receipt" ] && [ ! -L "$receipt" ] || return 1
        grep -Fx result=passed "$receipt" >/dev/null 2>&1 || return 1
    fi
    return 0
}

# template_instance_for_profile — the matching template instance name, if any.
# The names derive from the site's resource_prefix, so a site that adopted
# another prefix never matches a foreign template.
template_instance_for_profile() {
    incus list --format csv -c n 2>/dev/null \
        | grep -E "$(incus_template_instance_pattern "$SITE_RESOURCE_PREFIX" "$1")" \
        | head -n1 \
        || true
}

check_file() {
    if [ -f "$1" ] && [ ! -L "$1" ]; then
        ok "$2 present: $1"
    else
        drift "$2 missing or unsafe: $1"
    fi
}

check_exec() {
    if [ -x "$1" ] && [ ! -L "$1" ]; then
        ok "$2 present: $1"
    else
        drift "$2 missing or not executable: $1"
    fi
}

check_symlink() {
    if [ -L "$1" ] && [ "$(readlink "$1")" = "$2" ]; then
        ok "$3: $1 -> $2"
    else
        drift "$3 wrong or missing: $1 (expected -> $2)"
    fi
}

firewall_chain_verified() {
    chain_text=$(nft -y -s list chain inet chasm "$1") || return 1
    printf '%s\n' "$chain_text" | awk -v hook="$1" -v bridge="$SITE_BRIDGE_NAME" '
        !NF { next }
        { $1=$1 }
        $0 == "chain " hook " {" { inside=1; next }
        inside && NF {
            if (!attached) {
                if ($0 != "type filter hook " hook " priority -10; policy accept;") exit
                attached=1
                next
            }
            passed=($0 == "iifname \"" bridge "\" meta nfproto ipv6 counter drop")
            exit
        }
        END { exit !passed }
    '
}

drift_report() {
    echo "install: drift report"
    DESCRIPTOR_OK=0

    check_file "$SITE_CONF_PATH" "site descriptor"
    if [ -f "$SITE_CONF_PATH" ]; then
        if site_require_descriptor; then
            ok "descriptor parses and validates (site=$SITE_SITE_NAME profile=$SITE_BOOTSTRAP_PROFILE)"
            DESCRIPTOR_OK=1
        else
            drift "descriptor fails to parse or validate"
        fi
    fi
    if am_root; then
        if [ -f "$SITE_CONF_PATH" ]; then
            [ "$(stat -c '%U:%G:%a' "$SITE_CONF_PATH")" = "root:root:644" ] ||
                drift "site descriptor metadata is not root:root 0644"
        fi
    else
        skip "site descriptor metadata"
    fi

    if [ -f "$SITE_MARKER_PATH" ]; then
        if require_site_host; then
            ok "site identity verified against the pinned marker"
        else
            drift "site identity verification failed (descriptor or marker)"
        fi
        if am_root; then
            [ "$(stat -c '%u:%a' "$SITE_MARKER_PATH")" = "0:$SITE_MARKER_MODE" ] ||
                drift "site identity marker metadata is not root 0444"
        else
            skip "site identity marker metadata"
        fi
    else
        drift "site identity marker is missing: $SITE_MARKER_PATH"
    fi

    for binary in $GO_BINARIES; do
        installed_path=$(go_install_path "$binary")
        check_exec "$installed_path" "control-plane binary"
        if [ -e "$INSTALL_OLD_GO/$binary" ] && [ ! -L "$INSTALL_OLD_GO/$binary" ]; then
            drift "legacy binary is not a compatibility symlink: $INSTALL_OLD_GO/$binary"
        else
            check_symlink "$INSTALL_OLD_GO/$binary" "$installed_path" "legacy binary compatibility link"
        fi
        check_symlink "$INSTALL_INTERIM_GO/$binary" "$installed_path" "interim binary compatibility link"
    done
    # chasm-clipboard is an optional plugin helper (docs/plugins.md). Core
    # does not build, install, or require it; a helper installed by an
    # earlier core release is left in place and keeps working through the
    # explicit `chasm-clipboard -- chasm shell NAME` invocation.
    if resolved_incus_socket=$(incus_socket_probe 2>/dev/null); then
        INCUS_SOCKET=$resolved_incus_socket
        export INCUS_SOCKET
        ok "Incus local socket resolved: $resolved_incus_socket"
    else
        drift "Incus local socket is unavailable or configured path is invalid"
    fi
    if [ -f /etc/chasm/incus-socket ]; then
        [ "$(stat -c '%U:%G:%a' /etc/chasm/incus-socket 2>/dev/null || true)" = root:root:600 ] ||
            drift "Incus socket setting metadata is not root:root 0600"
    else
        ok "Incus socket setting uses standard path discovery"
    fi
    if selinux_enforcing; then
        for binary in $GO_BINARIES; do
            installed_path=$(go_install_path "$binary")
            if selinux_bin_label "$installed_path"; then
                ok "SELinux executable label bin_t: $installed_path"
            else
                drift "SELinux executable label is not bin_t: $installed_path (fix: restorecon -v $installed_path)"
            fi
        done
    else
        skip "SELinux executable labels (SELinux not enforcing or unavailable)"
    fi

    check_file "$INSTALL_PROVISION/scripts/lib/require-site-host.sh" "provision tree (scripts)"
    if [ -d "$INSTALL_PROVISION/guest-config/profiles" ] && \
        [ -n "$(ls "$INSTALL_PROVISION/guest-config/profiles" 2>/dev/null)" ]; then
        ok "provision tree (guest-config) present"
    else
        drift "provision tree (guest-config) missing"
    fi
    check_file "$INSTALL_PROVISION/host-config/incus-preseed.template.yaml" "provision tree (host-config)"
    check_file "$INSTALL_PROVISION/host-control/chasm-executor.service.template" "provision tree (host-control)"
    check_file "$INSTALL_SITE_HOST_LIB" "firewall site-identity library"
    check_exec "$INSTALL_FIREWALL_HELPER" "firewall helper"
    check_file "$INSTALL_FIREWALL_TEMPLATE" "firewall ruleset template"

    for unit in $UNITS; do
        check_file "/etc/systemd/system/$unit" "systemd unit"
    done
    for unit in chasm-executor.service chasm-supervisor.service; do
        server=${unit#chasm-}
        server=${server%.service}_server
        if [ -f "/etc/systemd/system/$unit" ] &&
            check_service_executable "/etc/systemd/system/$unit" "$(go_install_path "$server")"; then
            ok "systemd executable path migrated: $unit"
        else
            drift "systemd unit has an old or invalid executable path: $unit (expected $(go_install_path "$server"))"
        fi
    done
    check_file /etc/sudoers.d/chasm "sudoers fragment"
    if [ -f /etc/sudoers.d/chasm ] && [ ! -L /etc/sudoers.d/chasm ] && am_root; then
        if grep -Fqx "$SITE_OPERATOR_USER ALL=(root) NOPASSWD: /usr/local/bin/chasm __shell *" /etc/sudoers.d/chasm &&
            grep -Fqx "$SITE_OPERATOR_USER ALL=(root) NOPASSWD: NOSETENV: /usr/local/bin/chasm __exec" /etc/sudoers.d/chasm &&
            grep -Fqx "$SITE_OPERATOR_USER ALL=(root) NOPASSWD: NOSETENV: /usr/local/bin/chasm __doctor" /etc/sudoers.d/chasm &&
            ! grep -Eq 'NOLOG_INPUT|NOLOG_OUTPUT' /etc/sudoers.d/chasm; then
            ok "sudoers entry paths and portable tags verified"
        else
            drift "sudoers entry paths or tags differ from the installed CLI: /etc/sudoers.d/chasm"
        fi
    elif ! am_root; then
        skip "sudoers entry content"
    fi
    check_file /usr/lib/sysusers.d/chasm.conf "sysusers fragment"
    check_file /usr/lib/tmpfiles.d/chasm.conf "tmpfiles fragment"

    if command -v systemctl >/dev/null 2>&1; then
        for socket in chasm-executor.socket chasm-supervisor.socket; do
            state=$(systemctl is-active "$socket" 2>/dev/null || true)
            if [ "$state" = active ]; then
                ok "socket active: $socket"
            else
                drift "socket is not active (state=$state): $socket"
            fi
        done
        enabled=$(systemctl is-enabled chasm-firewall.service 2>/dev/null || true)
        if [ "$enabled" = enabled ]; then
            ok "firewall service enabled at boot"
        else
            drift "firewall service is not enabled at boot (state=$enabled)"
        fi
        state=$(systemctl is-active chasm-firewall.service 2>/dev/null || true)
        if [ "$state" = active ]; then
            ok "firewall service active"
        else
            drift "firewall service is not active (state=$state)"
        fi
    else
        drift "systemctl is unavailable"
    fi

    if ! am_root; then
        skip "Incus and nftables probes"
        return 0
    fi
    if [ "$DESCRIPTOR_OK" -ne 1 ]; then
        drift "Incus probes skipped: the descriptor did not validate"
        return 0
    fi
    if ! command -v incus >/dev/null 2>&1; then
        drift "incus binary is missing"
        return 0
    fi
    if incus storage show "$SITE_STORAGE_POOL" >/dev/null 2>&1; then
        ok "incus storage pool present: $SITE_STORAGE_POOL"
    else
        drift "incus storage pool missing: $SITE_STORAGE_POOL"
    fi
    if incus network show "$SITE_BRIDGE_NAME" >/dev/null 2>&1; then
        ok "incus network present: $SITE_BRIDGE_NAME"
        if actual_bridge_ipv4=$(incus network get "$SITE_BRIDGE_NAME" ipv4.address 2>/dev/null) &&
            [ "$actual_bridge_ipv4" = "$SITE_BRIDGE_IPV4" ]; then
            ok "incus bridge IPv4 matches the descriptor: $SITE_BRIDGE_IPV4"
        else
            drift "incus bridge IPv4 differs or is unreadable: $SITE_BRIDGE_NAME (expected $SITE_BRIDGE_IPV4, found ${actual_bridge_ipv4:-unavailable})"
        fi
        if actual_bridge_ipv6=$(incus network get "$SITE_BRIDGE_NAME" ipv6.address 2>/dev/null) &&
            [ "$actual_bridge_ipv6" = none ]; then
            ok "incus bridge IPv6 addressing is disabled"
        else
            drift "incus bridge IPv6 differs or is unreadable: $SITE_BRIDGE_NAME (expected none, found ${actual_bridge_ipv6:-unavailable})"
        fi
    else
        drift "incus network missing: $SITE_BRIDGE_NAME"
    fi
    if incus profile show "$SITE_PROFILE_NAME" >/dev/null 2>&1; then
        ok "incus profile present: $SITE_PROFILE_NAME"
    else
        drift "incus profile missing: $SITE_PROFILE_NAME"
    fi
    template_instance=$(template_instance_for_profile "$SITE_BOOTSTRAP_PROFILE")
    if [ -n "$template_instance" ] && template_sealed "$template_instance"; then
        # Staleness is identity-aware: the identity digests the builder and
        # every layer script, so a tree change makes the sealed template
        # outdated while its name still matches.
        if ! am_root; then
            ok "sealed template present for profile $SITE_BOOTSTRAP_PROFILE"
        elif "$INSTALL_PROVISION/scripts/build-sandbox-template.sh" --check >/dev/null 2>&1; then
            ok "sealed template present for profile $SITE_BOOTSTRAP_PROFILE"
        else
            drift "sealed template for profile $SITE_BOOTSTRAP_PROFILE is stale (its identity no longer matches the tree); re-run --provision to rebuild"
        fi
    elif [ -n "$template_instance" ]; then
        drift "template instance present but not sealed: $template_instance"
    else
        drift "sealed template missing for profile $SITE_BOOTSTRAP_PROFILE"
    fi
    if command -v nft >/dev/null 2>&1; then
        if nft list table inet chasm >/dev/null 2>&1; then
            ok "nftables table inet chasm present"
            for chain in input forward; do
                if firewall_chain_verified "$chain"; then
                    ok "nftables IPv6 drop is present in inet chasm $chain"
                else
                    drift "nftables leading IPv6 drop or hook differs or is unreadable in inet chasm $chain; re-run --provision"
                fi
            done
        else
            drift "nftables table inet chasm missing"
        fi
    else
        drift "nft binary is missing"
    fi
    # A stale installed template (provisioned before a token existed) renders
    # silently wrong rulesets: the leftover-token guard only fires when the
    # template HAS tokens, so probe for the expected ones instead.
    if am_root; then
        if grep -q '@BRIDGE_NAME@' "$INSTALL_FIREWALL_TEMPLATE" 2>/dev/null; then
            ok "firewall ruleset template carries the expected tokens"
        else
            drift "firewall ruleset template is stale (missing @BRIDGE_NAME@); re-run --provision to refresh it"
        fi
    fi
    if am_root; then
        # Foreign drop-ins that win the first-value rule are drift only
        # while a chasm fragment is installed to defeat; with the opt-in
        # hardening not applied the library reports them through ok as a
        # note instead, so they never fail --provision/--verify alone.
        # The hardening step is an optional plugin (docs/plugins.md): when
        # its library is not present core only reports that the fragments
        # were not examined and never consults the plugin.
        if [ -f "$SSH_HARDENING_PLUGIN" ] && [ ! -L "$SSH_HARDENING_PLUGIN" ] && \
            [ -d "$SSH_HARDENING_DROPINS" ] && [ ! -L "$SSH_HARDENING_DROPINS" ]; then
            SSH_DROPINS_DIR=$SSH_HARDENING_DROPINS
            # shellcheck source=plugins/ssh-hardening/ssh-hardening.sh
            . "$SSH_HARDENING_PLUGIN"
            ssh_hardening_report ok drift
        else
            skip "sshd hardening fragments (ssh-hardening plugin not present)"
        fi
    else
        skip "sshd hardening fragments"
    fi
}

# --- preflight ---------------------------------------------------------------

PREFLIGHT_FAILURES=0
INCUS_INSTALL_NEEDED=0

p_fail() { echo "FAIL  $*"; PREFLIGHT_FAILURES=$((PREFLIGHT_FAILURES + 1)); }
p_note() { echo "note  $*"; }
p_ok()   { echo "ok    $*"; }

# incus_preflight_check: the preflight verdict at the Incus acquisition
# boundary. A present Incus is a functional-prerequisites check only: no
# acquisition ever runs for it (INCUS_INSTALL_NEEDED stays 0), so a
# missing virtiofsd stays a hard failure here. An absent Incus fails the
# preflight closed with the manual guidance unless the documented
# acquisition path can actually run for it: --install-incus was given and
# the root-owned install-incus plugin is present (the same file/symlink
# gate install_incus_step applies); then the decision defers to that step
# after preflight, where the typed release and approval checks remain.
# VIRTIOFSD_LIBEXEC defaults to /usr/libexec/virtiofsd; the test harness
# overrides it, and an explicitly set value is also honoured in production.
virtiofsd_probe() {
    virtiofsd_path=$(command -v virtiofsd || true)
    [ -z "$virtiofsd_path" ] || return 0
    [ "$#" -gt 0 ] || set -- "${VIRTIOFSD_LIBEXEC:-/usr/libexec/virtiofsd}" /usr/lib/virtiofsd /usr/lib/qemu/virtiofsd
    for virtiofsd_candidate do
        if [ -f "$virtiofsd_candidate" ] && [ -x "$virtiofsd_candidate" ]; then
            virtiofsd_path=$virtiofsd_candidate
            return 0
        fi
    done
    return 1
}

incus_socket_probe() {
	if [ "${INCUS_SOCKET_EXPLICIT:-0}" -eq 1 ] && [ -n "${INCUS_SOCKET:-}" ]; then
		incus_socket_candidate=$INCUS_SOCKET
	elif [ "${install_mode:-}" != --provision ] && { [ -e /etc/chasm/incus-socket ] || [ -L /etc/chasm/incus-socket ]; }; then
		[ -f /etc/chasm/incus-socket ] && [ ! -L /etc/chasm/incus-socket ] &&
			[ "$(stat -c '%U:%G:%a' /etc/chasm/incus-socket 2>/dev/null || true)" = root:root:600 ] &&
			[ "$(wc -l </etc/chasm/incus-socket)" -eq 1 ] &&
			[ "$(wc -c </etc/chasm/incus-socket)" -le 4096 ] || return 1
		incus_socket_candidate=$(cat /etc/chasm/incus-socket) || return 1
	else
		incus_socket_candidate=
		for candidate in /var/lib/incus/unix.socket /run/incus/unix.socket; do
			if [ -S "$candidate" ]; then incus_socket_candidate=$candidate; break; fi
		done
	fi
	case "$incus_socket_candidate" in /*) ;; *) return 1 ;; esac
	case "$incus_socket_candidate" in *[!A-Za-z0-9_./-]*|'') return 1 ;; esac
	case "$incus_socket_candidate" in *//*|*/./*|*/../*|*/.|*/..|*/) return 1 ;; esac
	[ "${#incus_socket_candidate}" -le 107 ] && [ ! -L "$incus_socket_candidate" ] && [ -S "$incus_socket_candidate" ] || return 1
	incus_socket_parent=${incus_socket_candidate%/*}
	[ -n "$incus_socket_parent" ] || incus_socket_parent=/
	[ "$(stat -c %u "$incus_socket_candidate" 2>/dev/null)" = 0 ] || return 1
	while :; do
		[ -d "$incus_socket_parent" ] && [ ! -L "$incus_socket_parent" ] || return 1
		incus_socket_meta=$(stat -c '%u:%a' "$incus_socket_parent" 2>/dev/null) || return 1
		case "$incus_socket_meta" in 0:*) ;; *) return 1 ;; esac
		incus_socket_mode=${incus_socket_meta#*:}
		[ "$((0$incus_socket_mode & 022))" -eq 0 ] || return 1
		[ "$incus_socket_parent" = / ] && break
		incus_socket_parent=${incus_socket_parent%/*}
		[ -n "$incus_socket_parent" ] || incus_socket_parent=/
	done
	printf '%s\n' "$incus_socket_candidate"
}

incus_agent_probe() {
    incus_agent_path=$(command -v incus-agent || true)
    [ -z "$incus_agent_path" ] || return 0
    if [ "$#" -eq 0 ]; then
        agent_arch=$(uname -m 2>/dev/null || true)
        case "$agent_arch" in
            x86_64) ;;
            aarch64) ;;
            *) agent_arch= ;;
        esac
        set -- /usr/libexec/incus/incus-agent /usr/lib/incus/incus-agent /usr/libexec/incus-agent
        if [ -n "${INCUS_AGENT_PATH:-}" ]; then
            set -- "$@" "$INCUS_AGENT_PATH/incus-agent"
            [ -z "$agent_arch" ] || set -- "$@" "$INCUS_AGENT_PATH/incus-agent.linux.$agent_arch"
        fi
        if [ -n "$agent_arch" ]; then
            set -- "$@" \
                "/usr/lib/incus/agents/incus-agent.linux.$agent_arch" \
                "/usr/libexec/incus/agents/incus-agent.linux.$agent_arch" \
                "/usr/share/incus/agents/incus-agent.linux.$agent_arch"
        fi
    fi
    for agent_candidate do
        if [ -f "$agent_candidate" ] && [ -x "$agent_candidate" ]; then
            incus_agent_path=$agent_candidate
            return 0
        fi
    done
    return 1
}

go_version_at_least() {
	actual=$1
	minimum=$2
	case "$actual" in go*) actual=${actual#go} ;; esac
	awk -v actual="$actual" -v minimum="$minimum" 'BEGIN {
	  sub(/[^0-9.].*$/, "", actual);
	  na=split(actual,a,"."); nm=split(minimum,m,".");
	  if (na<2 || na>3 || nm<2 || nm>3) exit 1;
	  for (i=1;i<=na;i++) if (a[i] !~ /^[0-9]+$/) exit 1;
	  for (i=1;i<=nm;i++) if (m[i] !~ /^[0-9]+$/) exit 1;
	  for (i=1;i<=3;i++) { av=(i<=na ? a[i]+0 : 0); mv=(i<=nm ? m[i]+0 : 0); if (av>mv) exit 0; if (av<mv) exit 1; }
	  exit 0
	}'
}

incus_preflight_check() {
    if command -v incus >/dev/null 2>&1; then
        p_ok "incus present: $(command -v incus)"
        # Incus exposes VM disk shares (the sandbox /shared and /workspace
        # volumes) through vhost-user virtiofs only; without the virtiofsd
        # daemon the shares silently never reach the guest and every sandbox
        # create fails at the volume checks. Debian only packages it from
        # trixie; on older releases it must be built from the upstream crate
        # (cargo install virtiofsd) and installed to /usr/libexec/virtiofsd.
        if virtiofsd_probe; then
            p_ok "virtiofsd present: $virtiofsd_path"
        else
            p_fail "virtiofsd is missing (Incus VM disk shares need it; install the distro package or: cargo install virtiofsd --locked --version 1.13.2 && install it to /usr/libexec/virtiofsd)"
        fi
        if incus_agent_probe; then
            p_ok "Incus host agent present: $incus_agent_path"
        else
            agent_remedy="Incus host agent is missing (install the incus-agent package from your Incus distribution; template VM readiness depends on it)"
            template_instance=$(template_instance_for_profile "$SITE_BOOTSTRAP_PROFILE") || true
            if [ -n "$template_instance" ] && template_sealed "$template_instance"; then
                p_note "WARNING: $agent_remedy; existing sealed template is available"
            else
                p_fail "$agent_remedy"
            fi
        fi
    else
        INCUS_INSTALL_NEEDED=1
        if [ "$INSTALL_INCUS" -eq 1 ] && [ -f "$INCUS_INSTALL_PLUGIN" ] && [ ! -L "$INCUS_INSTALL_PLUGIN" ]; then
            p_note "incus is not installed; deferred to the install-incus acquisition step (--install-incus; Debian 12/13 only: trixie and newer from the release's own repositories, bookworm from bookworm-backports)"
        else
            p_fail "incus is not installed. Install incus and virtiofsd yourself, or re-run --provision with --install-incus to use the install-incus plugin (Debian 12/13 only; trixie and newer from the release's own repositories, bookworm from bookworm-backports)"
        fi
    fi
}

check_host_distribution() {
    debian_family=0
    if [ -r "$1" ]; then
        if awk -F= '/^ID(_LIKE)?=/ {
            gsub(/"/, "", $2)
            n = split($2, words, /[[:space:]]+/)
            for (i = 1; i <= n; i++) if (words[i] == "debian") found = 1
        }
        END { exit !found }' "$1"; then
            debian_family=1
        fi
    fi
    if [ "$debian_family" -eq 1 ]; then
        p_ok "Debian-family host"
    elif [ "$ALLOW_UNTESTED_HOST" -eq 1 ] || [ "${CHASM_ALLOW_UNTESTED_HOST:-0}" = 1 ]; then
        p_note "WARNING: continuing on an untested distro by explicit operator opt-in; capability checks do not qualify this distro"
    else
        p_fail "host is not Debian-family; this distro is untested. Re-run --provision with --allow-untested-host or CHASM_ALLOW_UNTESTED_HOST=1 after reviewing the qualification risk"
    fi
}

selinux_enforcing() {
    command -v getenforce >/dev/null 2>&1 && [ "$(getenforce 2>/dev/null || true)" = Enforcing ]
}

selinux_bin_label() {
    ls -Z "$1" 2>/dev/null | awk '{print $1}' | grep -q ':bin_t:'
}

check_service_executable() {
    awk -v executable="$2" '
        /^ExecStart=/ {
            starts++;
            if ($1 == "ExecStart=" executable) valid++;
        }
        END { exit !(starts == 1 && valid == 1) }
    ' "$1"
}

install_go_payload() {
    # Publish compatibility links only after every executable has
    # been installed and checked under the host label policy.
    for binary in $GO_BINARIES; do
        installed_path=$(go_install_path "$binary")
        install -o root -g root -m 0755 "$1/$binary" "$installed_path" ||
            fail "could not install $installed_path"
    done
    if selinux_enforcing; then
        command -v restorecon >/dev/null 2>&1 || fail "SELinux is enforcing but restorecon is unavailable"
        for binary in $GO_BINARIES; do
            installed_path=$(go_install_path "$binary")
            restorecon -F "$installed_path" || fail "could not restore the SELinux label: $installed_path"
            selinux_bin_label "$installed_path" ||
                fail "SELinux label is not bin_t after restorecon: $installed_path"
        done
    fi
    for binary in $GO_BINARIES; do
        installed_path=$(go_install_path "$binary")
        ln -sfn "$installed_path" "$INSTALL_OLD_GO/$binary"
        ln -sfn "$installed_path" "$INSTALL_INTERIM_GO/$binary"
    done
}

preflight() {
    echo "install: preflight"

    check_host_distribution /etc/os-release

    if command -v incus >/dev/null 2>&1; then
        if resolved_incus_socket=$(incus_socket_probe); then
            INCUS_SOCKET=$resolved_incus_socket
            export INCUS_SOCKET
            p_ok "Incus local socket: $resolved_incus_socket"
        else
            p_fail "Incus local Unix socket is unavailable or invalid (set INCUS_SOCKET to an absolute existing socket)"
        fi
    fi

    if [ -d /run/systemd/system ]; then
        p_ok "systemd is running"
    else
        p_fail "systemd is not running"
    fi

    command -v nft >/dev/null 2>&1 && p_ok "nftables present" ||
        p_fail "nft is not installed (apt install nftables)"
    command -v ip >/dev/null 2>&1 && p_ok "iproute2 present" ||
        p_fail "ip is not installed (apt install iproute2)"

    # KVM: the device, a loaded module, and nested virtualization off. The
    # admission rule and module list are shared with initialize-incus.sh
    # through the site library.
    if [ -c /dev/kvm ]; then
        p_ok "/dev/kvm present"
    else
        p_fail "/dev/kvm is missing"
    fi
    kvm_found=0
    for kvm_candidate in $(site_nested_kvm_modules); do
        kvm_parameter=/sys/module/$kvm_candidate/parameters/nested
        [ -r "$kvm_parameter" ] || continue
        kvm_found=1
        kvm_nested=$(cat "$kvm_parameter")
        if site_nested_kvm_disabled "$kvm_nested"; then
            p_ok "KVM module $kvm_candidate with nested=$kvm_nested"
        else
            case "$kvm_candidate" in
                kvm_intel) kvm_no_nested_conf=kvm-intel-no-nesting.conf ;;
                kvm_amd) kvm_no_nested_conf=kvm-amd-no-nesting.conf ;;
                *) kvm_no_nested_conf="kvm-<vendor>-no-nesting.conf" ;;
            esac
            p_fail "nested KVM is still enabled ($kvm_candidate nested=$kvm_nested); fix: install host-config/modprobe.d/$kvm_no_nested_conf as /etc/modprobe.d/$kvm_no_nested_conf, then reload the kvm module (modprobe -r $kvm_candidate && modprobe $kvm_candidate) or reboot the host"
        fi
    done
    [ "$kvm_found" -eq 1 ] || p_fail "neither kvm_intel nor kvm_amd is loaded"

    # Incus presence: absence is not a preflight failure by itself when the
    # explicit opt-in acquisition path can run; see incus_preflight_check.
    incus_preflight_check

    # Docker detection is informational: the firewall rules live in their own
    # inet chasm table and never read or modify Docker's rules; the
    # Docker/nftables coexistence smoke remains a hard bring-up gate.
    if command -v docker >/dev/null 2>&1 || systemctl is-active --quiet docker 2>/dev/null; then
        p_note "Docker detected: coexistence is by design (separate nftables table); the coexistence smoke still gates bring-up"
    else
        p_ok "no Docker detected"
    fi

    # Disk headroom: the full pool size plus a fixed margin when the pool
    # does not exist yet; on a site whose loop pool already exists (the
    # normal re-run, with sandboxes possibly running) only the margin plus
    # the room the pool's loop file can still grow into. The verdict names
    # which rule applied and its numbers.
    headroom_dir=/var/lib/incus
    [ -d "$headroom_dir" ] || headroom_dir=/var/lib
    disk_verdict=$(disk_headroom_verdict \
        "$SITE_STORAGE_POOL" "$SITE_POOL_SIZE_GIB" "$POOL_HEADROOM_GIB" "$headroom_dir") &&
        p_ok "$disk_verdict" || p_fail "$disk_verdict"

    # Pool growth plan (idiom 3): the supported path for a pool change is
    # editing pool_size_gib and rerunning --provision. A shrink is never
    # supported and must stop provision before any other change, so the plan
    # is checked here in preflight (the grow itself is applied later, in
    # initialize_incus, where approval and Incus are available). Best-effort
    # on a fresh host where incus is not installed yet: the plan runs only
    # when the pool already exists.
    if command -v incus >/dev/null 2>&1 && \
        incus storage show "$SITE_STORAGE_POOL" >/dev/null 2>&1; then
        pool_plan=$(pool_resize_plan "$SITE_STORAGE_POOL" "$SITE_POOL_SIZE_GIB") && pool_plan_status=0 || pool_plan_status=$?
        case "$pool_plan_status" in
            0) p_ok "$pool_plan" ;;
            2) p_ok "$pool_plan" ;;
            3) p_fail "$pool_plan" ;;
            *) p_fail "storage pool resize plan failed: $pool_plan" ;;
        esac
    else
        new_root_disk_gib=${SITE_PROFILE_ROOT_DISK_GIB:-60}
        if [ "$new_root_disk_gib" -gt "$SITE_POOL_SIZE_GIB" ]; then
            p_fail "profile root disk needs ${new_root_disk_gib}GiB but pool_size_gib is $SITE_POOL_SIZE_GIB; set profile_root_disk_gib to fit or increase the pool size"
        fi
    fi

    # Memory: report the admission suggestion; the descriptor value is the
    # operator's decision, so a smaller value only warns.
    total_mib=$(awk '/^MemTotal:/ {print int($2 / 1024)}' /proc/meminfo 2>/dev/null || true)
    case "$total_mib" in
        ''|*[!0-9]*)
            p_note "host memory could not be read from /proc/meminfo; the descriptor's host_reserve_mib is taken as-is"
            ;;
        *)
            suggested_mib=$(awk -v t="$total_mib" 'BEGIN {
                s = int(t / 16)
                s = int((s + 127) / 128) * 128
                if (s < 1024) s = 1024
                print s
            }')
            if [ "$SITE_HOST_RESERVE_MIB" -lt "$suggested_mib" ]; then
                p_note "host_reserve_mib=$SITE_HOST_RESERVE_MIB is below the suggestion for ${total_mib}MiB RAM (suggested $suggested_mib); override deliberately in the descriptor"
            else
                p_ok "host_reserve_mib=$SITE_HOST_RESERVE_MIB is at or above the suggestion ($suggested_mib) for ${total_mib}MiB RAM"
            fi
            ;;
    esac

    # Bridge-prefix collision: no main IPv4 route outside the site's bridge
    # may overlap the sandbox /24, or guests would be black-holed against
    # the real LAN. On a re-run the site's own bridge route — bridge_name
    # exists as an Incus-managed bridge carrying the descriptor's
    # bridge_ipv4 — is accepted as the existing site bridge. The default
    # route is ignored, and overlapping more-specific routes and supernets
    # both cause provisioning to refuse.
    route_verdict=$(prefix_route_verdict \
        "$SITE_BRIDGE_NAME" "$SITE_BRIDGE_IPV4" "$SITE_SANDBOX_IPV4_PREFIX") &&
        p_ok "$route_verdict" || p_fail "$route_verdict"

    # The operator must exist before the units that reference it are rendered.
    if operator_passwd=$(getent passwd "$SITE_OPERATOR_USER"); then
        operator_uid=$(printf '%s\n' "$operator_passwd" | cut -d: -f3)
        case "$operator_uid" in
            ''|*[!0-9]*) p_fail "operator user has no valid numeric UID: $SITE_OPERATOR_USER" ;;
            *)
                if [ "$operator_uid" -gt 0 ]; then
                    p_ok "operator user exists: $SITE_OPERATOR_USER (UID $operator_uid)"
                else
                    p_fail "operator user must have a nonzero UID: $SITE_OPERATOR_USER"
                fi
                ;;
        esac
    else
        p_fail "operator user does not exist: $SITE_OPERATOR_USER"
    fi

    # The binaries are built from this tree, so a Go toolchain is required.
    if command -v go >/dev/null 2>&1; then
        go_minimum=$(awk '$1 == "go" { print $2; exit }' "$PROVISION_ROOT/go.mod")
        go_actual=$(go version 2>/dev/null | awk '{ print $3 }')
        if [ -n "$go_minimum" ] && go_version_at_least "$go_actual" "$go_minimum"; then
            p_ok "Go toolchain present: $(command -v go) ($go_actual; minimum $go_minimum)"
        else
            p_fail "Go $go_minimum or newer is required by go.mod (found ${go_actual:-unknown}); install a newer Go toolchain and ensure it precedes the distro default in PATH"
        fi
    else
        p_fail "no Go toolchain in PATH (the installer builds the control plane from this tree)"
    fi

    [ "$PREFLIGHT_FAILURES" -eq 0 ] ||
        fail "preflight found $PREFLIGHT_FAILURES blocking condition(s)"
}

# --- provision steps ---------------------------------------------------------

# load_site_lib re-sources the site library so SITE_CONF_PATH (fixed at source
# time) follows CHASM_SITE_CONF as it stands right now.
load_site_lib() {
    . "$SCRIPT_DIR/lib/require-site-host.sh"
}

# load_candidate_descriptor CONF_PATH: parse CONF_PATH into the parent shell's
# SITE_* variables so preflight sees real values (the library fixes
# SITE_CONF_PATH when it is sourced, so a per-call env prefix would not work).
load_candidate_descriptor() {
    CHASM_SITE_CONF=$1
    export CHASM_SITE_CONF
    load_site_lib
    unset CHASM_SITE_CONF
    site_require_descriptor
}

write_descriptor() {
    if [ -e "$INSTALLED_SITE_CONF" ]; then
        echo "install: descriptor already present at $INSTALLED_SITE_CONF; keeping it"
    else
        approve "write the site descriptor $INSTALLED_SITE_CONF from host-config/site.conf.example"
        install -d -o root -g root -m 0755 /etc/chasm
        install -o root -g root -m 0644 \
            "$PROVISION_ROOT/host-config/site.conf.example" "$INSTALLED_SITE_CONF"
        echo "install: wrote $INSTALLED_SITE_CONF (root:root 0644)"
    fi
    # The installed descriptor is the live one from here on. site_name and
    # operator_user are written out explicitly: headless systemd services have
    # no SUDO_USER/USER/LOGNAME to resolve the empty-value defaults from.
    # The values come from the caller's parse — the resume path parsed this
    # exact file, and on the fresh path the candidate was byte-identical to
    # the example just installed — so there is no pre-sed re-parse; the
    # mandatory fail-closed check is the one after the sed, on the resulting
    # bytes.
    # Escape the replacement text for sed before interpolating it: both
    # readers accept '&' and '|' inside plain-token values, so an unescaped
    # value would corrupt the descriptor or abort the sed. This mirrors the
    # escape-first idiom of render-incus-preseed.sh and render-host-config.sh.
    site_name_escaped=$(printf '%s\n' "$SITE_SITE_NAME" | sed 's/[&|\\]/\\&/g')
    operator_user_escaped=$(printf '%s\n' "$SITE_OPERATOR_USER" | sed 's/[&|\\]/\\&/g')
    sed -i -e "s|^site_name=.*$|site_name=$site_name_escaped|" \
           -e "s|^operator_user=.*$|operator_user=$operator_user_escaped|" \
        "$INSTALLED_SITE_CONF"
    load_site_lib
    site_require_descriptor ||
        fail "the installed descriptor does not parse after writing the resolved site_name and operator_user"
}

pin_identity() {
    if [ -e "$SITE_MARKER_PATH" ]; then
        echo "install: site identity already pinned at $SITE_MARKER_PATH"
        require_site_host ||
            fail "the pinned identity does not match this host; re-pinning is a deliberate provisioning action"
        return 0
    fi
    approve "pin the site identity marker $SITE_MARKER_PATH (create-once, root:root 0444)"
    "$SCRIPT_DIR/lib/require-site-host.sh" --provision
}

install_payload() {
    approve "build the control-plane binaries and install the payload (binaries, provision tree, firewall helper, systemd units, sockets)"

    staging=$(mktemp -d)
    cleanup_staging() { rm -rf "$staging"; }
    trap cleanup_staging EXIT
    trap 'exit 129' HUP
    trap 'exit 130' INT
    trap 'exit 143' TERM

    # Minimal service environments may supply neither a home nor a cache.
    # Keep the fallback private and remove it with the build staging files.
    (cd "$PROVISION_ROOT" &&
        if [ -z "${GOCACHE:-}${XDG_CACHE_HOME:-}${HOME:-}" ]; then
            GOCACHE=$staging/go-cache
            export GOCACHE
        fi
        go build -o "$staging" \
        ./cmd/chasm ./cmd/chasmctl ./cmd/executor_server ./cmd/supervisor_server) ||
        fail "building the Go control plane failed"
    for binary in $GO_BINARIES; do
        [ -f "$staging/$binary" ] || fail "the build did not produce $binary"
    done

    install -d -o root -g root -m 0755 "$INSTALL_LIB" "$INSTALL_GO" "$INSTALL_BIN" "$INSTALL_OLD_GO" "$INSTALL_INTERIM_GO" "$INSTALL_PROVISION" /etc/chasm
    if [ "$INCUS_SOCKET_EXPLICIT" -eq 1 ]; then
        incus_socket_probe >/dev/null || fail "INCUS_SOCKET must be an absolute path to an existing local Unix socket"
        printf '%s\n' "$INCUS_SOCKET" >"$staging/incus-socket"
        install -o root -g root -m 0600 "$staging/incus-socket" /etc/chasm/incus-socket
    else
        rm -f /etc/chasm/incus-socket
    fi
    install_go_payload "$staging"

    # The staging directory has served its only purpose (the build and the
    # binary installs above); remove it and disarm this step's cleanup trap
    # NOW, while the step still owns it. The ssh hardening step that runs
    # later arms its own traps and clears all four when it ends, so any trap
    # still armed here would be silently dropped — and with it the staging
    # cleanup.
    rm -rf "$staging"
    trap - EXIT HUP INT TERM

    # The provision tree is installed with explicit modes and ownership:
    # a release candidate packaged with git archive carries 0664 files
    # (the tarball applies the packaging umask to every member), and the
    # profile reader refuses a group-writable manifest, which would abort
    # the template build. The library normalizes the trees regardless of
    # the source modes and the invoking umask.
    provision_tree_install "$PROVISION_ROOT" "$INSTALL_PROVISION" ||
        fail "installing the provision tree failed"

    # Core profile manifests: install core's own reserved names into the
    # production profile root, but only when absent, and never prune or
    # rewrite anything else there (docs/plugins.md). An upgrade over an
    # existing core manifest with different bytes keeps the installed one —
    # the live site's templates were sealed from it — and says so; fresh
    # installs get the current core manifest.
    install -d -o root -g root -m 0755 /etc/chasm/profiles
    for core_profile in $CORE_PROFILE_NAMES; do
        core_profile_src=$PROVISION_ROOT/guest-config/profiles/$core_profile.profile
        core_profile_dst=/etc/chasm/profiles/$core_profile.profile
        [ -f "$core_profile_src" ] && [ ! -L "$core_profile_src" ] ||
            fail "core profile manifest is missing or unsafe: $core_profile_src"
        if [ ! -e "$core_profile_dst" ]; then
            install -o root -g root -m 0644 "$core_profile_src" "$core_profile_dst"
        elif ! cmp -s "$core_profile_src" "$core_profile_dst"; then
            echo "install: keeping the installed $core_profile_dst (differs from the core manifest shipped in this tree; fresh semantics apply only to new installs)"
        fi
    done

    install -o root -g root -m 0644 \
        "$SCRIPT_DIR/lib/require-site-host.sh" "$INSTALL_SITE_HOST_LIB"
    install -o root -g root -m 0644 \
        "$PROVISION_ROOT/host-config/firewall.nft.template" "$INSTALL_FIREWALL_TEMPLATE"
    install -o root -g root -m 0755 \
        "$PROVISION_ROOT/host-control/chasm-firewall" "$INSTALL_FIREWALL_HELPER"

    "$SCRIPT_DIR/install-control-plane.sh"
}

# The sshd hardening and Incus acquisition steps are optional plugins
# (docs/plugins.md). The Incus acquisition dispatcher sources its plugin
# only with --install-incus. The hardening dispatcher reads and sources the
# ssh-hardening plugin when present even without --harden-ssh, but applies
# hardening only with that flag. With neither flag, these dispatchers do not
# change sshd or install packages. The hardening plugin's approval goes
# through approve_hardening:
# hardening can lock the operator out, so it is the one step
# CHASM_ASSUME_YES does not pre-approve.

# install_ssh_hardening_step: the --provision sshd step dispatcher.
install_ssh_hardening_step() {
    if [ "$HARDEN_SSH" -ne 1 ]; then
        if [ -f "$SSH_HARDENING_PLUGIN" ] && [ ! -L "$SSH_HARDENING_PLUGIN" ] && \
            [ -d "$SSH_HARDENING_DROPINS" ] && [ ! -L "$SSH_HARDENING_DROPINS" ]; then
            SSH_DROPINS_DIR=$SSH_HARDENING_DROPINS
            # shellcheck source=plugins/ssh-hardening/ssh-hardening.sh
            . "$SSH_HARDENING_PLUGIN"
            install_ssh_hardening
        else
            echo "install: ssh hardening: skipped (pass --harden-ssh to apply; requires the plugins/ssh-hardening add-on)"
        fi
        return 0
    fi
    if [ ! -f "$SSH_HARDENING_PLUGIN" ] || [ -L "$SSH_HARDENING_PLUGIN" ] || \
        [ ! -d "$SSH_HARDENING_DROPINS" ] || [ -L "$SSH_HARDENING_DROPINS" ]; then
        fail "--harden-ssh requires the ssh-hardening plugin, which is not present in this tree: plugins/ssh-hardening/{ssh-hardening.sh,drop-ins/} (see docs/plugins.md)"
    fi
    # Used by the sourced ssh-hardening plugin.
    # shellcheck disable=SC2034
    SSH_DROPINS_DIR=$SSH_HARDENING_DROPINS
    # shellcheck source=plugins/ssh-hardening/ssh-hardening.sh
    . "$SSH_HARDENING_PLUGIN"
    install_ssh_hardening
}

# install_incus_step: the --provision Incus acquisition dispatcher. Without
# --install-incus core installs no packages: a missing Incus is a clear
# failure pointing at the plugin or the manual path.
install_incus_step() {
    [ "$INCUS_INSTALL_NEEDED" -eq 1 ] || return 0
    if [ "$INSTALL_INCUS" -ne 1 ]; then
        fail "incus is not installed; core installs no packages. Install incus and virtiofsd yourself and re-run --provision, or re-run with --install-incus (requires the plugins/install-incus add-on; Debian 12/13 only)"
    fi
    if [ ! -f "$INCUS_INSTALL_PLUGIN" ] || [ -L "$INCUS_INSTALL_PLUGIN" ]; then
        fail "--install-incus requires the install-incus plugin, which is not present in this tree: plugins/install-incus/incus-install.sh (see docs/plugins.md)"
    fi
    # Source-of-trust: the plugin runs as root apt logic and must not be
    # replaced by a writable file; the provision-tree symlink refusal
    # already covers vendored copies, and the direct source-tree dispatch
    # here refuses symlinks too.
    # shellcheck source=plugins/install-incus/incus-install.sh
    . "$INCUS_INSTALL_PLUGIN"
    install_incus
    command -v incus >/dev/null 2>&1 || fail "incus is missing after the install-incus acquisition step"
    virtiofsd_probe || fail "virtiofsd is missing after the install-incus acquisition step"
    incus_agent_probe || fail "incus-agent is missing after the install-incus acquisition step; install the distro's Incus agent package"
}

# apply_pool_resize: idiom 3 — pool growth through provision. Called when
# the pool already exists: an equal pool_size_gib is a no-op, a larger one
# grows the pool after the host-backing verdict and is proven by reading the
# size back, and a smaller one refuses (shrinking a storage pool is not
# supported; preflight already stopped for that case, this is the fail-closed
# backstop). Nothing here tells an operator to run incus storage set by hand:
# this function is the supported path that runs it for them.
apply_pool_resize() {
    pool_plan=$(pool_resize_plan "$SITE_STORAGE_POOL" "$SITE_POOL_SIZE_GIB") && pool_plan_status=0 || pool_plan_status=$?
    case "$pool_plan_status" in
        0)
            echo "install: $pool_plan"
            ;;
        2)
            pool_verdict=$(pool_grow_verdict "$SITE_STORAGE_POOL" "$SITE_POOL_SIZE_GIB" "$POOL_HEADROOM_GIB" /var/lib/incus) ||
                fail "install: $pool_verdict"
            approve "grow the storage pool ($pool_verdict)"
            incus storage set "$SITE_STORAGE_POOL" "size=${SITE_POOL_SIZE_GIB}GiB"
            confirmed_mib=$(pool_configured_size_mib "$SITE_STORAGE_POOL") ||
                fail "install: the storage pool size could not be read back"
            [ "$confirmed_mib" -eq $((SITE_POOL_SIZE_GIB * 1024)) ] ||
                fail "install: the storage pool size was not proven (read back ${confirmed_mib}MiB, want $((SITE_POOL_SIZE_GIB * 1024))MiB)"
            echo "install: $pool_verdict — grown to ${SITE_POOL_SIZE_GIB}GiB and proven by read-back"
            ;;
        3)
            fail "install: $pool_plan"
            ;;
        *)
            fail "install: storage pool resize plan failed: $pool_plan"
            ;;
    esac
}

initialize_incus() {
    # The skip gate must cover the bridge too, not just the pool: the Incus
    # network is named by bridge_name, so a resumed host whose descriptor now
    # names a foreign interface would otherwise skip init and arm the
    # firewall for an interface Incus never created. On the resumed path the
    # preseed does not run (the pool exists), so the fail-closed check is
    # the named network-existence gate at the top of initialize-incus.sh,
    # before the unit is enabled; on the fresh path the preseed also fails
    # closed on a conflicting interface name.
    if incus admin waitready --timeout=5 >/dev/null 2>&1 && \
        incus storage show "$SITE_STORAGE_POOL" >/dev/null 2>&1 && \
        incus network show "$SITE_BRIDGE_NAME" >/dev/null 2>&1; then
        echo "install: Incus is already initialized for this site"
        apply_pool_resize
    else
        approve "initialize Incus from the rendered preseed (incus admin init: storage pool, bridge, site profile)"
        "$SCRIPT_DIR/initialize-incus.sh"
        return 0
    fi
    # The payload above replaced the firewall template. Reload an active
    # unit so the kernel receives the new rules without a stop/start gap.
    state=$(systemctl is-active chasm-firewall.service 2>/dev/null || true)
    if [ "$state" = active ]; then
        if [ "$(systemctl is-enabled chasm-firewall.service 2>/dev/null || true)" != enabled ]; then
            approve "enable the active firewall unit at boot"
            systemctl enable chasm-firewall.service || fail "enabling firewall at boot failed"
        fi
        approve "reload the active firewall unit (systemctl reload chasm-firewall.service)"
        systemctl reload chasm-firewall.service ||
            fail "reloading the active firewall failed"
        return 0
    fi
    approve "enable the firewall unit (systemctl enable --now chasm-firewall.service)"
    systemctl enable --now chasm-firewall.service
}

build_template() {
    # The skip decision must be identity-aware: the template identity digests
    # the builder itself and every layer script, so a tree change makes the
    # existing sealed template stale while its name still matches the
    # profile prefix. The builder's read-only --check returns 0 only when
    # the CURRENT-identity template exists and is sealed.
    if "$INSTALL_PROVISION/scripts/build-sandbox-template.sh" --check; then
        echo "install: a sealed template for profile $SITE_BOOTSTRAP_PROFILE already exists"
        return 0
    fi
    approve "build the sealed template from the bootstrap profile '$SITE_BOOTSTRAP_PROFILE' (imports the base image and runs a bounded builder VM; admitted beside running sandboxes when host memory fits)"
    # Run from the installed provision tree, not the checkout: the profile
    # reader's ownership rule admits a manifest owned by root or the invoking
    # user only, and --provision runs as root while the checkout is typically
    # operator-owned. install_payload refreshed this tree from the checkout
    # moments ago, so the content is identical.
    "$INSTALL_PROVISION/scripts/build-sandbox-template.sh"
}

print_quickstart() {
    cat <<QUICKSTART

install: chasm is provisioned on this site.

Quickstart (as the operator user):
  chasm create demo 2048     # create a sandbox from the sealed template
  chasm start demo           # creation leaves the sandbox stopped
  chasm shell demo           # enter the sandbox (the sudo hop is built in)
  chasm show demo            # inspect the running sandbox
  chasm stop demo
  chasm delete demo

Descriptor: $INSTALLED_SITE_CONF
Marker:     $SITE_MARKER_PATH
QUICKSTART
}

verify_drift() {
    drift_report
    echo "install: drift summary: $DRIFT_COUNT drifted, $SKIP_COUNT skipped (needs root)"
    [ "$DRIFT_COUNT" -eq 0 ] || fail "drift report has $DRIFT_COUNT drifted probe(s)"
    echo "install: verify passed"
}

cmd_provision() {
    if ! am_root; then
        fail "--provision must run as root (use sudo)"
    fi
    if [ -n "${CHASM_SITE_CONF:-}" ]; then
        fail "CHASM_SITE_CONF is set; provisioning writes and pins the installed descriptor"
    fi

    echo "install: --provision will, with one approval per step:"
    echo "  1. preflight this host (read-only)"
    echo "  2. install Incus (only with --install-incus and only if absent; without the flag incus and virtiofsd must already be present)"
    echo "  3. write $INSTALLED_SITE_CONF"
    echo "  4. pin the site identity marker"
    echo "  5. build the Go binaries and install the payload (incl. control plane)"
    echo "  6. optionally harden the host ssh daemon (sshd_config drop-ins; only with --harden-ssh, off by default)"
    echo "  7. initialize Incus (incus admin init from the rendered preseed)"
    echo "  8. build the sealed template from the bootstrap profile"
    echo "  9. verify and print the quickstart"
    echo

    # Load the descriptor that preflight and the steps below will act on: the
    # installed one on a resumed run, a root-owned copy of the shipped example
    # on a fresh host. The copy sidesteps the descriptor ownership rule: a
    # checkout is typically owned by the invoking operator, but --provision
    # runs as root, and the library refuses descriptors owned by neither root
    # nor the invoking user.
    if [ -e "$INSTALLED_SITE_CONF" ]; then
        load_site_lib
        site_require_descriptor ||
            fail "the installed descriptor does not parse on this host; set the failing keys explicitly"
        echo "install: resuming with the installed descriptor at $INSTALLED_SITE_CONF"
    else
        candidate=$(mktemp)
        cleanup_candidate() { rm -f "$candidate"; }
        trap cleanup_candidate EXIT
        trap 'exit 129' HUP
        trap 'exit 130' INT
        trap 'exit 143' TERM
        install -o root -g root -m 0644 \
            "$PROVISION_ROOT/host-config/site.conf.example" "$candidate"
        load_candidate_descriptor "$candidate" ||
            fail "the shipped example descriptor does not parse on this host"
        cleanup_candidate
        trap - EXIT HUP INT TERM
    fi

    preflight
    install_incus_step
    write_descriptor
    pin_identity
    install_payload
    install_ssh_hardening_step
    initialize_incus
    build_template

    echo "install: post-install verification"
    verify_drift
    print_quickstart
}

# Argument parsing: at most one mode plus any number of options. With no
# mode at all the script runs --verify, exactly as the help states; the old
# per-branch "$# -eq 1" checks used to turn a bare invocation into a usage
# error instead.
install_mode=
HARDEN_SSH=0
INSTALL_INCUS=0
ALLOW_UNTESTED_HOST=0
INCUS_SOCKET_EXPLICIT=0
[ -n "${INCUS_SOCKET:-}" ] && INCUS_SOCKET_EXPLICIT=1
for install_argument in "$@"; do
    case "$install_argument" in
        --harden-ssh) HARDEN_SSH=1 ;;
        --install-incus) INSTALL_INCUS=1 ;;
        --allow-untested-host) ALLOW_UNTESTED_HOST=1 ;;
        *)
            [ -z "$install_mode" ] || usage
            install_mode=$install_argument
            ;;
    esac
done
: "${install_mode:=--verify}"

# Help wins over every other consideration, including flags that would
# otherwise be usage errors: '--help --harden-ssh' prints help and exits 0.
case "$install_mode" in
    -h|--help)
        show_help
        ;;
esac

# --harden-ssh and --install-incus only shape the --provision sshd and
# Incus steps; anywhere else they would be silently inert, so refuse them
# as the usage errors they are.
if [ "$HARDEN_SSH" -eq 1 ] || [ "$INSTALL_INCUS" -eq 1 ] || [ "$ALLOW_UNTESTED_HOST" -eq 1 ]; then
    [ "$install_mode" = "--provision" ] || usage
fi

case "$install_mode" in
    --check|--verify)
        # One drift implementation behind two names: the modes always ran
        # identical probes with identical exit semantics, so a single arm
        # dispatches and only the summary label differs.
        drift_report
        if [ "$install_mode" = "--check" ]; then
            echo "install: check summary: $DRIFT_COUNT drifted, $SKIP_COUNT skipped (needs root)"
            [ "$DRIFT_COUNT" -eq 0 ] || exit 1
        else
            echo "install: drift summary: $DRIFT_COUNT drifted, $SKIP_COUNT skipped (needs root)"
            [ "$DRIFT_COUNT" -eq 0 ] || fail "drift report has $DRIFT_COUNT drifted probe(s)"
            echo "install: verify passed"
        fi
        ;;
    --provision)
        cmd_provision
        ;;
    *)
        usage
        ;;
esac
