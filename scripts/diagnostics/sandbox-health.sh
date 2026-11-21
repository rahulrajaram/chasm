#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=scripts/lib/require-site-host.sh
. "$SCRIPT_DIR/../lib/require-site-host.sh"

usage() {
    echo "usage: $0 <sandbox-name|sandbox-instance>" >&2
    exit 64
}

[ "$#" -eq 1 ] || usage
case "$1" in
    --help|-h) echo "usage: $0 <sandbox-name|sandbox-instance>"; exit 0 ;;
    sandbox-*) vm=$1 ;;
    *) vm=sandbox-$1 ;;
esac
case "$vm" in
    sandbox-[a-z0-9]*)
        case "$vm" in *[!a-z0-9-]*|*-) usage ;; esac
        ;;
    *) usage ;;
esac
[ "$(id -u)" -eq 0 ] || { echo "run as root with sudo" >&2; exit 1; }
require_site_host || exit 1

incus_read() { timeout 12s incus "$@"; }
pool=$SITE_STORAGE_POOL
pool_mount=/var/lib/incus/storage-pools/$pool
mountpoint -q "$pool_mount" || { echo "storage pool is not mounted: $pool" >&2; exit 1; }

info=$(incus_read info "$vm") || { echo "cannot read instance: $vm" >&2; exit 1; }
status=$(printf '%s\n' "$info" | sed -n 's/^Status: //p' | head -n 1)
[ -n "$status" ] || { echo "instance status is unavailable: $vm" >&2; exit 1; }
pool_size=$(incus_read storage get "$pool" size)
pool_source=$(incus_read storage get "$pool" source)
pool_numbers=$(df -B1 --output=size,used,avail "$pool_mount" | awk 'NR == 2 {print $1, $2, $3}')
read -r pool_total pool_used pool_free <<EOF
$pool_numbers
EOF
for capacity in "$pool_total" "$pool_used" "$pool_free"; do
    case "$capacity" in
        ''|*[!0-9]*) echo "storage capacity is unavailable: $pool" >&2; exit 1 ;;
    esac
done
host_free=$(df -B1 --output=avail /var/lib/incus | awk 'NR == 2 {print $1}')
mem_available=$(awk '$1 == "MemAvailable:" {printf "%.0f", $2 * 1024}' /proc/meminfo)

printf 'vm=%s\nincus_status=%s\n' "$vm" "$status"
printf 'pool=%s\npool_source=%s\npool_config_size=%s\n' "$pool" "$pool_source" "$pool_size"
printf 'site_declared_pool_size=%sGiB\n' "$SITE_POOL_SIZE_GIB"
if [ "$pool_size" = "${SITE_POOL_SIZE_GIB}GiB" ]; then
    printf 'site_pool_size_matches=yes\n'
else
    printf 'site_pool_size_matches=no\n'
fi
printf 'pool_total_bytes=%s\npool_used_bytes=%s\npool_free_bytes=%s\n' \
    "$pool_total" "$pool_used" "$pool_free"
printf 'host_free_bytes=%s\nhost_mem_available_bytes=%s\n' "$host_free" "$mem_available"

qmp_log=/var/log/incus/$vm/qemu.qmp.log
qmp_status=unknown
if [ -f "$qmp_log" ] && [ ! -L "$qmp_log" ]; then
    latest_status=$(sed -n 's/.*"status": "\([^"]*\)".*/\1/p' "$qmp_log" | tail -n 1)
    [ -z "$latest_status" ] || qmp_status=$latest_status
fi
printf 'qemu_last_logged_status=%s\n' "$qmp_status"

qemu_pid_file=/run/incus/$vm/qemu.pid
qemu_process_alive=unknown
if [ -f "$qemu_pid_file" ] && [ ! -L "$qemu_pid_file" ]; then
    qemu_pid=$(cat "$qemu_pid_file")
    case "$qemu_pid" in
        *[!0-9]*|'') qemu_process_alive=unknown ;;
        *)
            qemu_comm=$(cat "/proc/$qemu_pid/comm" 2>/dev/null || true)
            if kill -0 "$qemu_pid" 2>/dev/null &&
                [ "$qemu_comm" = qemu-system-x86 ]; then
                qemu_process_alive=yes
            else
                qemu_process_alive=no
            fi
            ;;
    esac
