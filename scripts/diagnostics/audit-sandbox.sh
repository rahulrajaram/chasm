#!/bin/sh
# Collect a Lynis report from one explicitly named, running Chasm VM.
set -eu
umask 077
usage() { echo "usage: $0 LOGICAL-NAME" >&2; exit 64; }
[ "$#" = 1 ] || usage
case "$1" in ''|*[!a-z0-9-]*|-*|*-) usage ;; esac
name=$1
vm=sandbox-$name
script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=scripts/lib/require-site-host.sh
. "$script_dir/../lib/require-site-host.sh"
require_site_host
incus_local() { sudo incus --force-local --project default "$@"; }
identity() {
    incus_local config get "$vm" volatile.uuid
    incus_local exec "$vm" -T -- cat /proc/sys/kernel/random/boot_id
}
[ "$(incus_local list "$vm" --format csv -c nst)" = "$vm,RUNNING,VIRTUAL-MACHINE" ] &&
    [ "$(incus_local config get "$vm" "user.$SITE_RESOURCE_PREFIX.managed")" = true ] &&
    [ "$(incus_local config get "$vm" "user.$SITE_RESOURCE_PREFIX.logical-name")" = "$name" ] || {
        echo "expected a running Chasm-managed VM for $name on this site" >&2
        exit 1
    }
incus_local exec "$vm" -T -- sh -ec 'mountpoint -q /workspace && mountpoint -q /shared'
out=$(mktemp -d "./lynis-evidence.XXXXXXXX")
echo "Audit evidence directory: $out" >&2
printf '%s\n' "$vm" > "$out/instance"
git -C "$script_dir" rev-parse HEAD > "$out/harness-revision"
sha256sum "$script_dir/guest-lynis.sh" > "$out/guest-script.sha256"
identity > "$out/identity.before"
awk 'length($0) == 36 {n++} END {exit !(NR == 2 && n == 2)}' "$out/identity.before"
incus_local exec "$vm" -T -- dpkg-query -W lynis > "$out/package-version"
incus_local exec "$vm" -T -- sh -s < "$script_dir/guest-lynis.sh" \
    > "$out/report.partial" 2> "$out/console.log"
identity > "$out/identity.after"
cmp "$out/identity.before" "$out/identity.after"
mv "$out/report.partial" "$out/report.dat"
printf '%s\n' "$out/report.dat"
