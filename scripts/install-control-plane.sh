#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=scripts/lib/require-site-host.sh
. "$SCRIPT_DIR/lib/require-site-host.sh"

fail() {
    echo "control plane install: $*" >&2
    exit 1
}

require_site_host || fail "site identity check failed"
[ "$(id -u)" -eq 0 ] || fail "run through sudo as root"

# Render the host-control units and the per-site host configuration from the
# site descriptor, then install them. Nothing is installed unrendered: a
# surviving @TOKEN@ or an invalid sudoers fragment aborts the install.
rendered=$(mktemp -d)
trap 'rm -rf "$rendered"' EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
"$SCRIPT_DIR/render-host-units.sh" "$rendered"
"$SCRIPT_DIR/render-host-config.sh" "$rendered"
for unit in \
    chasm-executor.service \
    chasm-executor.socket \
    chasm-firewall.service \
    chasm-supervisor.service \
    chasm-supervisor.socket
do
    [ -f "$rendered/$unit" ] || fail "rendered unit is missing: $unit"
    install -o root -g root -m 0644 "$rendered/$unit" "/etc/systemd/system/$unit"
done

[ -f "$rendered/chasm" ] || fail "rendered sudoers fragment is missing"
install -o root -g root -m 0440 "$rendered/chasm" /etc/sudoers.d/chasm
if command -v visudo >/dev/null 2>&1; then
    visudo -cf /etc/sudoers.d/chasm >/dev/null ||
        fail "installed sudoers fragment is not valid"
fi

# The sysusers and tmpfiles fragments are rendered from the site descriptor
# (the supervisor user is a site key, not a constant), so the rendered
# copies are what get installed and what systemd-sysusers/systemd-tmpfiles
# read.
[ -f "$rendered/sysusers.d/chasm.conf" ] ||
    fail "rendered sysusers fragment is missing"
[ -f "$rendered/tmpfiles.d/chasm.conf" ] ||
    fail "rendered tmpfiles fragment is missing"
install -o root -g root -m 0644 "$rendered/sysusers.d/chasm.conf" \
    /usr/lib/sysusers.d/chasm.conf
install -o root -g root -m 0644 "$rendered/tmpfiles.d/chasm.conf" \
    /usr/lib/tmpfiles.d/chasm.conf
systemd-sysusers /usr/lib/sysusers.d/chasm.conf
systemd-tmpfiles --create /usr/lib/tmpfiles.d/chasm.conf
systemctl daemon-reload
# A stale socket-activated service left running by an earlier deployment
# (or an interrupted reinstall) keeps its socket from ever listening;
# systemd refuses with "Socket service ... already active, refusing". Stop
# the services before arming the sockets.
systemctl stop chasm-executor.service chasm-supervisor.service 2>/dev/null || true
systemctl enable --now chasm-executor.socket chasm-supervisor.socket

supervisor_uid=$(id -u "$SITE_SUPERVISOR_USER")
[ "$supervisor_uid" -gt 0 ] || fail "supervisor UID is invalid"
[ "$(getent passwd "$SITE_SUPERVISOR_USER" | cut -d: -f7)" = /usr/sbin/nologin ] || \
    fail "supervisor login shell is not disabled"
if id -nG "$SITE_SUPERVISOR_USER" | tr ' ' '\n' | \
    grep -Eq '^(incus-admin|sudo)$'; then
    fail "supervisor belongs to a privileged administration group"
fi
[ "$(stat -c '%U:%G:%a' /var/lib/chasm/supervisor)" = \
    "$SITE_SUPERVISOR_USER:$SITE_SUPERVISOR_USER:700" ] || \
    fail "supervisor state directory metadata is invalid"
[ "$(stat -c '%U:%G:%a' /var/lib/chasm/executor)" = root:root:700 ] || \
    fail "executor state directory metadata is invalid"
[ "$(stat -c '%U:%G:%a' /run/chasm-executor.sock)" = \
    "root:$SITE_SUPERVISOR_USER:660" ] || fail "executor socket metadata is invalid"
[ "$(stat -c '%U:%G:%a' /run/chasm-supervisor.sock)" = \
    "root:$SITE_OPERATOR_USER:660" ] || fail "supervisor socket metadata is invalid"
systemctl is-active --quiet chasm-executor.socket
systemctl is-active --quiet chasm-supervisor.socket

echo "control plane install: sockets active; supervisor remains unprivileged"