fi
printf 'qemu_process_alive=%s\n' "$qemu_process_alive"

if instance_snapshots=$(incus_read snapshot list "$vm" --format csv 2>/dev/null); then
    instance_snapshot_count=$(printf '%s\n' "$instance_snapshots" | awk 'NF {n++} END {print n+0}')
else
    instance_snapshot_count=unknown
fi
printf 'instance_snapshot_count=%s\n' "$instance_snapshot_count"

root_image=$pool_mount/virtual-machines/$vm/root.img
if [ -f "$root_image" ] && [ ! -L "$root_image" ]; then
    printf 'root_image_present=yes\nroot_image_apparent_bytes=%s\nroot_image_allocated_bytes=%s\n' \
        "$(stat -c %s "$root_image")" "$(stat -c %b "$root_image" | awk '{print $1 * 512}')"
else
    printf 'root_image_present=no\n'
fi

workspace=$(incus_read config device get "$vm" workspace source 2>/dev/null || true)
case "$workspace" in
    *[!a-z0-9-]*|'') printf 'workspace_volume=unknown\n' ;;
    *)
        printf 'workspace_volume=%s\n' "$workspace"
        if incus_read storage volume show "$pool" "$workspace" >/dev/null 2>&1; then
            printf 'workspace_volume_present=yes\n'
        else
            printf 'workspace_volume_present=no\n'
        fi
        if workspace_snapshots=$(incus_read storage volume snapshot list "$pool" "$workspace" --format csv 2>/dev/null); then
            printf 'workspace_snapshot_count=%s\n' \
                "$(printf '%s\n' "$workspace_snapshots" | awk 'NF {n++} END {print n+0}')"
        else
            printf 'workspace_snapshot_count=unknown\n'
        fi
        workspace_path=$pool_mount/custom/default_$workspace
        if [ -d "$workspace_path" ] && [ ! -L "$workspace_path" ]; then
            workspace_bytes=$(timeout 20s du -s -B1 "$workspace_path" 2>/dev/null | awk '{print $1}' || true)
            printf 'workspace_du_bytes=%s\n' "${workspace_bytes:-unknown}"
        fi
        ;;
esac

shared=$(incus_read config device get "$vm" shared source 2>/dev/null || true)
case "$shared" in
    *[!a-z0-9-]*|'') printf 'shared_volume=unknown\n' ;;
    *)
        printf 'shared_volume=%s\n' "$shared"
        if incus_read storage volume show "$pool" "$shared" >/dev/null 2>&1; then
            printf 'shared_volume_present=yes\n'
        else
            printf 'shared_volume_present=no\n'
        fi
        ;;
esac

if [ "$status" = RUNNING ]; then
    if guest_mounts=$(timeout 8s incus exec "$vm" -- sh -c '
        mountpoint -q /workspace && mountpoint -q /shared &&
        test -r /workspace && printf "workspace_and_shared_mounted"
    ' 2>/dev/null); then
        printf 'guest_probe=%s\n' "$guest_mounts"
    else
        printf 'guest_probe=failed_or_timed_out\n'
    fi
else
    printf 'guest_probe=skipped_instance_not_running\n'
fi

if [ "$status" = ERROR ] && [ "$pool_free" -eq 0 ] && [ "$qmp_status" = io-error ]; then
    printf 'diagnosis=pool_exhaustion_paused_vm\n'
elif [ "$pool_free" -eq 0 ]; then
    printf 'diagnosis=pool_exhausted\n'
elif [ "$status" = RUNNING ]; then
    printf 'diagnosis=running_with_pool_space\n'
else
    printf 'diagnosis=inspect_qemu_and_instance_logs\n'
fi

# This command observes only. It never grows storage, resumes, restarts, or
# snapshots a VM; those actions need an operator decision after inspection.
